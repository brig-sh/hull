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

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// streamOf writes a stream of the given bundle files and disk.
func streamOf(t *testing.T, files map[string]string, disk string) []byte {
	t.Helper()
	var buf bytes.Buffer
	sw := newMigrateStreamWriter(&buf)
	if err := sw.header(streamHeader{TxID: "t", ManifestSHA256: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "ram.bin"} {
		if path, ok := files[name]; ok {
			if _, err := sw.file("bundle/"+name, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	if disk != "" {
		if _, err := sw.sparse("disk", disk); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.finish(); err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) != sw.written() {
		t.Errorf("written() = %d, the buffer holds %d", sw.written(), buf.Len())
	}
	return buf.Bytes()
}

// receiveInto reads a stream into dir.
func receiveInto(dir string, stream []byte) (*migrateStreamResult, error) {
	_ = os.MkdirAll(filepath.Join(dir, "bundle"), 0o700)
	return readMigrateStream(bytes.NewReader(stream), func(name string, _ int64) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, filepath.FromSlash(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	})
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

// A disk with holes arrives byte for byte the same, with its holes still
// holes, and the stream carries its data and not its size.
func TestMigrateStreamKeepsHoles(t *testing.T) {
	src := t.TempDir()
	disk := filepath.Join(src, "disk")
	const size = 256 << 20
	writeSparseDisk(t, disk, size)
	// A run of zeros written as data is left out too.
	f, _ := os.OpenFile(disk, os.O_WRONLY, 0)
	_, _ = f.WriteAt(make([]byte, 1<<20), 100<<20)
	_ = f.Close()
	man := filepath.Join(src, "manifest.json")
	_ = os.WriteFile(man, []byte(`{"components":[]}`), 0o600)

	stream := streamOf(t, map[string]string{"manifest.json": man}, disk)
	if len(stream) > 1<<20 {
		t.Errorf("the stream of a %d-byte disk with 12 KiB of data is %d bytes", size, len(stream))
	}
	dst := t.TempDir()
	res, err := receiveInto(dst, stream)
	if err != nil {
		t.Fatal(err)
	}
	assertSameContent(t, disk, filepath.Join(dst, "disk"))
	assertSameContent(t, man, filepath.Join(dst, "bundle", "manifest.json"))
	if got := allocated(t, filepath.Join(dst, "disk")); got > 4<<20 {
		t.Errorf("the received disk occupies %d bytes; its holes were filled", got)
	}
	// Data goes in 64 KiB chunks: three 4 KiB writes carry at most three.
	if d := res.Files[1].DataBytes; res.Bytes != int64(len(stream)) || len(res.Files) != 2 || d < 3*4096 || d > 3*migrateChunk {
		t.Errorf("result %+v", res)
	}
	if res.Header.TxID != "t" || res.Header.ManifestSHA256 != "m" {
		t.Errorf("header %+v", res.Header)
	}
}

// A stream is refused whole when it is cut, altered, padded or names a path.
func TestMigrateStreamRefusals(t *testing.T) {
	src := t.TempDir()
	disk := filepath.Join(src, "disk")
	writeSparseDisk(t, disk, 8<<20)
	ram := filepath.Join(src, "ram.bin")
	_ = os.WriteFile(ram, bytes.Repeat([]byte("x"), 10000), 0o600)
	good := streamOf(t, map[string]string{"ram.bin": ram}, disk)
	if _, err := receiveInto(t.TempDir(), good); err != nil {
		t.Fatalf("the good stream: %v", err)
	}
	flip := append([]byte(nil), good...)
	i := bytes.Index(flip, []byte("xxxx"))
	flip[i] = 'y'
	record := func(kind byte, js string) []byte {
		var hdr [5]byte
		hdr[0] = kind
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(js)))
		return append(hdr[:], js...)
	}
	head := append([]byte(migrateStreamMagic), record('H', `{"txid":"t","manifestSha256":"m"}`)...)
	cases := map[string]struct {
		stream []byte
		want   string
	}{
		"cut before the trailer":   {good[:len(good)-20], "ended"},
		"cut inside a file":        {good[:200], "ended"},
		"a changed byte":           {flip, "sha256"},
		"bytes after the trailer":  {append(append([]byte(nil), good...), 0), "after its trailer"},
		"not a stream":             {[]byte("GET / HTTP/1.1\r\n"), "does not start"},
		"a path above the bundle":  {append(append([]byte(nil), head...), record('F', `{"name":"bundle/../../x","size":1}`)...), "not a bundle file"},
		"an absolute path":         {append(append([]byte(nil), head...), record('F', `{"name":"/etc/passwd","size":1}`)...), "not a bundle file"},
		"the disk as a plain file": {append(append([]byte(nil), head...), record('F', `{"name":"disk","size":1}`)...), "plain file"},
		"a negative size":          {append(append([]byte(nil), head...), record('F', `{"name":"bundle/a","size":-1}`)...), "outside"},
		"an unknown field":         {append(append([]byte(nil), head...), record('F', `{"name":"bundle/a","size":1,"mode":511}`)...), "unknown field"},
		"an oversized header":      {append(append([]byte(nil), head...), 'F', 0xff, 0xff, 0xff, 0xff), "over the"},
		"an extent outside the disk": {append(append(append([]byte(nil), head...), record('S', `{"name":"disk","size":4096}`)...),
			0, 0, 0, 0, 0, 0, 0x10, 0, 0, 0, 0, 0, 0, 0, 0x10, 0), "outside"},
	}
	for name, c := range cases {
		_, err := receiveInto(t.TempDir(), c.stream)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// writeSparseDisk makes a sparse file of size with data at a few offsets.
func writeSparseDisk(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 3 << 20, size - 4096} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{byte(off>>12) | 1}, 4096), off); err != nil {
			t.Fatal(err)
		}
	}
}

// assertSameContent compares two files byte for byte.
func assertSameContent(t *testing.T, a, b string) {
	t.Helper()
	da, err1 := os.ReadFile(a)
	db, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil || !bytes.Equal(da, db) {
		t.Errorf("%s and %s differ (%v %v)", a, b, err1, err2)
	}
}
