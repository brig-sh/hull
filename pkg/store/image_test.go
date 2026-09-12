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
	"os"
	"path/filepath"
	"strings"
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
		if err := s.RemoveStagingDir(bad); err == nil {
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
		if err := s.RemoveStagingDir(name); err != nil {
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
