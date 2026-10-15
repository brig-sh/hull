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
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// diskLayout writes size bytes at path whose content is data at each offset
// and zeros elsewhere. dense writes the zeros out; otherwise they are holes.
func diskLayout(t *testing.T, path string, size int64, data map[int64][]byte, dense bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if dense {
		if _, err := f.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
	} else if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, b := range data {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
}

// The digest is of the bytes alone: the same content written densely, with
// holes, or with explicit zero runs inside an extent that starts off a block
// boundary hashes the same, and any change of one byte or of the size does
// not.
func TestDiskDigestIsOfTheContentOnly(t *testing.T) {
	dir := t.TempDir()
	const size = 3<<20 + 4096 + 17 // a short last block
	content := map[int64][]byte{
		0:                         bytes.Repeat([]byte{1}, 100),
		5*4096 + 3:                bytes.Repeat([]byte{2}, 9000),
		1 << 20:                   bytes.Repeat([]byte{3}, diskBlockSize),
		size - 5:                  {9, 9, 9, 9, 9},
		2<<20 + diskBlockSize - 1: {7, 7},
	}
	dense := filepath.Join(dir, "dense")
	sparse := filepath.Join(dir, "sparse")
	diskLayout(t, dense, size, content, true)
	diskLayout(t, sparse, size, content, false)
	// Zeros written over a hole make an extent of zeros, which hashes as
	// the hole did.
	odd := filepath.Join(dir, "odd")
	diskLayout(t, odd, size, content, false)
	f, err := os.OpenFile(odd, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 3*diskBlockSize), 2<<20+4*diskBlockSize+4096); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	want, data, err := diskDigestFile(dense)
	if err != nil {
		t.Fatal(err)
	}
	// Blocks 0, 16, 32 and 33 (the {7,7} pair straddles them), and the
	// short last block, 48, of 4096+17 bytes.
	if data != 4*diskBlockSize+4096+17 {
		t.Errorf("data bytes %d", data)
	}
	for _, p := range []string{sparse, odd} {
		got, _, err := diskDigestFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s hashes %s, the dense copy %s", filepath.Base(p), got, want)
		}
	}

	changed := filepath.Join(dir, "changed")
	diskLayout(t, changed, size, content, false)
	f, err = os.OpenFile(changed, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, size-1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	bigger := filepath.Join(dir, "bigger")
	diskLayout(t, bigger, size+1, content, false)
	for _, p := range []string{changed, bigger} {
		if got, _, _ := diskDigestFile(p); got == want {
			t.Errorf("%s hashes the same as the original", filepath.Base(p))
		}
	}
}

// A digest fed blocks out of order, or a block of the wrong length, refuses.
func TestDiskDigestRefusesBadBlocks(t *testing.T) {
	d := newDiskDigest(3 * diskBlockSize)
	if err := d.add(1, make([]byte, diskBlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := d.add(0, make([]byte, diskBlockSize)); err == nil {
		t.Error("a block before the last one was taken")
	}
	if err := d.add(2, make([]byte, 10)); err == nil {
		t.Error("a short middle block was taken")
	}
	if err := d.add(3, make([]byte, diskBlockSize)); err == nil {
		t.Error("a block past the end was taken")
	}
}

// keepBase keeps one read-only copy per digest, with the image's bytes, and
// the copy keeps the holes.
func TestKeepBase(t *testing.T) {
	s := shortStore(t)
	disk := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeSparseDisk(t, disk, 64<<20)
	sum, err := keepBase(s, disk)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := diskDigestFile(disk)
	if err != nil || sum != want {
		t.Fatalf("base %s, image %s (%v)", sum, want, err)
	}
	info, err := os.Stat(s.BasePath(sum))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("the base is %v, not read-only", info.Mode().Perm())
	}
	if got := fileAllocated(s.BasePath(sum)); got > 4<<20 {
		t.Errorf("the base of a 64 MiB sparse image occupies %d bytes", got)
	}
	assertSameContent(t, disk, s.BasePath(sum))
	// The same image again keeps the one base and leaves nothing behind.
	if again, err := keepBase(s, disk); err != nil || again != sum {
		t.Fatalf("again: %s %v", again, err)
	}
	entries, _ := os.ReadDir(s.BasesDir())
	if len(entries) != 1 {
		t.Errorf("bases holds %d entries after two runs of one image", len(entries))
	}
	// The guest's writes to its disk do not reach the base.
	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("guest"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got, _, _ := diskDigestFile(s.BasePath(sum)); got != sum {
		t.Errorf("the base changed with the disk: %s", got)
	}
}

// The written bitmap sets exactly the blocks where the disk differs from
// its base: a block changed, one written where the base has a hole, one
// zeroed where the base had data, and the short last
// block; a block written with the base's own bytes is not set.
func TestDiskWrittenBitmap(t *testing.T) {
	dir := t.TempDir()
	const size = 20*diskBlockSize + 100
	base, disk := filepath.Join(dir, "base"), filepath.Join(dir, "disk")
	write := func(path string, at int64, b []byte) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(b, at); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	block := func(c byte) []byte { return bytes.Repeat([]byte{c}, diskBlockSize) }
	for _, i := range []int64{1, 2, 3, 5, 7} {
		write(base, i*diskBlockSize, block(byte(i)))
	}
	write(base, 20*diskBlockSize, bytes.Repeat([]byte{9}, 100))
	if _, err := copyDisk(base, disk); err != nil {
		t.Fatal(err)
	}
	write(disk, 1*diskBlockSize, block(0xee))         // changed
	write(disk, 2*diskBlockSize, block(2))            // the base's own bytes
	write(disk, 3*diskBlockSize, block(0))            // zeroed
	write(disk, 10*diskBlockSize+7, []byte{1})        // into a hole
	write(disk, 20*diskBlockSize, []byte{8, 8, 8, 8}) // the short last block
	bits, n, err := diskWrittenBitmap(base, disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(bits) != 3 || n != 4 {
		t.Fatalf("%d bytes, %d set", len(bits), n)
	}
	var set []int64
	for i := int64(0); i < 21; i++ {
		if bits[i/8]&(1<<(i%8)) != 0 {
			set = append(set, i)
		}
	}
	if fmt.Sprint(set) != "[1 3 10 20]" {
		t.Errorf("set %v", set)
	}
	if _, _, err := diskWrittenBitmap(base, filepath.Join(dir, "none")); err == nil {
		t.Error("a missing disk compared")
	}
}
