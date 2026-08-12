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

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testDigest = "sha256:c408baae42f5c74c0661fbc20a289fd23d4322988e52c88cd54108e5c4c74893"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func saveTestImage(t *testing.T, s *Store, ref string) string {
	t.Helper()
	dir, err := s.SaveImage(testDigest, &ImageMetadata{
		Ref:      ref,
		Digest:   testDigest,
		PulledAt: time.Now(),
		Size:     42,
	})
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	return dir
}

func TestSaveAndGetImage(t *testing.T) {
	s := newTestStore(t)
	saveTestImage(t, s, "example.com/img:tag")

	got, err := s.GetImage(testDigest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if got.Ref != "example.com/img:tag" || got.Digest != testDigest || got.Size != 42 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// An interrupted pull can leave image.json behind with no rootfs. Callers that
// trusted the metadata alone resolved the image as cached and then failed on
// every run with "failed to copy rootfs", recoverable only by deleting the
// whole store by hand. ImageComplete is what stops that.
func TestImageComplete(t *testing.T) {
	tests := []struct {
		name     string
		metadata bool
		rootfs   bool
		want     bool
	}{
		{"metadata and rootfs", true, true, true},
		{"metadata without rootfs", true, false, false},
		{"rootfs without metadata", false, true, false},
		{"neither", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			imageDir := filepath.Join(s.RootDir(), "images", testDigest)

			if tt.metadata {
				saveTestImage(t, s, "example.com/img:tag")
			} else if err := os.MkdirAll(imageDir, 0700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if tt.rootfs {
				if err := os.MkdirAll(filepath.Join(imageDir, "rootfs"), 0755); err != nil {
					t.Fatalf("MkdirAll rootfs: %v", err)
				}
			}

			if got := s.ImageComplete(testDigest); got != tt.want {
				t.Errorf("ImageComplete = %v, want %v", got, tt.want)
			}
		})
	}
}

// The exact reproduction of the reported bug: a good image whose rootfs is
// removed (as an interrupt during the pre-rename RemoveAll used to do) must
// stop counting as cached, so the next run re-pulls instead of failing forever.
func TestImageCompleteAfterRootfsLoss(t *testing.T) {
	s := newTestStore(t)
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	if err := os.MkdirAll(filepath.Join(imageDir, "rootfs"), 0755); err != nil {
		t.Fatalf("MkdirAll rootfs: %v", err)
	}
	if !s.ImageComplete(testDigest) {
		t.Fatal("freshly pulled image should be complete")
	}

	if err := os.RemoveAll(filepath.Join(imageDir, "rootfs")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if s.ImageComplete(testDigest) {
		t.Error("image with no rootfs must not count as complete")
	}
	// The metadata is deliberately still readable; it is ImageComplete, not
	// GetImage, that callers must consult.
	if _, err := s.GetImage(testDigest); err != nil {
		t.Errorf("GetImage should still succeed: %v", err)
	}
}

// A rootfs that is a file rather than a directory is corruption, not a cache
// hit.
func TestImageCompleteRejectsRootfsFile(t *testing.T) {
	s := newTestStore(t)
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	if err := os.WriteFile(filepath.Join(imageDir, "rootfs"), []byte("not a dir"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if s.ImageComplete(testDigest) {
		t.Error("a rootfs file must not count as complete")
	}
}

func TestWriteImageMetadataIsReadableByGetImage(t *testing.T) {
	s := newTestStore(t)
	// Mimic the pull path: metadata written into a staging dir that is then
	// renamed into place, so rootfs and metadata appear together.
	staging := filepath.Join(s.RootDir(), "images", testDigest+".tmp-1")
	if err := os.MkdirAll(filepath.Join(staging, "rootfs"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := WriteImageMetadata(staging, &ImageMetadata{Ref: "r", Digest: testDigest}); err != nil {
		t.Fatalf("WriteImageMetadata: %v", err)
	}
	if err := os.Rename(staging, filepath.Join(s.RootDir(), "images", testDigest)); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if !s.ImageComplete(testDigest) {
		t.Error("image published by rename should be complete")
	}
	got, err := s.GetImage(testDigest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if got.Ref != "r" {
		t.Errorf("Ref = %q, want %q", got.Ref, "r")
	}
}

func TestListImagesIgnoresStagingDirs(t *testing.T) {
	s := newTestStore(t)
	saveTestImage(t, s, "example.com/img:tag")
	// Leftovers from an interrupted pull must not surface as images. They now
	// carry an image.json too, since the pull path stages metadata alongside
	// the rootfs, so the directory name is the only thing telling them apart.
	for _, name := range []string{testDigest + ".tmp-123", testDigest + ".old-123"} {
		dir := filepath.Join(s.RootDir(), "images", name)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := WriteImageMetadata(dir, &ImageMetadata{Ref: "staging", Digest: testDigest}); err != nil {
			t.Fatalf("WriteImageMetadata: %v", err)
		}
	}

	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("ListImages returned %d images, want 1", len(images))
	}
}

func TestInstanceStateExitStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("job1"); err != nil {
		t.Fatal(err)
	}
	code := 7
	exited := time.Now().Truncate(time.Second)
	if err := s.SaveInstance(&InstanceState{
		ID: "job1", Status: "stopped", ExitCode: &code, ExitedAt: exited,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstance("job1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Errorf("ExitCode = %v, want 7", got.ExitCode)
	}
	if !got.ExitedAt.Equal(exited) {
		t.Errorf("ExitedAt = %v, want %v", got.ExitedAt, exited)
	}

	// A record written without the fields (the pre-ADR-0007 shape) must
	// load with a nil code: "ended without a reportable status".
	if err := s.SaveInstance(&InstanceState{ID: "job1", Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	plain, err := s.GetInstance("job1")
	if err != nil {
		t.Fatal(err)
	}
	if plain.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil for a record without a status", plain.ExitCode)
	}
}
