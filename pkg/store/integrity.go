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

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrImageDamaged says a cached image is not what its pull published.
var ErrImageDamaged = errors.New("image is damaged")

// rootfsEntriesFile holds the number of entries under rootfs/ at the moment
// the image was published. It is what lets a later pull tell an image that
// has lost part of its tree since from one that is still what was published.
const rootfsEntriesFile = "rootfs-entries"

// CountRootfsEntries counts every entry under rootfs/ in imageDir, the
// directory itself excluded.
//
// Nothing writes into a published rootfs: `hull run` shares it read-only or
// clones it into the instance, so the count a pull records holds for as long
// as the image does.
//
// A directory that cannot be listed counts as one entry and its contents as
// none. Unpack puts the image's own mode on every directory it ships, and an
// image is free to ship one its owner cannot read; the walk sees the same
// mode when the count is recorded and when it is checked, so the two agree,
// and a pull is not failed over a count. Only a rootfs that is not there at
// all is an error.
func CountRootfsEntries(imageDir string) (int, error) {
	root := filepath.Join(imageDir, "rootfs")
	n := -1 // the root is visited first and does not count
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// WalkDir has already called back for this directory once,
			// without an error, and it was counted then.
			return fs.SkipDir
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// WriteRootfsEntries records the rootfs entry count in imageDir. Call it on
// the staging directory, after the last layer is unpacked and before the
// rename that publishes it, so a published image always carries the count it
// was published with.
func WriteRootfsEntries(imageDir string) error {
	n, err := CountRootfsEntries(imageDir)
	if err != nil {
		return fmt.Errorf("failed to count the rootfs entries: %w", err)
	}
	p := filepath.Join(imageDir, rootfsEntriesFile)
	if err := os.WriteFile(p, []byte(strconv.Itoa(n)+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to record the rootfs entry count: %w", err)
	}
	return nil
}

// readRootfsEntries returns the recorded count, and false when the image
// carries none, which is what an image published before the count existed
// looks like.
func readRootfsEntries(imageDir string) (int, bool, error) {
	data, err := os.ReadFile(filepath.Join(imageDir, rootfsEntriesFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("the recorded rootfs entry count %q is not a count", strings.TrimSpace(string(data)))
	}
	return n, true, nil
}

// VerifyImage checks a cached image against what its pull recorded, and
// returns an error wrapping ErrImageDamaged when the rootfs has lost entries
// since.
//
// ImageComplete says the pieces are there; this says the tree is still the
// size it was published at. An image can lose entries after publication and
// look no different from here: a sweep's RemoveAll that was still running
// through the staging directory when the rename published it, or anything
// else that deletes under images/. Every later lookup found such an image
// complete and reused it, and `hull rmi` was the only way out. The pull path
// checks this before deciding a digest it already holds needs no unpack, so
// one `hull pull` repairs it instead.
//
// What the count cannot tell is a tree that was already short when it was
// counted: it is taken after the last layer is unpacked, so an unpack that
// lost entries along the way records the tree it ended up with. That is the
// sweep race, and the sweep is what was fixed; this is the check for what
// happens to an image afterwards.
//
// Only a lower count is damage. Losing part of the tree is the one failure
// this exists for, and it can only take entries away; an entry added beside
// the image's own -- a .DS_Store from a Finder window, say -- is not that,
// and re-unpacking a whole image over it would be the wrong answer.
//
// An image that recorded no count cannot be checked and passes: it was
// published by a hull from before the count existed, and refusing it would
// re-unpack every image in every existing store.
//
// The walk runs without the store lock, since it is on the `--pull=always`
// path and every other process's instance records go through that lock; a
// walk of twenty thousand entries with it held would stall a `hull ps` next
// door. What the lock is for is a commit of the same digest in another
// process: its swap happens under the lock, and a walk that saw the swap
// half-way would read a whole image as damaged, or not find the rootfs at
// all. So a shortfall, and a walk that failed, are confirmed under the lock,
// by reading the count and walking again; only what holds with nothing
// swapping is the answer.
func (s *Store) VerifyImage(digest string) error {
	if err := ValidateImageDigest(digest); err != nil {
		return err
	}
	imageDir := s.ImageDir(digest)
	short, err := s.rootfsShort(imageDir)
	if err == nil && short == nil {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	short, err = s.rootfsShort(imageDir)
	if err != nil {
		return err
	}
	return short
}

// rootfsShort compares the rootfs against its recorded count, and returns
// the ErrImageDamaged error for a shortfall, nil for a tree that is whole or
// unrecorded, and any other error for a record or a tree that could not be
// read.
func (s *Store) rootfsShort(imageDir string) (short error, err error) {
	recorded, ok, err := readRootfsEntries(imageDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageDamaged, err)
	}
	if !ok {
		return nil, nil
	}
	found, err := CountRootfsEntries(imageDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageDamaged, err)
	}
	if found < recorded {
		return fmt.Errorf("%w: rootfs holds %d entries, %d were recorded when it was pulled",
			ErrImageDamaged, found, recorded), nil
	}
	return nil, nil
}

// ImageHolders returns the records of the instances that name the digest,
// running or stopped, sorted by id.
//
// This is what `hull rmi` and `hull prune` refuse to remove an image over,
// and what a pull that would displace an image has to check first: a live
// instance shares the image's rootfs with its guest, and a stopped one
// expects it back on the next start. Replacing the directory under either
// is not a repair. Which records are live is the caller's to decide: a
// record's status alone is not it, since a death after the pid write
// leaves one at "running" for good, and the check that tells a VMM from a
// recycled pid lives with the commands.
func (s *Store) ImageHolders(digest string) ([]*InstanceState, error) {
	instances, err := s.ListInstances()
	if err != nil {
		return nil, err
	}
	var holders []*InstanceState
	for _, inst := range instances {
		if inst.ImageDigest == digest {
			holders = append(holders, inst)
		}
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].ID < holders[j].ID })
	return holders, nil
}
