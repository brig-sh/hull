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
	"errors"
	"io"
	"os"
	"path/filepath"
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
