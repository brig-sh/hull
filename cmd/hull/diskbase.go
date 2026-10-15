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
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// A base is an image's ext4 rootfs as `hull run` builds it, before any
// instance's own files are written in (imagebase.go), kept read-only under
// <store>/bases by its disk digest. A move whose destination holds the same
// base sends only the blocks that differ from it (docs/migration.md,
// "Bases").

// diskBlockSize is the grain of a disk digest and of the disk blocks a
// streamed move sends: 64 KiB, the block hvi's virtio-blk tracks writes at.
const diskBlockSize = 64 << 10

// diskDigestMagic opens every disk digest, so it can never equal the sha256
// of some other byte string hull hashes.
const diskDigestMagic = "HULLDISK1\x00"

// diskDigestPattern is a disk digest as hull writes it: 64 lowercase hex
// digits. It is also a base's file name, so nothing else is accepted.
var diskDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkDiskDigest refuses a string that is not a disk digest.
func checkDiskDigest(sum string) error {
	if !diskDigestPattern.MatchString(sum) {
		return fmt.Errorf("%q is not a disk digest (64 hex digits)", sum)
	}
	return nil
}

// diskDigest hashes a disk image by its content alone: the magic, the size
// and the block size, then, for every 64 KiB block on an absolute boundary
// that is not all zeros, its index and its bytes (the last block may be
// short). A hole and a block of written zeros hash alike, and so do two
// files with the same bytes whatever their filesystems report as data: a
// clone, a sparse copy, an image received over a move. The sha256 of the
// plain file would say the same, but would have to read every hole.
type diskDigest struct {
	h      hash.Hash
	size   int64
	next   int64
	blocks int64
	data   int64
}

func newDiskDigest(size int64) *diskDigest {
	d := &diskDigest{h: sha256.New(), size: size}
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[:8], uint64(size))
	binary.BigEndian.PutUint64(hdr[8:], diskBlockSize)
	_, _ = d.h.Write([]byte(diskDigestMagic))
	_, _ = d.h.Write(hdr[:])
	return d
}

// add hashes block index, which must come after every block added before it
// and hold its full length. A zero block is left out, as a hole is.
func (d *diskDigest) add(index int64, data []byte) error {
	if index < d.next {
		return fmt.Errorf("disk block %d came after block %d", index, d.next-1)
	}
	if want := blockLen(d.size, index); want <= 0 || int64(len(data)) != want {
		return fmt.Errorf("disk block %d has %d bytes, not %d", index, len(data), want)
	}
	d.next = index + 1
	if isZero(data) {
		return nil
	}
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], uint64(index))
	_, _ = d.h.Write(idx[:])
	_, _ = d.h.Write(data)
	d.blocks++
	d.data += int64(len(data))
	return nil
}

// sum is the digest, as hex.
func (d *diskDigest) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// blockLen is the length of block index of a disk of size bytes: a whole
// block, or what is left of the disk for the last one.
func blockLen(size, index int64) int64 {
	return min(diskBlockSize, size-index*diskBlockSize)
}

// diskBlocks is the number of blocks of a disk of size bytes.
func diskBlocks(size int64) int64 {
	return (size + diskBlockSize - 1) / diskBlockSize
}

// forEachDataBlock calls fn, in index order, for every block of f that holds
// a non-zero byte. Only the extents SEEK_DATA reports are read, each rounded
// out to block boundaries; a block fn is handed is valid only during the
// call.
func forEachDataBlock(f *os.File, size int64, fn func(index int64, data []byte) error) error {
	extents, err := dataExtents(f, size)
	if err != nil {
		return err
	}
	const span = 16
	buf := make([]byte, span*diskBlockSize)
	next := int64(0)
	for _, ext := range extents {
		first := max(ext.off/diskBlockSize, next)
		end := diskBlocks(ext.end)
		for i := first; i < end; {
			n := min(span, end-i)
			length := min(n*diskBlockSize, size-i*diskBlockSize)
			if _, err := f.ReadAt(buf[:length], i*diskBlockSize); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("read %s at block %d: %w", f.Name(), i, err)
			}
			for j := int64(0); j < n; j++ {
				lo := j * diskBlockSize
				block := buf[lo:min(lo+diskBlockSize, length)]
				if isZero(block) {
					continue
				}
				if err := fn(i+j, block); err != nil {
					return err
				}
			}
			i += n
		}
		next = max(next, end)
	}
	return nil
}

// diskDigestFile is the disk digest of the file at path, and the bytes of
// its non-zero blocks.
func diskDigestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	d := newDiskDigest(info.Size())
	if err := forEachDataBlock(f, info.Size(), d.add); err != nil {
		return "", 0, err
	}
	return d.sum(), d.data, nil
}

// copySparse copies src to dst, a new file, writing only the blocks of src
// that hold data, so the copy keeps the holes.
func copySparse(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = out.Truncate(info.Size())
	if err == nil {
		err = forEachDataBlock(in, info.Size(), func(index int64, data []byte) error {
			_, err := out.WriteAt(data, index*diskBlockSize)
			return err
		})
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// copyDisk copies the image src to dst, a path that must not exist: a clone
// where the filesystem has them (APFS, or a reflink on Linux), else a copy
// that keeps the holes. It reports which.
func copyDisk(src, dst string) (string, error) {
	if err := cloneFile(src, dst); err == nil {
		return "clone", nil
	}
	_ = os.Remove(dst)
	if err := copySparse(src, dst); err != nil {
		return "", err
	}
	return "copy", nil
}

// keepBase puts an immutable copy of the image at disk under the store's
// bases, named by its disk digest, and returns the digest. It is taken from
// the image as hull built it, before the guest has run: the copy is made
// first, then hashed, so the digest names exactly the bytes kept. A base
// already there under the same digest is kept and the copy dropped.
func keepBase(s *store.Store, disk string) (string, error) {
	started := time.Now()
	if err := os.MkdirAll(s.BasesDir(), 0o700); err != nil {
		return "", err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(s.BasesDir(), ".new-"+hex.EncodeToString(rnd[:]))
	how, err := copyDisk(disk, tmp)
	if err != nil {
		return "", fmt.Errorf("copy the base image: %w", err)
	}
	sum, _, err := diskDigestFile(tmp)
	if err == nil {
		err = sealBase(s, tmp, sum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	log.Debugf("Kept the base of %s as %s (%s, %s)", disk, s.BasePath(sum), how, time.Since(started))
	return sum, nil
}

// sealBase makes the file at tmp, whose disk digest is sum, the base of
// that digest: read-only, synced, then renamed into place. A base already
// there is kept, and tmp removed.
func sealBase(s *store.Store, tmp, sum string) error {
	if err := checkDiskDigest(sum); err != nil {
		return err
	}
	dst := s.BasePath(sum)
	if baseHeld(s, sum) {
		return os.Remove(tmp)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	if err == nil {
		err = os.Chmod(tmp, 0o400)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err == nil {
		err = syncDir(s.BasesDir())
	}
	return err
}

// baseHeld reports whether the store keeps the base of digest sum.
func baseHeld(s *store.Store, sum string) bool {
	if checkDiskDigest(sum) != nil {
		return false
	}
	info, err := os.Stat(s.BasePath(sum))
	return err == nil && info.Mode().IsRegular()
}

// hviDiskBaseFlag hands hvi the digest of the base its disk was made from,
// and hviDiskWrittenFlag the bitmap of the blocks hull wrote over the base
// before the boot (the instance's own files): with both, hvi's blocks
// written since boot are every block that differs from the base, and its
// migrate stream carries them as disk rounds.
const (
	hviDiskBaseFlag    = "--disk-base-sha256"
	hviDiskWrittenFlag = "--disk-written"
)

// diskWrittenBitmap is the bitmap of the 64 KiB blocks where the disk at
// diskPath differs from the base at basePath, as hvi's --disk-written takes
// it: bit i of byte i/8 for block i, ceil(blocks/8) bytes. It also returns
// how many blocks are set. Each block of data on the disk is read once and
// compared with the base's, and then only the base's data blocks the disk
// holds as a hole are read, which a copy of the base has few of.
func diskWrittenBitmap(basePath, diskPath string) ([]byte, int64, error) {
	base, err := os.Open(basePath)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = base.Close() }()
	disk, err := os.Open(diskPath)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = disk.Close() }()
	di, err := disk.Stat()
	if err != nil {
		return nil, 0, err
	}
	bi, err := base.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := di.Size()
	if bi.Size() != size {
		return nil, 0, fmt.Errorf("the base is %d bytes and the disk %d", bi.Size(), size)
	}
	bits := make([]byte, (diskBlocks(size)+7)/8)
	seen := make([]byte, len(bits))
	var n int64
	set := func(i int64) {
		if bits[i/8]&(1<<(i%8)) == 0 {
			bits[i/8] |= 1 << (i % 8)
			n++
		}
	}
	buf := make([]byte, diskBlockSize)
	err = forEachDataBlock(disk, size, func(i int64, data []byte) error {
		seen[i/8] |= 1 << (i % 8)
		b := buf[:len(data)]
		if _, err := base.ReadAt(b, i*diskBlockSize); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read %s at block %d: %w", basePath, i, err)
		}
		if !bytes.Equal(b, data) {
			set(i)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	// A block that holds data in the base and none on the disk.
	extents, err := dataExtents(base, size)
	if err != nil {
		return nil, 0, err
	}
	for _, ext := range extents {
		for i := ext.off / diskBlockSize; i < diskBlocks(ext.end); i++ {
			if seen[i/8]&(1<<(i%8)) != 0 {
				continue
			}
			seen[i/8] |= 1 << (i % 8)
			b := buf[:blockLen(size, i)]
			if _, err := base.ReadAt(b, i*diskBlockSize); err != nil && !errors.Is(err, io.EOF) {
				return nil, 0, fmt.Errorf("read %s at block %d: %w", basePath, i, err)
			}
			if !isZero(b) {
				set(i)
			}
		}
	}
	return bits, n, nil
}

// hviBaseArgs are the arguments that boot hvi against the base sum its disk
// at disk was made from: the base's digest and, written to a file in dir,
// the bitmap of the blocks hull wrote over it (the instance's own files).
// hvi's blocks written since boot then start as those, so its migrate
// stream carries every block that differs from the base.
func hviBaseArgs(s *store.Store, dir, sum, disk string) ([]string, error) {
	started := time.Now()
	bits, n, err := diskWrittenBitmap(s.BasePath(sum), disk)
	if err != nil {
		return nil, fmt.Errorf("compare the disk with base %s: %w", digestHead(sum), err)
	}
	path := filepath.Join(dir, "disk-written.bitmap")
	if err := os.WriteFile(path, bits, 0o600); err != nil {
		return nil, err
	}
	log.Debugf("Disk %s differs from base %s in %d blocks (compared in %s)", disk, digestHead(sum), n,
		time.Since(started))
	return []string{hviDiskBaseFlag, sum, hviDiskWrittenFlag, path}, nil
}
