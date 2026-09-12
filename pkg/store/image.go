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
// rename is atomic, and a leftover `.old-<pid>` never satisfies a lookup,
// never appears in a listing, and is swept by the next pull or prune.
func (s *Store) DeleteImage(digest string) error {
	if err := ValidateImageDigest(digest); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	imageDir := s.ImageDir(digest)
	if _, err := os.Stat(imageDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrImageNotFound, digest)
		}
		return fmt.Errorf("failed to read image directory: %w", err)
	}

	aside := filepath.Join(s.rootDir, "images", fmt.Sprintf("%s.old-%d", digest, os.Getpid()))
	if err := os.Rename(imageDir, aside); err != nil {
		return fmt.Errorf("failed to displace image %s: %w", digest, err)
	}
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("failed to remove image %s: %w", digest, err)
	}
	return nil
}

// StagingDirs lists the pull staging directories in images/.
//
// These are the `<digest>.tmp-<pid>` a pull unpacks into and the
// `<digest>.old-<pid>` it displaces a previous image to. Both are swept at the
// start of the next pull, which may never come; prune is the other sweeper.
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

// RemoveStagingDir removes one pull staging directory by name.
//
// Separate from DeleteImage because a staging name is not a valid digest, and
// because the two mean different things to whoever is watching: one is an image
// somebody pulled, the other is scaffolding from a pull that did not finish.
func (s *Store) RemoveStagingDir(name string) error {
	if name != filepath.Base(name) || !isStagingDir(name) {
		return fmt.Errorf("%q is not a pull staging directory", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// The store lock, as DeleteImage takes it: the in-process mutex covers one
	// process, and this removes a directory another process may be committing
	// a pull through.
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	if err := os.RemoveAll(filepath.Join(s.rootDir, "images", name)); err != nil {
		return fmt.Errorf("failed to remove staging directory %s: %w", name, err)
	}
	return nil
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
