// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ociclient

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	gcrtarball "github.com/google/go-containerregistry/pkg/v1/tarball"
)

const testFileName = "hello.txt"

// testLayer builds a one-file layer by hand. random.Image is not usable here:
// its tar headers carry mode 0, so extraction cannot open the file for writing.
func testLayer(t *testing.T, content string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     testFileName,
		Mode:     0644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatalf("write content: %v", err)
	}
	for _, c := range []io.Closer{tw, zw} {
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	data := buf.Bytes()
	layer, err := gcrtarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}
	return layer
}

// testRegistry serves an image from an in-memory registry on 127.0.0.1 and
// returns its reference.
func testRegistry(t *testing.T, content string) string {
	t.Helper()
	return testRegistryWithBlobHook(t, content, nil)
}

// testRegistryWithBlobHook is testRegistry with onBlob called before the
// layer blob a pull fetches, once the image is in place: a way to act in the
// middle of a pull, between its decision to download and its commit. The
// layer only, not the config blob: the pull reads the config before it
// decides anything, so a hook on that fires before the decision.
func testRegistryWithBlobHook(t *testing.T, content string, onBlob func()) string {
	t.Helper()
	layer := testLayer(t, content)
	layerDigest, err := layer.Digest()
	if err != nil {
		t.Fatalf("layer digest: %v", err)
	}
	backend := registry.New()
	var serving atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onBlob != nil && serving.Load() && r.Method == http.MethodGet &&
			strings.HasSuffix(r.URL.Path, "/blobs/"+layerDigest.String()) {
			onBlob()
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	defer serving.Store(true)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	ref, err := name.ParseReference(u.Host+"/urunc/test:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	return ref.String()
}

func newClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return New(s), s
}

func TestPullUnpacksAndPublishesTogether(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	res, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !s.ImageComplete(res.Digest) {
		t.Error("a successful pull must leave a complete image")
	}
	// No staging directories survive a clean pull.
	entries, err := os.ReadDir(filepath.Join(s.RootDir(), "images"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != res.Digest {
			t.Errorf("unexpected leftover in images/: %s", e.Name())
		}
	}
}

// Re-pulling a digest we already hold must not re-unpack it. That is what makes
// --pull=always affordable: it should cost a manifest lookup, not a full
// re-download. The sentinel proves it -- a re-unpack swaps the whole directory,
// which would take the file with it.
func TestPullSkipsUnchangedDigest(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}

	sentinel := filepath.Join(s.RootDir(), "images", first.Digest, "rootfs", ".sentinel")
	if err := os.WriteFile(sentinel, []byte("survived"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if second.Digest != first.Digest {
		t.Fatalf("digest changed: %s -> %s", first.Digest, second.Digest)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("image was re-unpacked; the unchanged-digest short circuit did not fire")
	}
	if !s.ImageComplete(second.Digest) {
		t.Error("image should still be complete")
	}
}

// An image whose rootfs was lost must be re-fetched rather than short-circuited
// on the strength of its metadata alone.
func TestPullRepairsIncompleteImage(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	rootfs := filepath.Join(s.RootDir(), "images", first.Digest, "rootfs")
	if err := os.RemoveAll(rootfs); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if s.ImageComplete(first.Digest) {
		t.Fatal("precondition: image should be incomplete")
	}

	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if !s.ImageComplete(first.Digest) {
		t.Error("pull should have restored the missing rootfs")
	}
}

// The unpack tests reach extractTarIgnoreChown directly. This one comes in the
// way an attacker would: a hostile layer served from a registry, pulled through
// the public Client.Pull. It is here because the escape used to survive the
// whole path -- the pull returned nil, reported success, and had already
// written a file the image was never allowed to name.
func TestPullRefusesLayerEscapingTheRootfs(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "pwned")

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range []tar.Header{
		{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
		{Name: "escape/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
	} {
		hdr := h
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if hdr.Size > 0 {
			if _, err := io.WriteString(tw, "owned"); err != nil {
				t.Fatalf("write content: %v", err)
			}
		}
	}
	for _, c := range []io.Closer{tw, zw} {
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	data := buf.Bytes()
	layer, err := gcrtarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	ref, err := name.ParseReference(u.Host+"/urunc/hostile:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	c, _ := newClient(t)
	if _, err := c.Pull(context.Background(), ref.String()); err == nil {
		t.Error("Pull reported success on an image that writes outside its rootfs")
	}
	if _, serr := os.Lstat(victim); !os.IsNotExist(serr) {
		t.Errorf("%s was created outside the rootfs: %v", victim, serr)
	}
}

func TestImageExistsRequiresRootfs(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	res, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !c.ImageExists(res.Digest) {
		t.Fatal("freshly pulled image should exist")
	}

	if err := os.RemoveAll(filepath.Join(s.RootDir(), "images", res.Digest, "rootfs")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if c.ImageExists(res.Digest) {
		t.Error("an image with no rootfs must not report as existing")
	}
}

// A pull can move hundreds of megabytes, so cancelling it has to stop it.
// PullPlatform took a context and never handed it to crane, which defaults to
// context.Background(), so Ctrl-C left the transfer running.
func TestPullHonorsContextCancellation(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, _ := newClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.PullPlatform(ctx, ref, DefaultPlatform); err == nil {
		t.Fatal("a cancelled context must stop the pull")
	}
}

// Two pulls into one store at the same time (#114). The sweep a pull runs
// before unpacking must leave the other pull's staging directories alone: the
// `.tmp-<pid>` it is unpacking into, and the `.old-<pid>` it is holding the
// previous image in until its commit is through. Removing the first truncates
// the other pull's image; removing the second leaves its rollback nothing to
// restore. The other pull holds a lock on each, which is what the sweep
// reads; the leftovers of a pull that is gone, whose locks died with it, are
// still swept.
func TestPullLeavesAConcurrentPullsStagingAlone(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	const other = "sha256:c408baae42f5c74c0661fbc20a289fd23d4322988e52c88cd54108e5c4c74893"
	plant := func(name string, held bool) string {
		dir := filepath.Join(s.RootDir(), "images", name)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs", "usr"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if held {
			f, err := os.Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("Flock: %v", err)
			}
			t.Cleanup(func() { _ = f.Close() })
		}
		return dir
	}
	inFlight := plant(other+".tmp-1", true)
	rollback := plant(other+".old-1", true)
	gone := plant(other+".tmp-2", false)

	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	for _, dir := range []string{inFlight, rollback} {
		if _, err := os.Stat(filepath.Join(dir, "rootfs", "usr")); err != nil {
			t.Errorf("the pull swept a concurrent pull's %s: %v", filepath.Base(dir), err)
		}
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the leftover of a dead pull survived: %v", err)
	}
}

// A legacy `.old-<pid>` whose best-effort delete did not finish is still
// swept before a new pull unpacks, even when its pid is this process's own.
// Its directory lock, not the pid in its name, decides whether it is busy.
func TestPullCommitsOverItsOwnOldLeftover(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	// What a commit whose RemoveAll did not finish leaves behind, with the
	// image incomplete so the second pull has to commit again.
	leftover := filepath.Join(s.RootDir(), "images", first.Digest+".old-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(filepath.Join(leftover, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "rootfs", "stale"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(filepath.Join(s.RootDir(), "images", first.Digest, "unpack-schema")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("second Pull over an own .old- leftover: %v", err)
	}
	if !s.ImageComplete(first.Digest) {
		t.Error("the second pull did not publish a complete image")
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the own .old- leftover survived the commit: %v", err)
	}
}

// A store left with a truncated image by the race in #114 looks complete, and
// before the count was recorded nothing but `hull rmi` got it out of that
// state. Pulling the same reference again must notice the missing entry and
// unpack the image afresh.
func TestPullRepairsATruncatedImage(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	lost := filepath.Join(s.RootDir(), "images", first.Digest, "rootfs", testFileName)
	if err := os.Remove(lost); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !s.ImageComplete(first.Digest) {
		t.Fatal("precondition: the truncated image should still look complete")
	}

	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if _, err := os.Stat(lost); err != nil {
		t.Errorf("the second pull reused the truncated image: %v", err)
	}
	if err := s.VerifyImage(first.Digest); err != nil {
		t.Errorf("the repaired image fails verification: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.RootDir(), "images"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != first.Digest {
			t.Errorf("the repair left %s behind", e.Name())
		}
	}
}

// A damaged image an instance refers to is not displaced: a running guest has
// that rootfs shared in, and a stopped instance expects it back on start. The
// pull warns and serves what is there, as rmi and prune refuse the same way.
func TestPullKeepsADamagedImageAnInstanceHolds(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	lost := filepath.Join(s.RootDir(), "images", first.Digest, "rootfs", testFileName)
	if err := os.Remove(lost); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.CreateInstance("holder"); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := s.SaveInstance(&store.InstanceState{ID: "holder", ImageDigest: first.Digest, Status: "running"}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	sentinel := filepath.Join(s.RootDir(), "images", first.Digest, ".sentinel")
	if err := os.WriteFile(sentinel, []byte("held"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("the pull displaced an image an instance refers to")
	}
	if _, err := os.Stat(lost); err == nil {
		t.Error("the pull re-unpacked an image an instance refers to")
	}
}

// The check before the download is not the last word: the download takes
// minutes on a real image, and an instance can start on the cached image in
// that time, with its rootfs shared straight into the guest. The commit has
// to look again, and keep the image the instance has.
func TestPullKeepsAnImageAnInstanceStartedOnDuringThePull(t *testing.T) {
	c, s := newClient(t)
	// The hook runs on the server's goroutine; the digest it needs is only
	// known after the first pull, so it is handed over atomically.
	var digest atomic.Pointer[string]
	var started atomic.Bool
	ref := testRegistryWithBlobHook(t, "hello from the test layer", func() {
		d := digest.Load()
		if d == nil || started.Swap(true) {
			return
		}
		if _, err := s.CreateInstance("late"); err != nil {
			t.Errorf("CreateInstance: %v", err)
		}
		if err := s.SaveInstance(&store.InstanceState{ID: "late", ImageDigest: *d, Status: "running"}); err != nil {
			t.Errorf("SaveInstance: %v", err)
		}
	})

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	digest.Store(&first.Digest)
	lost := filepath.Join(s.RootDir(), "images", first.Digest, "rootfs", testFileName)
	if err := os.Remove(lost); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	sentinel := filepath.Join(s.RootDir(), "images", first.Digest, ".sentinel")
	if err := os.WriteFile(sentinel, []byte("held"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before := time.Now()
	if _, err := c.Pull(context.Background(), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if !started.Load() {
		t.Fatal("precondition: the instance was never started during the pull")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("the pull displaced an image an instance started on during the download")
	}
	// Kept as is, but still resolved: the record says this pull happened,
	// as it does when the early skip keeps an image.
	img, err := s.GetImage(first.Digest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if img.PulledAt.Before(before) {
		t.Errorf("the kept image's record was not refreshed: pulled at %v, pull started %v", img.PulledAt, before)
	}
	staging, err := s.StagingDirs()
	if err != nil {
		t.Fatalf("StagingDirs: %v", err)
	}
	if len(staging) != 0 {
		t.Errorf("the discarded pull left %v behind", staging)
	}
}

// The cache can call an image incomplete while a guest is running on it: a
// hull with a bumped unpack schema reads the old rootfs as a miss, and the
// instance booted before the upgrade has that very directory shared in.
// The pull must not displace it; a stopped instance on an incomplete image
// is the opposite case, waiting for exactly this pull.
func TestPullKeepsAnIncompleteImageALiveInstanceHolds(t *testing.T) {
	for _, tc := range []struct {
		status string
		kept   bool
	}{
		{"running", true},
		{"stopped", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			ref := testRegistry(t, "hello from the test layer")
			c, s := newClient(t)
			first, err := c.Pull(context.Background(), ref)
			if err != nil {
				t.Fatalf("first Pull: %v", err)
			}
			imageDir := filepath.Join(s.RootDir(), "images", first.Digest)
			// What a schema bump looks like from the store's side.
			if err := os.WriteFile(filepath.Join(imageDir, "unpack-schema"), []byte("1\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if s.ImageComplete(first.Digest) {
				t.Fatal("precondition: the image should read as incomplete")
			}
			if _, err := s.CreateInstance("holder"); err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}
			if err := s.SaveInstance(&store.InstanceState{ID: "holder", ImageDigest: first.Digest, Status: tc.status}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			sentinel := filepath.Join(imageDir, ".sentinel")
			if err := os.WriteFile(sentinel, []byte("held"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			if _, err := c.Pull(context.Background(), ref); err != nil {
				t.Fatalf("second Pull: %v", err)
			}
			_, err = os.Stat(sentinel)
			if tc.kept && err != nil {
				t.Error("the pull displaced an incomplete image a live instance is running on")
			}
			if !tc.kept && err == nil {
				t.Error("the pull kept an incomplete image only a stopped instance refers to")
			}
		})
	}
}

// The store cannot tell a VMM from a recycled pid, so hull wires in the
// check rmi uses. A record left at "running" by a death must not pin an
// incomplete image forever: with the caller's check saying it is dead, the
// pull replaces the image; without one, the status alone keeps it.
func TestPullAsksTheCallerWhetherAHolderIsLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		live func(*store.InstanceState) bool
		kept bool
	}{
		{"caller says dead", func(*store.InstanceState) bool { return false }, false},
		{"caller says live", func(*store.InstanceState) bool { return true }, true},
		{"no caller check", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := testRegistry(t, "hello from the test layer")
			c, s := newClient(t)
			c.InstanceLive = tc.live
			first, err := c.Pull(context.Background(), ref)
			if err != nil {
				t.Fatalf("first Pull: %v", err)
			}
			imageDir := filepath.Join(s.RootDir(), "images", first.Digest)
			if err := os.WriteFile(filepath.Join(imageDir, "unpack-schema"), []byte("1\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := s.CreateInstance("stale"); err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}
			if err := s.SaveInstance(&store.InstanceState{ID: "stale", ImageDigest: first.Digest, Status: "running", PID: 999999}); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			sentinel := filepath.Join(imageDir, ".sentinel")
			if err := os.WriteFile(sentinel, []byte("held"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := c.Pull(context.Background(), ref); err != nil {
				t.Fatalf("second Pull: %v", err)
			}
			_, err = os.Stat(sentinel)
			if tc.kept && err != nil {
				t.Error("the pull displaced an image a holder the caller calls live is on")
			}
			if !tc.kept && err == nil {
				t.Error("the pull kept an incomplete image for a record the caller calls dead")
			}
		})
	}
}
