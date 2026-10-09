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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// otherDigest is a second store key, for the cases that need two images.
const otherDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func seedImage(t *testing.T, s *Store, digest string, meta *ImageMetadata) {
	t.Helper()
	meta.Digest = digest
	if meta.PulledAt.IsZero() {
		meta.PulledAt = time.Now()
	}
	dir, err := s.SaveImage(digest, meta)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll rootfs: %v", err)
	}
	if err := WriteUnpackSchema(dir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
}

func TestDeleteImageRemovesTheDirectory(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	if err := s.DeleteImage(testDigest); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if _, err := os.Stat(s.ImageDir(testDigest)); !os.IsNotExist(err) {
		t.Fatalf("image directory is still there: %v", err)
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("the store still lists %d image(s)", len(images))
	}
}

// The delete must leave nothing behind that a later lookup could mistake for a
// cached image, including the directory it renames aside on the way out.
func TestDeleteImageLeavesNoStagingLeftover(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	if err := s.DeleteImage(testDigest); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	staging, err := s.StagingDirs()
	if err != nil {
		t.Fatalf("StagingDirs: %v", err)
	}
	if len(staging) != 0 {
		t.Fatalf("delete left staging directories behind: %v", staging)
	}
}

func TestDeleteImageUnknownDigest(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteImage(testDigest); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("error = %v, want ErrImageNotFound", err)
	}
}

// DeleteImage recurses, so it must never be handed a path that leaves the
// store. The digest reaches it from a user-supplied reference.
func TestDeleteImageRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	victim := filepath.Join(s.RootDir(), "instances")
	for _, bad := range []string{
		"../instances",
		"sha256:abc/../../../etc",
		"..",
		"",
		testDigest + ".old-1",
	} {
		if err := s.DeleteImage(bad); !errors.Is(err, ErrInvalidImageDigest) {
			t.Errorf("DeleteImage(%q) = %v, want ErrInvalidImageDigest", bad, err)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a rejected digest still removed %s: %v", victim, err)
	}
}

func TestRemoveStagingDirRejectsAnythingElse(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	for _, bad := range []string{testDigest, "../instances", "images"} {
		if _, err := s.RemoveStagingDir(bad); err == nil {
			t.Errorf("RemoveStagingDir(%q) was accepted", bad)
		}
	}
	if _, err := os.Stat(s.ImageDir(testDigest)); err != nil {
		t.Fatalf("the image was removed by a rejected name: %v", err)
	}
}

func TestStagingDirsAndRemoval(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})
	for _, suffix := range []string{".tmp-4242", ".old-4242"} {
		if err := os.MkdirAll(filepath.Join(s.RootDir(), "images", testDigest+suffix), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	staging, err := s.StagingDirs()
	if err != nil {
		t.Fatalf("StagingDirs: %v", err)
	}
	if len(staging) != 2 {
		t.Fatalf("StagingDirs = %v, want the two staging entries", staging)
	}
	for _, name := range staging {
		if _, err := s.RemoveStagingDir(name); err != nil {
			t.Fatalf("RemoveStagingDir(%q): %v", name, err)
		}
	}
	if _, err := os.Stat(s.ImageDir(testDigest)); err != nil {
		t.Fatalf("sweeping the staging directories removed the image: %v", err)
	}
}

// An image whose metadata cannot be read is invisible to ListImages and still
// occupies the disk. Reclaiming it has to start from the directory listing.
func TestImageDirNamesSeesAnUnreadableRecord(t *testing.T) {
	s := newTestStore(t)
	dir := s.ImageDir(testDigest)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(s.RootDir(), "images", testDigest+".tmp-1"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("ListImages returned %d record(s) for a corrupt image.json", len(images))
	}
	names, err := s.ImageDirNames()
	if err != nil {
		t.Fatalf("ImageDirNames: %v", err)
	}
	if len(names) != 1 || names[0] != testDigest {
		t.Fatalf("ImageDirNames = %v, want just the image directory", names)
	}
}

func TestFindImagesByRefDigestAndPrefix(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	for _, lookup := range []struct{ name, ref string }{
		{"by ref", "ghcr.io/x/y:v1"},
		{"by digest", testDigest},
		{"by digest pin", "ghcr.io/x/y@" + testDigest},
		{"by truncated digest", testDigest[:19]},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			matches, byPrefix, err := s.FindImages(lookup.ref)
			if err != nil {
				t.Fatalf("FindImages: %v", err)
			}
			if len(matches) != 1 || matches[0].Digest != testDigest {
				t.Fatalf("FindImages(%q) = %v", lookup.ref, matches)
			}
			if want := lookup.name == "by truncated digest"; byPrefix != want {
				t.Errorf("byPrefix = %v, want %v", byPrefix, want)
			}
		})
	}
}

// A tag naming several stored images names all of them: that is what a
// republished tag leaves behind. A digest prefix naming several names one of
// them and cannot say which, which is why the two are reported differently.
func TestFindImagesReportsHowItMatched(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})
	seedImage(t, s, otherDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	matches, byPrefix, err := s.FindImages("ghcr.io/x/y:v1")
	if err != nil {
		t.Fatalf("FindImages: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("a republished tag should answer both digests, got %d", len(matches))
	}
	if byPrefix {
		t.Error("a tag match must not be reported as a prefix match")
	}

	matches, byPrefix, err = s.FindImages("sha256:1111111")
	if err != nil {
		t.Fatalf("FindImages: %v", err)
	}
	if len(matches) != 1 || !byPrefix {
		t.Fatalf("prefix lookup = %v (byPrefix %v)", matches, byPrefix)
	}
}

func TestFindImagesUnknownRef(t *testing.T) {
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1"})

	for _, ref := range []string{"ghcr.io/x/y:v2", "sha256:deadbeef", "y"} {
		matches, _, err := s.FindImages(ref)
		if err != nil {
			t.Fatalf("FindImages(%q): %v", ref, err)
		}
		if len(matches) != 0 {
			t.Errorf("FindImages(%q) matched %v", ref, matches)
		}
	}
}

func TestIsDigestPrefix(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{testDigest, true},
		{testDigest[:19], true},
		{"sha256:abcdef", true},
		{"sha256:abcde", false},   // too short to be worth guessing at
		{"sha256:ABCDEF", false},  // a digest is lower-case hex
		{"ghcr.io/x/y:v1", false}, // a tag, not a digest
		{"", false},
	} {
		if got := IsDigestPrefix(tc.ref); got != tc.want {
			t.Errorf("IsDigestPrefix(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

// The index digest never keys a directory, but it is what a user pinning a
// multi-arch image sees, so a prefix of it has to resolve to the record that
// recorded it.
func TestHasDigestPrefixCoversTheIndexDigest(t *testing.T) {
	m := &ImageMetadata{Digest: testDigest, IndexDigest: otherDigest}
	if !m.HasDigestPrefix(otherDigest[:19]) {
		t.Error("an index digest prefix should match")
	}
	if m.HasDigestPrefix("sha256:9999999") {
		t.Error("an unrelated prefix matched")
	}
}

func TestValidateImageDigest(t *testing.T) {
	if err := ValidateImageDigest(testDigest); err != nil {
		t.Fatalf("a real digest was rejected: %v", err)
	}
	for _, bad := range []string{"", "sha256:", "sha256:xyz", testDigest + "/rootfs", "../x"} {
		err := ValidateImageDigest(bad)
		if !errors.Is(err, ErrInvalidImageDigest) {
			t.Errorf("ValidateImageDigest(%q) = %v", bad, err)
		}
		if err != nil && !strings.Contains(err.Error(), "digest") {
			t.Errorf("error for %q does not say what is wrong: %v", bad, err)
		}
	}
}

// A bare index digest names every platform record that resolved through it.
// ParseReference returns early for a bare digest with Digest empty, so Answers
// never reaches its IndexDigest comparison and the reference used to fall
// through to the prefix pass, where the caller refused it as ambiguous.
func TestFindImagesMatchesABareIndexDigestWhole(t *testing.T) {
	const index = "sha256:28bd5fe8aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1", IndexDigest: index})
	seedImage(t, s, otherDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1", IndexDigest: index})

	matches, byPrefix, err := s.FindImages(index)
	if err != nil {
		t.Fatalf("FindImages: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("an index digest answered %d record(s), want both platforms", len(matches))
	}
	if byPrefix {
		t.Error("a whole index digest was reported as a prefix match")
	}
}

// A prefix of that same index digest is still a prefix match, so a caller that
// refuses ambiguity keeps refusing it.
func TestFindImagesStillTreatsAShortIndexDigestAsAPrefix(t *testing.T) {
	const index = "sha256:28bd5fe8aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := newTestStore(t)
	seedImage(t, s, testDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1", IndexDigest: index})
	seedImage(t, s, otherDigest, &ImageMetadata{Ref: "ghcr.io/x/y:v1", IndexDigest: index})

	matches, byPrefix, err := s.FindImages(index[:19])
	if err != nil {
		t.Fatalf("FindImages: %v", err)
	}
	if len(matches) != 2 || !byPrefix {
		t.Fatalf("short index digest: %d match(es), byPrefix %v", len(matches), byPrefix)
	}
}

// A commit publishes by rename: the previous image is displaced, the staging
// directory takes its place, and the displaced copy is gone afterwards. When
// the staging directory cannot be renamed in, the previous image is put back
// and nothing is lost.
func TestCommitImageSwapsAndRollsBack(t *testing.T) {
	s := newTestStore(t)
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	if err := os.WriteFile(filepath.Join(imageDir, "previous"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	staging := filepath.Join(s.RootDir(), "images", testDigest+".tmp-"+strconv.Itoa(os.Getpid()))
	held, err := CreateStaging(staging)
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A legacy leftover from this pid must not collide with the new
	// displacement name. The ordinary staging sweep will collect it.
	leftover := filepath.Join(s.RootDir(), "images", testDigest+".old-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(filepath.Join(leftover, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := s.CommitImage(testDigest, staging, held); err != nil {
		t.Fatalf("CommitImage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imageDir, "rootfs")); err != nil {
		t.Errorf("the staging directory was not published: %v", err)
	}
	// The pull's lock followed the inode into the published image, and the
	// commit let go of it there: another commit of the same digest must find
	// the image free to displace, not busy.
	if l, err := lockDir(imageDir, false); err != nil {
		t.Errorf("the published image is still locked after the commit: %v", err)
	} else {
		_ = l.Close()
	}
	if _, err := os.Stat(filepath.Join(imageDir, "previous")); !os.IsNotExist(err) {
		t.Errorf("the previous image is still in place: %v", err)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("the commit touched an unrelated leftover: %v", err)
	}
	if _, err := s.SweepStaging(); err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}

	// Rollback: nothing to rename in, so the image that was there comes back.
	if err := s.CommitImage(testDigest, staging, nil); err == nil {
		t.Fatal("CommitImage with no staging directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(imageDir, "rootfs")); err != nil {
		t.Errorf("the previous image was not put back after a failed commit: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("a failed commit left the previous image displaced: %v", err)
	}
	if err := s.CommitImage("not a digest", staging, nil); err == nil {
		t.Error("CommitImage accepted a name that is not a digest")
	}
}

// A live operation with the same pid must not prevent another publication
// or removal. Legacy names can still exist during an upgrade.
func TestImageOperationsLeaveLockedLegacyLeftoverAlone(t *testing.T) {
	for _, operation := range []string{"commit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s := newTestStore(t)
			saveTestImage(t, s, "example.com/img:tag")
			name := testDigest + ".old-" + strconv.Itoa(os.Getpid())
			leftover := plantStaging(t, s, name)
			holdStaging(t, leftover)
			var err error
			if operation == "commit" {
				staging := plantStaging(t, s, testDigest+".tmp-1")
				err = s.CommitImage(testDigest, staging, holdStaging(t, staging))
			} else {
				err = s.DeleteImage(testDigest)
			}
			if err != nil {
				t.Fatalf("%s: %v", operation, err)
			}
			if _, err := os.Stat(filepath.Join(leftover, "rootfs", "layer")); err != nil {
				t.Fatalf("%s touched the live leftover: %v", operation, err)
			}
		})
	}
}

// Pause after the rmi rename, before its recursive delete. Prune must see
// the directory as busy, while other store users and commits can proceed.
func TestDisplacedImageRemovalHoldsOnlyDirectoryLock(t *testing.T) {
	s := newTestStore(t)
	saveTestImage(t, s, "example.com/img:tag")
	aside := s.displacedImageDir(testDigest)
	held, err := s.displaceImage(testDigest, aside)
	if err != nil {
		t.Fatalf("displaceImage: %v", err)
	}
	defer func() { _ = held.Close() }()
	name := filepath.Base(aside)
	if !s.StagingInFlight(name) {
		t.Fatal("an active image removal is listed as abandoned")
	}
	if !s.mu.TryLock() {
		t.Fatal("image removal still holds the store mutex")
	}
	s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.RootDir(), ".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("image removal still holds the store flock: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if removed, err := s.RemoveStagingDir(name); removed || !errors.Is(err, ErrStagingInUse) {
		t.Fatalf("RemoveStagingDir = %v, %v; want false, ErrStagingInUse", removed, err)
	}
	staging := plantStaging(t, s, testDigest+".tmp-1")
	if err := s.CommitImage(testDigest, staging, holdStaging(t, staging)); err != nil {
		t.Fatalf("commit during image removal: %v", err)
	}
	if err := s.DeleteImage(testDigest); err != nil {
		t.Fatalf("second removal: %v", err)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("another operation touched the first removal: %v", err)
	}
	_ = held.Close()
	if removed, err := s.RemoveStagingDir(name); err != nil || !removed {
		t.Fatalf("cleanup after release = %v, %v", removed, err)
	}
}

func TestConcurrentCommitsInOneProcess(t *testing.T) {
	s := newTestStore(t)
	saveTestImage(t, s, "example.com/img:tag")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := range 2 {
		wg.Go(func() {
			<-start
			for iteration := range 25 {
				name := fmt.Sprintf("%s.tmp-%d-%d", testDigest, worker, iteration)
				staging := plantStaging(t, s, name)
				if err := s.CommitImage(testDigest, staging, holdStaging(t, staging)); err != nil {
					t.Errorf("CommitImage: %v", err)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	if _, err := os.Stat(filepath.Join(s.ImageDir(testDigest), "rootfs", "layer")); err != nil {
		t.Fatalf("published image missing: %v", err)
	}
	if names, err := s.StagingDirs(); err != nil || len(names) != 0 {
		t.Fatalf("commits left staging directories: %v, %v", names, err)
	}
}
