// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	gcrtarball "github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/nofireai/urunc-macos/pkg/store"
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
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	ref, err := name.ParseReference(u.Host+"/urunc/test:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, testLayer(t, content))
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
