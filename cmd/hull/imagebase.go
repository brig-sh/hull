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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// An image's base is the ext4 image `hull run` builds from an OCI image's
// rootfs, with the ownership and modes the image records, and nothing of
// any one instance: no /urunit.conf, no init wrapper, no /etc/hosts. It is
// built once per image (and size) on a host and kept under <store>/bases by
// its disk digest; every instance of the image on that host starts from a
// copy of it, with its own files written in. Instances of one image then
// share one base, and a destination that holds it takes a move of any of
// them by the blocks that differ (docs/migration.md, "Bases").
//
// mke2fs writes a fresh UUID, hash seed and timestamps into every image, so
// two hosts build two bases of one image: a base goes to another host once
// per (image, host it was built on, destination).

// imageBaseRecipe changes whenever hull builds an image's base differently,
// so a base built the old way is not taken for one built the new way.
const imageBaseRecipe = "hull-image-base-1"

// imageBaseKey names the base of image imageDigest built at sizeMB, or is
// "" for an image hull cannot name by content (a local bundle).
func imageBaseKey(imageDigest string, sizeMB int) string {
	if !strings.HasPrefix(imageDigest, "sha256:") {
		return ""
	}
	return sha256Hex([]byte(fmt.Sprintf("%s\x00%s\x00%d", imageBaseRecipe, imageDigest, sizeMB)))
}

// imageBaseEntry is <store>/bases/images/<key>.json: which base an image's
// instances start from on this host.
type imageBaseEntry struct {
	Digest  string    `json:"digest"`
	Image   string    `json:"image"`
	SizeMB  int       `json:"sizeMB"`
	Recipe  string    `json:"recipe"`
	Created time.Time `json:"created"`
}

// imageBasesDir is where the entries live.
func imageBasesDir(s *store.Store) string { return filepath.Join(s.BasesDir(), "images") }

// readImageBaseEntry reads the entry of key, nil when there is none.
func readImageBaseEntry(s *store.Store, key string) (*imageBaseEntry, error) {
	data, err := os.ReadFile(filepath.Join(imageBasesDir(s), key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e imageBaseEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("the image base entry %s: %w", key, err)
	}
	if err := checkDiskDigest(e.Digest); err != nil {
		return nil, fmt.Errorf("the image base entry %s: %w", key, err)
	}
	return &e, nil
}

// imageBaseEntries lists every entry, by key.
func imageBaseEntries(s *store.Store) (map[string]*imageBaseEntry, error) {
	names, err := os.ReadDir(imageBasesDir(s))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*imageBaseEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]*imageBaseEntry{}
	for _, n := range names {
		key, ok := strings.CutSuffix(n.Name(), ".json")
		if !ok || checkDiskDigest(key) != nil {
			continue
		}
		e, err := readImageBaseEntry(s, key)
		if err != nil || e == nil {
			continue
		}
		out[key] = e
	}
	return out, nil
}

// lockImageBase holds the build of key's base, so two runs of one image
// build it once.
func lockImageBase(s *store.Store, key string) (func(), error) {
	if err := os.MkdirAll(imageBasesDir(s), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(imageBasesDir(s), key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// imageBase returns the digest of the base of the image whose rootfs is
// rootfsDir, and whether this call built it: the one the store keeps for
// imageDigest at sizeMB, or a new one, built, kept, and recorded as that
// image's (a local image's is built every time and recorded nowhere).
func imageBase(ctx context.Context, s *store.Store, imageDigest, rootfsDir string, sizeMB int) (string, bool, error) {
	key := imageBaseKey(imageDigest, sizeMB)
	if key != "" {
		unlock, err := lockImageBase(s, key)
		if err != nil {
			return "", false, err
		}
		defer unlock()
		e, err := readImageBaseEntry(s, key)
		if err != nil {
			log.Warnf("%v; building the image's base again", err)
		} else if e != nil && baseHeld(s, e.Digest) {
			return e.Digest, false, nil
		}
	}
	if err := os.MkdirAll(s.BasesDir(), 0o700); err != nil {
		return "", false, err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(s.BasesDir(), ".new-"+hex.EncodeToString(rnd[:]))
	err := makeBlockImage(ctx, tmp, rootfsDir, sizeMB)
	if err == nil {
		err = fixupBlockImage(ctx, tmp, rootfsDir, nil)
	}
	var sum string
	if err == nil {
		sum, _, err = diskDigestFile(tmp)
	}
	if err == nil {
		err = sealBase(s, tmp, sum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	if key != "" {
		js, err := json.MarshalIndent(imageBaseEntry{Digest: sum, Image: imageDigest, SizeMB: sizeMB,
			Recipe: imageBaseRecipe, Created: time.Now().UTC()}, "", "  ")
		if err != nil {
			return "", false, err
		}
		if err := writeFileAtomic(filepath.Join(imageBasesDir(s), key+".json"), append(js, '\n')); err != nil {
			return "", false, err
		}
	}
	return sum, true, nil
}

// writeFileAtomic writes data to path through a synced temporary file
// renamed over it, then syncs the directory.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFileSynced(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// diskFromImageBase makes the instance's disk at diskPath: a copy of its
// image's base (a clone where the filesystem has them), with the
// instance's own files written in. It returns the base's digest.
func diskFromImageBase(ctx context.Context, s *store.Store, imageDigest, diskPath, rootfsDir string, injects []blockInject,
	sizeMB int) (string, error) {
	started := time.Now()
	sum, built, err := imageBase(ctx, s, imageDigest, rootfsDir, sizeMB)
	if err != nil {
		return "", fmt.Errorf("the image's base: %w", err)
	}
	_ = os.Remove(diskPath)
	how, err := copyDisk(s.BasePath(sum), diskPath)
	if err != nil {
		return "", fmt.Errorf("copy the image's base: %w", err)
	}
	if err := os.Chmod(diskPath, 0o600); err != nil {
		return "", err
	}
	if err := fixupBlockImage(ctx, diskPath, "", injects); err != nil {
		return "", err
	}
	log.Debugf("Disk %s from base %s (built %v, %s) in %s", diskPath, digestHead(sum), built, how,
		time.Since(started))
	return sum, nil
}

// A destination's spare of a base is a ready copy of it, made ahead of a
// move (by `migrate-prepare`, when a prepare sends the base, or by the
// source of a move once it commits, for the way back), which a streamed
// receive takes as the instance's disk by renaming it. Where the
// filesystem has no clones a copy of a 1.6 GB base took the destination
// 22 s, all of it before the source could start the disk rounds.

// spareBasesDir holds the spares of bases, and their locks.
func spareBasesDir(s *store.Store) string { return filepath.Join(s.BasesDir(), "spare") }

// spareBasePath is where the spare of the base sum is kept.
func spareBasePath(s *store.Store, sum string) string {
	return filepath.Join(spareBasesDir(s), filepath.Base(sum)+".ext4")
}

// spareLockPath is the lock one maker of the spare of base sum holds.
func spareLockPath(s *store.Store, sum string) string {
	return filepath.Join(spareBasesDir(s), filepath.Base(sum)+".lock")
}

// ensureSpareBase makes the spare of the held base sum unless there is one,
// and reports how ("held", "clone" or "copy"). Two makers of the same
// spare, a prepare and the background one after a move, take turns, so
// the second finds it made rather than copying the base again.
func ensureSpareBase(s *store.Store, sum string) (string, error) {
	if !baseHeld(s, sum) {
		return "", fmt.Errorf("base %s is not held here", digestHead(sum))
	}
	if _, err := os.Stat(spareBasePath(s, sum)); err == nil {
		return "held", nil
	}
	unlock, err := lockSpare(s, sum)
	if err != nil {
		return "", err
	}
	defer unlock()
	return makeSpareLocked(s, sum)
}

// lockSpare takes the lock of the spare of base sum, waiting for a maker
// that holds it, and returns its release. The lock file stays: a maker
// that held the lock of a file removed under it would let the next maker
// lock a new one and copy the base beside it.
func lockSpare(s *store.Store, sum string) (func(), error) {
	path := spareLockPath(s, sum)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock the spare of base %s: %w", digestHead(sum), err)
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

// spareCopy copies a base into its spare. A variable so a test can make
// the copy as slow as a Pi's.
var spareCopy = copyDisk

// makeSpareLocked is ensureSpareBase with the spare's lock held. The copy
// is synced before it is put in place: the receive that takes it syncs
// the disk before the source's guest may stop, and a copy still in
// writeback would make that sync seconds long on a Pi.
func makeSpareLocked(s *store.Store, sum string) (string, error) {
	dst := spareBasePath(s, sum)
	if _, err := os.Stat(dst); err == nil {
		return "held", nil
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(dst), ".new-"+hex.EncodeToString(rnd[:]))
	how, err := spareCopy(s.BasePath(sum), tmp)
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = syncPath(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if !baseHeld(s, sum) {
		// The base was removed while it was copied: a spare of a base
		// that is gone would never be taken.
		_ = os.Remove(dst)
		return "", fmt.Errorf("base %s was removed while its spare was made", digestHead(sum))
	}
	return how, nil
}

// spareMakerWait bounds a streamed receive's wait for a maker of the spare
// at work: one that is stuck must not hold the move up for good. A copy
// of the receive's own took 22 s on a Pi.
var spareMakerWait = time.Minute

// spareMakerBusy is called when a receive finds a maker of the spare at
// work. A variable so a test can let the maker finish only then.
var spareMakerBusy = func() {}

// lockPoll is how often a bounded wait for a lock tries it again.
const lockPoll = 10 * time.Millisecond

// flockWait takes the exclusive lock of f, trying it again until wait has
// passed, and reports whether it got it and how long it waited for it:
// none when the lock was free. busy, unless nil, is called once the lock
// is found held.
func flockWait(f *os.File, wait time.Duration, busy func()) (bool, time.Duration, error) {
	started := time.Now()
	for tries := 0; ; tries++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil && tries == 0 {
			return true, 0, nil
		}
		if err == nil {
			return true, time.Since(started), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return false, time.Since(started), err
		}
		if tries == 0 && busy != nil {
			busy()
		}
		if time.Since(started) >= wait {
			return false, time.Since(started), nil
		}
		time.Sleep(lockPoll)
	}
}

// waitSpareMaker waits, up to wait, for a maker of the spare of base sum
// at work here, such as the background step of a move that left this
// host, and returns the release of the spare's lock, how long it waited
// and whether it gave up, at the bound or on an error of the lock. A store where no maker ever ran has no lock to
// wait for, and none is made: the receive takes no part in the makers'
// turns but this.
func waitSpareMaker(s *store.Store, sum string, wait time.Duration) (func(), time.Duration, bool) {
	lock, err := os.OpenFile(spareLockPath(s, sum), os.O_RDWR, 0)
	if err != nil {
		return func() {}, 0, false
	}
	got, waited, err := flockWait(lock, wait, spareMakerBusy)
	if err != nil || !got {
		// A lock that cannot be taken is not waited for either.
		_ = lock.Close()
		return func() {}, waited, true
	}
	return sync.OnceFunc(func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}), waited, false
}

// syncPath syncs the file at path.
func syncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = syncFile(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// takeSpareBase moves the spare of base sum to dst, a path on the same
// filesystem, and reports whether there was one.
func takeSpareBase(s *store.Store, sum, dst string) bool {
	return checkDiskDigest(sum) == nil && os.Rename(spareBasePath(s, sum), dst) == nil
}
