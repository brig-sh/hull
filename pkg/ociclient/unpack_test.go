// Copyright (c) 2023-2026, Nubificus LTD
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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// tarball builds an in-memory tar stream from (name, content) pairs.
// A nil content marks a directory.
func tarball(t *testing.T, entries [][2]string) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, e := range entries {
		name, content := e[0], e[1]
		if content == "DIR" {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestExtractTarAppliesWhiteouts(t *testing.T) {
	rootfs := t.TempDir()

	// Layer 1: /home/ubuntu/.profile and /etc/{keep,gone}
	l1 := tarball(t, [][2]string{
		{"home/", "DIR"},
		{"home/ubuntu/", "DIR"},
		{"home/ubuntu/.profile", "profile"},
		{"etc/", "DIR"},
		{"etc/keep", "keep"},
		{"etc/gone", "gone"},
	})
	if err := extractTarIgnoreChown(l1, rootfs); err != nil {
		t.Fatal(err)
	}

	// Layer 2: delete /home/ubuntu and /etc/gone, add /etc/new
	l2 := tarball(t, [][2]string{
		{"home/.wh.ubuntu", ""},
		{"etc/.wh.gone", ""},
		{"etc/new", "new"},
	})
	if err := extractTarIgnoreChown(l2, rootfs); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"home/ubuntu", "etc/gone", "home/.wh.ubuntu", "etc/.wh.gone"} {
		if _, err := os.Stat(filepath.Join(rootfs, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed (whiteout), stat err=%v", gone, err)
		}
	}
	for _, kept := range []string{"etc/keep", "etc/new"} {
		if _, err := os.Stat(filepath.Join(rootfs, kept)); err != nil {
			t.Errorf("%s should exist: %v", kept, err)
		}
	}
}

func TestExtractTarAppliesOpaqueWhiteouts(t *testing.T) {
	rootfs := t.TempDir()

	l1 := tarball(t, [][2]string{
		{"data/", "DIR"},
		{"data/old1", "a"},
		{"data/old2", "b"},
	})
	if err := extractTarIgnoreChown(l1, rootfs); err != nil {
		t.Fatal(err)
	}

	// Layer 2: opaque whiteout empties /data, then repopulates it.
	l2 := tarball(t, [][2]string{
		{"data/.wh..wh..opq", ""},
		{"data/fresh", "c"},
	})
	if err := extractTarIgnoreChown(l2, rootfs); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"data/old1", "data/old2", "data/.wh..wh..opq"} {
		if _, err := os.Stat(filepath.Join(rootfs, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed (opaque whiteout), stat err=%v", gone, err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(rootfs, "data/fresh")); err != nil || string(content) != "c" {
		t.Errorf("data/fresh should exist with content c: %v %q", err, content)
	}
}

func TestExtractTarHardLinks(t *testing.T) {
	rootfs := t.TempDir()

	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0755, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("bins")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool", Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractTarIgnoreChown(buf, rootfs); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(rootfs, "bin/alias"))
	if err != nil || string(content) != "bins" {
		t.Errorf("hard link should materialize with content: %v %q", err, content)
	}
}

// modeTarball builds a one-file tar with an explicit mode; the shared tarball
// helper above does not carry modes, and these tests are about modes.
func modeTarball(t *testing.T, name string, mode int64, body string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return &buf
}

// A later layer replacing a file an earlier layer wrote read-only must
// succeed. macOS enforces mode bits against the owner, so opening the existing
// file for write fails with EACCES and used to abort the whole pull.
func TestExtractTarReplacesReadOnlyFile(t *testing.T) {
	dir := t.TempDir()

	first := modeTarball(t, "usr/lib/hwdb.bin", 0o444, "old")
	if err := extractTarIgnoreChown(first, dir); err != nil {
		t.Fatalf("first layer: %v", err)
	}

	second := modeTarball(t, "usr/lib/hwdb.bin", 0o444, "new")
	if err := extractTarIgnoreChown(second, dir); err != nil {
		t.Fatalf("second layer over a read-only file: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "usr/lib/hwdb.bin"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

// The same, with the containing directory read-only: removal depends on the
// parent, so it has to be relaxed before the replace can happen.
func TestExtractTarReplacesFileInReadOnlyDir(t *testing.T) {
	dir := t.TempDir()

	first := modeTarball(t, "ro/file", 0o444, "old")
	if err := extractTarIgnoreChown(first, dir); err != nil {
		t.Fatalf("first layer: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "ro"), 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "ro"), 0o755) })

	second := modeTarball(t, "ro/file", 0o444, "new")
	if err := extractTarIgnoreChown(second, dir); err != nil {
		t.Fatalf("second layer into a read-only dir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "ro/file"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

// flakyLayer serves a real tarball but breaks the stream part-way through the
// first n reads, the way a dropped HTTP/2 stream does mid-layer.
type flakyLayer struct {
	data     []byte
	failures int
	attempts int
}

func (f *flakyLayer) Uncompressed() (io.ReadCloser, error) {
	f.attempts++
	if f.attempts <= f.failures {
		half := len(f.data) / 2
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(f.data[:half]),
			&errReader{err: errors.New("stream error: stream ID 17; PROTOCOL_ERROR; received from peer")},
		)), nil
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (f *flakyLayer) Compressed() (io.ReadCloser, error) { return f.Uncompressed() }
func (f *flakyLayer) Size() (int64, error)               { return int64(len(f.data)), nil }
func (f *flakyLayer) Digest() (v1.Hash, error)           { return v1.Hash{}, nil }
func (f *flakyLayer) DiffID() (v1.Hash, error)           { return v1.Hash{}, nil }
func (f *flakyLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

// A layer whose stream drops must be re-fetched rather than losing the pull.
func TestUnpackLayerRetriesBrokenStream(t *testing.T) {
	dir := t.TempDir()
	body := modeTarball(t, "usr/bin/tool", 0o755, "payload that is long enough to be split")
	layer := &flakyLayer{data: body.Bytes(), failures: 2}

	if err := unpackLayerWithRetry(layer, dir, 0, nil); err != nil {
		t.Fatalf("expected the retry to recover, got: %v", err)
	}
	// unpackLayer may open the layer twice per attempt (archive.Apply, then
	// the manual extractor), so assert a lower bound rather than an exact
	// count: what matters is that it retried and recovered.
	if layer.attempts < 3 {
		t.Errorf("attempts = %d, want at least 3 (two broken streams then a good one)", layer.attempts)
	}
	got, err := os.ReadFile(filepath.Join(dir, "usr/bin/tool"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "payload that is long enough to be split" {
		t.Errorf("content = %q", got)
	}
}

// A stream that never recovers must fail, not spin forever.
func TestUnpackLayerGivesUpEventually(t *testing.T) {
	dir := t.TempDir()
	body := modeTarball(t, "f", 0o644, "x")
	layer := &flakyLayer{data: body.Bytes(), failures: 99}

	if err := unpackLayerWithRetry(layer, dir, 0, nil); err == nil {
		t.Fatal("expected failure once attempts are exhausted")
	}
	// Up to two opens per attempt; the cap is on attempts, not opens.
	if layer.attempts > layerAttempts*2 {
		t.Errorf("attempts = %d, must not exceed %d", layer.attempts, layerAttempts*2)
	}
}

// A malformed archive is indistinguishable from a truncated download here:
// both surface as io.ErrUnexpectedEOF. It is retried and then reported, which
// costs a wasted fetch but never loses a recoverable pull. What matters is
// that it terminates with the real error rather than spinning.
func TestUnpackLayerReportsMalformedArchive(t *testing.T) {
	dir := t.TempDir()
	layer := &flakyLayer{data: []byte("this is not a tar archive at all")}

	err := unpackLayerWithRetry(layer, dir, 0, nil)
	if err == nil {
		t.Fatal("expected a failure on a malformed archive")
	}
	if layer.attempts > layerAttempts*2 {
		t.Errorf("attempts = %d, must not exceed %d", layer.attempts, layerAttempts*2)
	}
}

// Everything below is about one bug: the extractor used to decide containment
// by joining the entry name onto the rootfs and checking the result with
// filepath.Rel, then hand the joined path to os.Create, os.MkdirAll and
// os.RemoveAll -- all of which follow symlinks. A name is not a path, and the
// check could not see the symlinks the archive had just planted, so "escape"
// (a symlink to somewhere on the host) followed by "escape/pwned" was enough
// to create, truncate, delete and recursively wipe host files as whoever ran
// hull, with the pull still returning nil.
//
// Each test below plants the escape at a path under its own t.TempDir() and
// asserts both halves of the fix: the host path is untouched, and the
// extraction says so rather than skipping quietly.

// tarEntry is one raw tar header plus its body, which the tarball helper above
// cannot express: these tests are about the header fields -- Typeflag,
// Linkname -- that a well-formed layer never abuses.
type tarEntry struct {
	hdr  tar.Header
	body string
}

func rawTarball(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader(%s): %v", h.Name, err)
		}
		if e.body != "" {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatalf("Write(%s): %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return &buf
}

// escapeSetup makes a rootfs and, beside it, an "outside" directory standing
// in for the host. Both live under the test's own temporary directory, so a
// regression that gets through damages nothing but the test.
func escapeSetup(t *testing.T) (rootfs, outside string) {
	t.Helper()
	base := t.TempDir()
	rootfs = filepath.Join(base, "rootfs")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{rootfs, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	return rootfs, outside
}

// symlinkEscapes returns the two shapes an escaping symlink takes: an absolute
// target, and a relative one that climbs out with "..". Both have to be
// refused, and only the first is obvious.
func symlinkEscapes(outside string) map[string]string {
	return map[string]string{
		"absolute": outside,
		"relative": filepath.Join("..", filepath.Base(outside)),
	}
}

// (a) The minimal payload: a symlink out of the rootfs, then a regular file
// written through it. This one creates a host file that was not there before.
func TestExtractTarRefusesSymlinkEscapeWrite(t *testing.T) {
	for _, kind := range []string{"absolute", "relative"} {
		t.Run(kind, func(t *testing.T) {
			rootfs, outside := escapeSetup(t)
			victim := filepath.Join(outside, "pwned")

			layer := rawTarball(t,
				tarEntry{hdr: tar.Header{
					Name:     "escape",
					Typeflag: tar.TypeSymlink,
					Linkname: symlinkEscapes(outside)[kind],
					Mode:     0o777,
				}},
				tarEntry{hdr: tar.Header{
					Name:     "escape/pwned",
					Typeflag: tar.TypeReg,
					Mode:     0o644,
				}, body: "owned"},
			)

			err := extractTarIgnoreChown(layer, rootfs)
			if err == nil {
				t.Error("extraction reported success on a layer that writes outside the rootfs")
			} else if !strings.Contains(err.Error(), "escape/pwned") {
				t.Errorf("error should name the refused entry, got: %v", err)
			}
			if _, serr := os.Lstat(victim); !os.IsNotExist(serr) {
				t.Errorf("%s was created outside the rootfs: %v", victim, serr)
			}
		})
	}
}

// (b) The same route, over a host file that already exists. os.Create
// truncates, so this destroys content rather than adding a file.
func TestExtractTarRefusesOverwriteOutsideRootfs(t *testing.T) {
	for _, kind := range []string{"absolute", "relative"} {
		t.Run(kind, func(t *testing.T) {
			rootfs, outside := escapeSetup(t)
			victim := filepath.Join(outside, "secret")
			if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			layer := rawTarball(t,
				tarEntry{hdr: tar.Header{
					Name:     "escape",
					Typeflag: tar.TypeSymlink,
					Linkname: symlinkEscapes(outside)[kind],
					Mode:     0o777,
				}},
				tarEntry{hdr: tar.Header{
					Name:     "escape/secret",
					Typeflag: tar.TypeReg,
					Mode:     0o644,
				}, body: "clobbered"},
			)

			if err := extractTarIgnoreChown(layer, rootfs); err == nil {
				t.Error("extraction reported success on a layer that overwrites outside the rootfs")
			}
			got, rerr := os.ReadFile(victim)
			if rerr != nil {
				t.Fatalf("ReadFile: %v", rerr)
			}
			if string(got) != "original" {
				t.Errorf("host file was rewritten: %q", got)
			}
		})
	}
}

// (c) A whiteout is a delete, and it went through os.RemoveAll on the same
// unresolved path, so a symlink turned it into a delete of a host file.
func TestExtractTarRefusesWhiteoutThroughSymlink(t *testing.T) {
	for _, kind := range []string{"absolute", "relative"} {
		t.Run(kind, func(t *testing.T) {
			rootfs, outside := escapeSetup(t)
			victim := filepath.Join(outside, "keepme")
			if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			layer := rawTarball(t,
				tarEntry{hdr: tar.Header{
					Name:     "escape",
					Typeflag: tar.TypeSymlink,
					Linkname: symlinkEscapes(outside)[kind],
					Mode:     0o777,
				}},
				tarEntry{hdr: tar.Header{Name: "escape/.wh.keepme", Typeflag: tar.TypeReg, Mode: 0o644}},
			)

			if err := extractTarIgnoreChown(layer, rootfs); err == nil {
				t.Error("extraction reported success on a whiteout aimed outside the rootfs")
			}
			if _, serr := os.Stat(victim); serr != nil {
				t.Errorf("%s was deleted from outside the rootfs: %v", victim, serr)
			}
		})
	}
}

// (d) The worst of the four: an opaque whiteout empties the directory it sits
// in, so pointing it out of the rootfs recursively wipes a host directory.
func TestExtractTarRefusesOpaqueWhiteoutOutsideRootfs(t *testing.T) {
	for _, kind := range []string{"absolute", "relative"} {
		t.Run(kind, func(t *testing.T) {
			rootfs, outside := escapeSetup(t)
			for _, n := range []string{"one", "two"} {
				if err := os.WriteFile(filepath.Join(outside, n), []byte(n), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			if err := os.MkdirAll(filepath.Join(outside, "sub", "deep"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			layer := rawTarball(t,
				tarEntry{hdr: tar.Header{
					Name:     "escape",
					Typeflag: tar.TypeSymlink,
					Linkname: symlinkEscapes(outside)[kind],
					Mode:     0o777,
				}},
				tarEntry{hdr: tar.Header{Name: "escape/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}},
			)

			if err := extractTarIgnoreChown(layer, rootfs); err == nil {
				t.Error("extraction reported success on an opaque whiteout aimed outside the rootfs")
			}
			for _, n := range []string{"one", "two", "sub/deep"} {
				if _, serr := os.Stat(filepath.Join(outside, n)); serr != nil {
					t.Errorf("outside/%s was wiped: %v", n, serr)
				}
			}
		})
	}
}

// (e) A hard link dereferences immediately, so its Linkname never had any
// containment check at all: naming a host file here handed the guest that
// file's inode, with no symlink left behind to give it away.
func TestExtractTarRefusesHardLinkOutsideRootfs(t *testing.T) {
	rootfs, outside := escapeSetup(t)
	victim := filepath.Join(outside, "secret")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for name, linkname := range map[string]string{
		"traversing": filepath.Join("..", filepath.Base(outside), "secret"),
		"absolute":   victim,
	} {
		t.Run(name, func(t *testing.T) {
			layer := rawTarball(t, tarEntry{hdr: tar.Header{
				Name:     "loot",
				Typeflag: tar.TypeLink,
				Linkname: linkname,
				Mode:     0o644,
			}})

			err := extractTarIgnoreChown(layer, rootfs)
			if err == nil {
				t.Error("extraction reported success on a hard link out of the rootfs")
			} else if !strings.Contains(err.Error(), "loot") {
				t.Errorf("error should name the refused entry, got: %v", err)
			}
			// Neither the link nor the copy fallback may leave the content
			// inside the rootfs.
			if _, serr := os.Lstat(filepath.Join(rootfs, "loot")); !os.IsNotExist(serr) {
				t.Errorf("host content was materialized inside the rootfs: %v", serr)
			}
		})
	}
}

// archive.Apply runs before the manual pass and commonly fails part-way with
// "operation not permitted", which means the malicious symlink can already be
// on disk before the manual extractor reads its first header. The containment
// has to hold against a link this pass never saw created.
func TestExtractTarRefusesEscapeThroughPreplantedSymlink(t *testing.T) {
	rootfs, outside := escapeSetup(t)
	victim := filepath.Join(outside, "pwned")
	if err := os.Symlink(outside, filepath.Join(rootfs, "escape")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	layer := rawTarball(t, tarEntry{hdr: tar.Header{
		Name:     "escape/pwned",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
	}, body: "owned"})

	if err := extractTarIgnoreChown(layer, rootfs); err == nil {
		t.Error("extraction reported success through a symlink planted by an earlier pass")
	}
	if _, serr := os.Lstat(victim); !os.IsNotExist(serr) {
		t.Errorf("%s was created outside the rootfs: %v", victim, serr)
	}
}

// The lexical gate still has to reject the crude cases on its own.
func TestExtractTarRefusesTraversingEntryNames(t *testing.T) {
	for _, name := range []string{"../pwned", "foo/../../pwned", "/etc/pwned"} {
		t.Run(name, func(t *testing.T) {
			rootfs, _ := escapeSetup(t)
			layer := rawTarball(t, tarEntry{
				hdr:  tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644},
				body: "owned",
			})
			if err := extractTarIgnoreChown(layer, rootfs); err == nil {
				t.Errorf("entry %q was not refused", name)
			}
		})
	}
}

// The other half of the fix: containment must not cost the extraction anything
// a real image needs. Absolute symlinks are everywhere in an ubuntu rootfs
// (/lib -> /usr/lib, all of /etc/alternatives), and creating one is not
// following one, so they must still be written exactly as the image asked.
func TestExtractTarKeepsAbsoluteAndRelativeSymlinks(t *testing.T) {
	rootfs, _ := escapeSetup(t)

	layer := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "usr/lib/", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{hdr: tar.Header{Name: "usr/lib/libc.so", Typeflag: tar.TypeReg, Mode: 0o755}, body: "libc"},
		tarEntry{hdr: tar.Header{Name: "lib", Typeflag: tar.TypeSymlink, Linkname: "usr/lib", Mode: 0o777}},
		tarEntry{hdr: tar.Header{Name: "usr/bin/awk", Typeflag: tar.TypeSymlink, Linkname: "/etc/alternatives/awk", Mode: 0o777}},
	)
	if err := extractTarIgnoreChown(layer, rootfs); err != nil {
		t.Fatalf("a well-formed layer must still extract: %v", err)
	}

	for path, want := range map[string]string{
		"lib":         "usr/lib",
		"usr/bin/awk": "/etc/alternatives/awk",
	} {
		got, err := os.Readlink(filepath.Join(rootfs, path))
		if err != nil {
			t.Errorf("Readlink(%s): %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", path, got, want)
		}
	}
	// And a symlink that stays inside the rootfs is still walkable.
	if _, err := os.ReadFile(filepath.Join(rootfs, "lib", "libc.so")); err != nil {
		t.Errorf("contained symlink should still resolve: %v", err)
	}

	// A later layer writing *through* that symlink must land on the other side
	// of it, not be refused. This is the case containment is most likely to
	// break by accident, and the one an image is most likely to need.
	next := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "lib/libm.so", Typeflag: tar.TypeReg, Mode: 0o755}, body: "libm"},
		tarEntry{hdr: tar.Header{Name: "lib/subdir/x", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
	)
	if err := extractTarIgnoreChown(next, rootfs); err != nil {
		t.Fatalf("writing through a contained symlink must work: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "usr", "lib", "libm.so")); err != nil || string(got) != "libm" {
		t.Errorf("usr/lib/libm.so = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "usr", "lib", "subdir", "x")); err != nil || string(got) != "x" {
		t.Errorf("usr/lib/subdir/x = %q, %v", got, err)
	}
}

// gosec G110: io.Copy off a tar reader with nothing bounding it. The caps are
// far above any real image, so the test shrinks them rather than building a
// 16GiB archive; what it proves is that the guard fires and names its limit,
// not where the limit sits.
func TestExtractTarRefusesDecompressionBomb(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		rootfs, _ := escapeSetup(t)
		defer func(v int64) { layerByteCap = v }(layerByteCap)
		layerByteCap = 16

		layer := rawTarball(t, tarEntry{
			hdr:  tar.Header{Name: "bomb", Typeflag: tar.TypeReg, Mode: 0o644},
			body: strings.Repeat("A", 1024),
		})
		err := extractTarIgnoreChown(layer, rootfs)
		if err == nil {
			t.Fatal("a layer past the byte cap must be refused")
		}
		if !strings.Contains(err.Error(), "byte limit") {
			t.Errorf("error should name the limit, got: %v", err)
		}
	})

	t.Run("entries", func(t *testing.T) {
		rootfs, _ := escapeSetup(t)
		defer func(v int) { layerEntryCap = v }(layerEntryCap)
		layerEntryCap = 4

		entries := make([]tarEntry, 0, 16)
		for i := 0; i < 16; i++ {
			entries = append(entries, tarEntry{hdr: tar.Header{
				Name:     "f" + strconv.Itoa(i),
				Typeflag: tar.TypeReg,
				Mode:     0o644,
			}})
		}
		err := extractTarIgnoreChown(rawTarball(t, entries...), rootfs)
		if err == nil {
			t.Fatal("a layer past the entry cap must be refused")
		}
		if !strings.Contains(err.Error(), "entries") {
			t.Errorf("error should name the limit, got: %v", err)
		}
	})
}

// rootfsBytes sums the regular-file bytes sitting under dir, which is the only
// way to see what a refused pull managed to write before it was refused.
func rootfsBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		// A directory, or an entry that vanished under the walk, contributes
		// nothing; neither is what this is measuring.
		if err != nil || d.IsDir() {
			return nil
		}
		fi, serr := d.Info()
		if serr == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return total
}

// The caps used to live only in the manual extractor, which is the *second*
// pass. containerd's archive.Apply runs first and had nothing bounding it at
// all, so a bomb was fully written to disk and only then noticed -- measured at
// 1,048,576 bytes against a 16-byte budget, four orders of magnitude over.
//
// The layer below is shaped to make that visible after the fact rather than
// only during: the first entry trips the cap, so the manual pass stops on it
// and never rewrites the big one behind it. Whatever archive.Apply put on disk
// is therefore still there to be measured when the pull returns.
func TestUnpackLayerBoundsTheFirstPass(t *testing.T) {
	dir := t.TempDir()
	defer func(v int64) { layerByteCap = v }(layerByteCap)
	layerByteCap = 16

	// Owned by whoever runs the test, not by root. A root-owned entry makes
	// archive.Apply stop on its own at the first chown, which hides how much
	// it would otherwise write; the images that matter here are the ones it
	// gets all the way through.
	uid, gid := os.Getuid(), os.Getgid()
	body := rawTarball(t,
		tarEntry{
			hdr:  tar.Header{Name: "trip", Typeflag: tar.TypeReg, Mode: 0o644, Uid: uid, Gid: gid},
			body: strings.Repeat("A", 17),
		},
		tarEntry{
			hdr:  tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Uid: uid, Gid: gid},
			body: strings.Repeat("B", 1<<20),
		},
	)
	layer := &flakyLayer{data: body.Bytes()}

	err := unpackLayerWithRetry(layer, dir, 0, nil)
	if err == nil {
		t.Fatal("a layer past the byte cap must fail the pull")
	}
	if !errors.Is(err, errLayerBomb) {
		t.Errorf("error should identify a decompression bomb, got: %v", err)
	}
	// The whole point of the taxonomy fix: a bomb must never be filed as the
	// expected non-root chown failure and carried on from.
	if errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("a bomb must not be reported as a permission error, got: %v", err)
	}
	if got := rootfsBytes(t, dir); got > layerByteCap {
		t.Errorf("%d bytes reached the disk against a %d byte cap; the first pass is unbounded",
			got, layerByteCap)
	}
}

// A hard link is not dereferenced when it is made -- link(2) copies a symlink
// source rather than following it. A symlink out of the rootfs followed by a
// hard link to that symlink therefore produced a second host-pointing symlink
// under a name nothing had any reason to refuse, while root.ReadFile on the
// very same name refused it.
func TestExtractTarRefusesHardLinkToSymlink(t *testing.T) {
	for _, kind := range []string{"absolute", "relative"} {
		t.Run(kind, func(t *testing.T) {
			rootfs, outside := escapeSetup(t)
			victim := filepath.Join(outside, "secret")
			if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			layer := rawTarball(t,
				tarEntry{hdr: tar.Header{
					Name:     "ln",
					Typeflag: tar.TypeSymlink,
					Linkname: symlinkEscapes(outside)[kind],
					Mode:     0o777,
				}},
				tarEntry{hdr: tar.Header{
					Name:     "stolen",
					Typeflag: tar.TypeLink,
					Linkname: "ln",
					Mode:     0o644,
				}},
			)

			err := extractTarIgnoreChown(layer, rootfs)
			if err == nil {
				t.Fatal("extraction reported success on a hard link to a symlink")
			}
			if !strings.Contains(err.Error(), "stolen") {
				t.Errorf("error should name the refused entry, got: %v", err)
			}
			if _, serr := os.Lstat(filepath.Join(rootfs, "stolen")); !os.IsNotExist(serr) {
				t.Errorf("a link out of the rootfs was materialized: %v", serr)
			}
		})
	}
}

// Regular files were chmodded and directories were not, so every directory in
// every image landed 0755: /tmp lost its sticky bit and its world-write, which
// breaks most of what an agent image runs, and /root came out readable by
// everyone. hvi answers the guest from the recorded xattr rather than the host
// mode, which is why this only showed on vz and qemu.
func TestExtractTarAppliesDirectoryModes(t *testing.T) {
	rootfs, _ := escapeSetup(t)

	layer := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777}},
		tarEntry{hdr: tar.Header{Name: "root/", Typeflag: tar.TypeDir, Mode: 0o700}},
		tarEntry{hdr: tar.Header{Name: "root/.bashrc", Typeflag: tar.TypeReg, Mode: 0o644}, body: "rc"},
		// A directory the image wants unwritable, holding a file. The mode can
		// only go on once the layer is through, or the file below it is lost.
		tarEntry{hdr: tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o500}},
		tarEntry{hdr: tar.Header{Name: "ro/f", Typeflag: tar.TypeReg, Mode: 0o444}, body: "f"},
	)
	// Registered before the extraction so it runs before t.TempDir's own
	// cleanup, which cannot delete through a directory with no write bit.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(rootfs, "ro"), 0o755) })

	if err := extractTarIgnoreChown(layer, rootfs); err != nil {
		t.Fatalf("extract: %v", err)
	}

	for name, want := range map[string]int64{
		"tmp":  0o1777,
		"root": 0o700,
		"ro":   0o500,
	} {
		fi, err := os.Stat(filepath.Join(rootfs, name))
		if err != nil {
			t.Errorf("Stat(%s): %v", name, err)
			continue
		}
		got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		if got != posixFileMode(want) {
			t.Errorf("%s mode = %v, want %v (image asked for %#o)", name, got, posixFileMode(want), want)
		}
	}

	// The ordering half: tightening a directory must not cost the entries
	// underneath it.
	if got, err := os.ReadFile(filepath.Join(rootfs, "root/.bashrc")); err != nil || string(got) != "rc" {
		t.Errorf("root/.bashrc = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "ro/f")); err != nil || string(got) != "f" {
		t.Errorf("ro/f = %q, %v", got, err)
	}
}

// A whiteout names what it deletes after the ".wh." prefix. A bare ".wh." left
// nothing after it, and filepath.Join folded the empty name into the directory
// the entry sits in -- so the entry deleted its own parent. "a/b/.wh..." is the
// same bug one level up: it trims to "..", which resolves to a/b's parent.
func TestExtractTarRefusesEmptyWhiteoutName(t *testing.T) {
	for _, entry := range []string{"data/.wh.", "data/sub/.wh..."} {
		t.Run(entry, func(t *testing.T) {
			rootfs, _ := escapeSetup(t)

			base := rawTarball(t,
				tarEntry{hdr: tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755}},
				tarEntry{hdr: tar.Header{Name: "data/keep", Typeflag: tar.TypeReg, Mode: 0o644}, body: "keep"},
				tarEntry{hdr: tar.Header{Name: "data/sub/", Typeflag: tar.TypeDir, Mode: 0o755}},
				tarEntry{hdr: tar.Header{Name: "data/sub/keep", Typeflag: tar.TypeReg, Mode: 0o644}, body: "keep"},
			)
			if err := extractTarIgnoreChown(base, rootfs); err != nil {
				t.Fatalf("base layer: %v", err)
			}

			wh := rawTarball(t, tarEntry{hdr: tar.Header{
				Name: entry, Typeflag: tar.TypeReg, Mode: 0o644,
			}})
			if err := extractTarIgnoreChown(wh, rootfs); err == nil {
				t.Errorf("whiteout entry %q was not refused", entry)
			}
			for _, kept := range []string{"data/keep", "data/sub/keep"} {
				if _, err := os.Stat(filepath.Join(rootfs, kept)); err != nil {
					t.Errorf("%s was deleted by a whiteout that named nothing: %v", kept, err)
				}
			}
		})
	}
}
