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
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// ErrInvalidImageDigest rejects anything that is not a store key.
var ErrInvalidImageDigest = errors.New("invalid image digest")

// imageDigestPattern is what a store key looks like: an algorithm name, a
// colon, and the digest in hex.
//
// The store lays every image out as <root>/images/<digest>, so the digest is a
// directory name and the delete path recurses through it. The pattern admits
// no separator and no dot, so a digest that matches cannot name anything
// outside images/, and it excludes the `.tmp-<pid>` and `.old-<pid>` staging
// names by construction.
var imageDigestPattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]{32,128}$`)

// ValidateImageDigest rejects any digest that is not a single safe path
// element naming an image directory.
func ValidateImageDigest(digest string) error {
	if digest == "" {
		return fmt.Errorf("%w: the digest is empty", ErrInvalidImageDigest)
	}
	if !imageDigestPattern.MatchString(digest) {
		return fmt.Errorf("%w %q: a digest is <algorithm>:<hex>", ErrInvalidImageDigest, digest)
	}
	return nil
}

// ImageDir returns the cache directory for an image.
func (s *Store) ImageDir(digest string) string {
	return filepath.Join(s.rootDir, "images", digest)
}

// DeleteImage removes one image from the cache.
//
// It commits the way the pull path does, by renaming the directory aside and
// deleting it afterwards rather than deleting in place. RemoveAll on an
// unpacked rootfs takes seconds and deletes in readdir order, so an interrupt
// part-way through would strip the rootfs while leaving image.json -- the
// half-state that makes an image look cached and every later run fail. The
// rename is atomic, and a leftover `.old-<pid>-<unique>` never satisfies a
// lookup and is swept by the next pull or prune. The directory's own lock
// keeps sweepers off it while the slow delete runs outside the store lock.
func (s *Store) DeleteImage(digest string) error {
	if err := ValidateImageDigest(digest); err != nil {
		return err
	}
	aside := s.displacedImageDir(digest)
	held, err := s.displaceImage(digest, aside)
	if err != nil {
		return err
	}
	defer func() { _ = held.Close() }()
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("failed to remove image %s: %w", digest, err)
	}
	return nil
}

// displacedImageDir gives each commit or removal its own name, including
// concurrent operations in one process or in separate PID namespaces.
func (s *Store) displacedImageDir(digest string) string {
	return filepath.Join(s.rootDir, "images", fmt.Sprintf("%s.old-%d-%s", digest, os.Getpid(), rand.Text()))
}

// displaceImage is the locked part of DeleteImage. Its returned directory
// lock follows the image through the rename and protects the caller's delete.
func (s *Store) displaceImage(digest, aside string) (*StagingLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()

	held, err := lockDir(s.ImageDir(digest), false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrImageNotFound, digest)
		}
		return nil, fmt.Errorf("failed to lock image directory: %w", err)
	}
	if err := os.Rename(s.ImageDir(digest), aside); err != nil {
		_ = held.Close()
		return nil, fmt.Errorf("failed to displace image %s: %w", digest, err)
	}
	return held, nil
}

// CommitImage publishes the image unpacked into stagingDir as digest, by
// swapping directories, never by deleting in place.
//
// RemoveAll on a populated rootfs takes seconds and deletes in readdir order,
// so an interrupt during it could strip the rootfs while leaving image.json --
// exactly the half-state that made every later run fail. Renaming the
// previous image aside is atomic; the slow delete then happens once the new
// image is already published, where an interrupt is harmless: the leftover is
// swept by the next pull or prune once its directory lock is released. If
// publishing fails, the previous image is renamed back.
//
// The previous image is locked before it is renamed aside, and stays locked
// until it is deleted or put back: the lock follows the inode, so a sweeper
// finds the `.old-<pid>-<unique>` held through rollback or deletion. Each
// commit uses a unique name so another commit in the same process can
// publish while this one is still deleting the displaced image.
//
// The swap happens under the store lock, as DeleteImage's does: another
// process reading the image -- a pull deciding whether it is whole, a prune
// deciding whether it is used -- must see it either entirely before or
// entirely after. The displaced image is deleted outside the store lock,
// so the lock is held for two renames and not for seconds.
//
// staging is the pull's lock on stagingDir, and is released the moment the
// rename publishes the directory, still inside the locked section. The lock
// follows the inode, so left to the caller it would hold the published image
// for as long as the caller took to let go -- through the delete of the
// displaced copy, seconds on a real image -- and another pull committing the
// same digest in that window would find the image busy and fail, over a
// directory nothing sweeps.
func (s *Store) CommitImage(digest, stagingDir string, staging *StagingLock) error {
	if err := ValidateImageDigest(digest); err != nil {
		return err
	}
	finalDir := s.ImageDir(digest)
	oldDir := s.displacedImageDir(digest)
	previous, err := s.swapImage(stagingDir, finalDir, oldDir, staging)
	if err != nil {
		return err
	}
	if previous != nil {
		_ = os.RemoveAll(oldDir)
		_ = previous.Close()
	}
	return nil
}

// swapImage is the locked part of CommitImage. It returns the lock on the
// previous image when one was displaced to oldDir, and nil when there was
// none.
func (s *Store) swapImage(stagingDir, finalDir, oldDir string, staging *StagingLock) (*StagingLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()

	var previous *StagingLock
	if _, err := os.Stat(finalDir); err == nil {
		previous, err = lockDir(finalDir, false)
		if err != nil {
			return nil, fmt.Errorf("failed to displace the previous image: %w", err)
		}
		if err := os.Rename(finalDir, oldDir); err != nil {
			_ = previous.Close()
			return nil, fmt.Errorf("failed to displace the previous image: %w", err)
		}
	}
	if err := os.Rename(stagingDir, finalDir); err != nil {
		if previous != nil {
			_ = os.Rename(oldDir, finalDir) // put the usable image back
			_ = previous.Close()
		}
		return nil, fmt.Errorf("failed to commit image: %w", err)
	}
	_ = staging.Close()
	return previous, nil
}

// StagingDirs lists the pull staging directories in images/.
//
// These are the `<digest>.tmp-<pid>` a pull unpacks into and the
// `<digest>.old-<pid>-<unique>` it displaces a previous image to. Both are
// swept at the start of the next pull, which may never come; prune is the
// other sweeper.
// Either may be in use: see StagingInFlight.
func (s *Store) StagingDirs() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(filepath.Join(s.rootDir, "images"))
	if err != nil {
		return nil, fmt.Errorf("failed to list images directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && isStagingDir(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// RemoveStagingDir removes one pull staging directory by name, unless a pull
// is using it, which is ErrStagingInUse. It reports whether this call removed
// it: false with no error means it was already gone.
//
// Separate from DeleteImage because a staging name is not a valid digest, and
// because the two mean different things to whoever is watching: one is an image
// somebody pulled, the other is scaffolding from a pull that did not finish.
//
// The directory's own lock is taken without waiting and held through the
// delete: a pull holds it for as long as it uses the directory, and the
// delete happening under the lock is what keeps a pull from adopting the
// name half-way through, and the other sweeper from deleting beside this
// one. The store lock is taken for the acquisition only, so a commit or an
// rmi mid-rename is not seen half-way, and released before the delete: a
// RemoveAll of a whole rootfs takes seconds, every other process's instance
// records go through that lock, and the directory lock is what excludes the
// others from this tree.
//
// A directory that is already gone is not an error. Prune lists, then
// removes, and a pull's sweep can take the leftover in between; the outcome
// prune wanted is there either way, and the report says which call got it.
func (s *Store) RemoveStagingDir(name string) (bool, error) {
	if name != filepath.Base(name) || !isStagingDir(name) {
		return false, fmt.Errorf("%q is not a pull staging directory", name)
	}
	dir := filepath.Join(s.rootDir, "images", name)
	l, err := s.lockStagingForRemoval(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = l.Close() }()
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("failed to remove staging directory %s: %w", name, err)
	}
	return true, nil
}

// lockStagingForRemoval takes a staging directory's lock without waiting,
// under the store lock, and hands it back with the store lock released.
func (s *Store) lockStagingForRemoval(dir string) (*StagingLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return lockDir(dir, false)
}

// ImageDirNames lists every entry in images/ that is not pull scaffolding,
// whether or not its metadata can be read.
//
// ListImages answers "what has this store got?", and skips an entry whose
// image.json is missing or corrupt. Such an entry is invisible to every
// command and still occupies the disk, so reclaiming space has to start from
// the directory listing rather than from the records.
func (s *Store) ImageDirNames() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(filepath.Join(s.rootDir, "images"))
	if err != nil {
		return nil, fmt.Errorf("failed to list images directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && !isStagingDir(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// FindImages returns every stored image the reference names.
//
// A reference can name more than one stored image, and this returns all of
// them rather than picking one. Pulling a republished tag adds a digest
// without retiring the old one, and pulling two platforms of a tag keeps both,
// so one tag routinely answers two or more image directories. `hull run`
// resolves that to the newest complete entry because it has to boot exactly
// one; a caller removing images wants the set.
//
// The reference is matched as ImageMetadata.Answers defines it: the reference
// as written, a store key, or a digest pin checked against both digests the
// record knows. A reference that matches nothing that way is then tried as a
// digest prefix, because the DIGEST column of `hull images` is truncated to 19
// characters and that is what a user copies out of it.
//
// byPrefix says the answer came from that fallback. A caller acting on the
// result has to treat several matches differently in the two cases: a tag
// naming three images names all three, while a digest prefix naming three names
// one of them and cannot say which.
func (s *Store) FindImages(ref string) (matches []*ImageMetadata, byPrefix bool, err error) {
	images, err := s.ListImages()
	if err != nil {
		return nil, false, err
	}

	want := ParseReference(ref)
	for _, img := range images {
		if img.Answers(want) {
			matches = append(matches, img)
		}
	}
	if len(matches) > 0 || !IsDigestPrefix(ref) {
		return matches, false, nil
	}

	// A bare index digest names every platform record that resolved through
	// it, whole. ParseReference returns early for a bare `sha256:` string with
	// Digest empty, so Answers never reaches its IndexDigest comparison and
	// the reference fell through to the prefix pass below. There it matched
	// each platform record through the shared index digest and was refused as
	// ambiguous, listing digests that do not start with what was typed and
	// leaving no way to remove by that identifier.
	for _, img := range images {
		if img.IndexDigest != "" && img.IndexDigest == ref {
			matches = append(matches, img)
		}
	}
	if len(matches) > 0 {
		return matches, false, nil
	}

	for _, img := range images {
		if img.HasDigestPrefix(ref) {
			matches = append(matches, img)
		}
	}
	return matches, true, nil
}
