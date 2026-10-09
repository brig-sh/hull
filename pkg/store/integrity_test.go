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
	"reflect"
	"strings"
	"testing"
)

// publishTestRootfs writes a small rootfs into the test image and records its
// entry count, the way a pull publishes one.
func publishTestRootfs(t *testing.T, s *Store) string {
	t.Helper()
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	for _, p := range []string{"usr/bin/bash", "usr/bin/sh", "etc/passwd"} {
		full := filepath.Join(imageDir, "rootfs", p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if err := WriteUnpackSchema(imageDir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
	if err := WriteRootfsEntries(imageDir); err != nil {
		t.Fatalf("WriteRootfsEntries: %v", err)
	}
	return imageDir
}

// The second half of #114: a pull whose staging directory was deleted under
// it published an image with part of its tree missing, and every later lookup
// found it complete. The recorded count is what tells the two apart.
func TestVerifyImageDetectsAMissingEntry(t *testing.T) {
	s := newTestStore(t)
	imageDir := publishTestRootfs(t, s)

	if n, err := CountRootfsEntries(imageDir); err != nil || n != 6 {
		t.Fatalf("CountRootfsEntries = %d, %v; want 6 (usr, usr/bin, two binaries, etc, passwd)", n, err)
	}
	if err := s.VerifyImage(testDigest); err != nil {
		t.Fatalf("a whole image failed verification: %v", err)
	}

	if err := os.Remove(filepath.Join(imageDir, "rootfs", "usr", "bin", "bash")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !s.ImageComplete(testDigest) {
		t.Fatal("precondition: the truncated image still looks complete, which is the bug")
	}
	err := s.VerifyImage(testDigest)
	if !errors.Is(err, ErrImageDamaged) {
		t.Fatalf("VerifyImage on a truncated rootfs = %v, want ErrImageDamaged", err)
	}
}

// An image from before the count existed carries none, and must keep passing:
// failing it would re-unpack every image in every existing store on upgrade.
func TestVerifyImageTrustsAnUnrecordedImage(t *testing.T) {
	s := newTestStore(t)
	imageDir := publishTestRootfs(t, s)
	if err := os.Remove(filepath.Join(imageDir, rootfsEntriesFile)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.Remove(filepath.Join(imageDir, "rootfs", "etc", "passwd")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.VerifyImage(testDigest); err != nil {
		t.Errorf("an image with no recorded count was refused: %v", err)
	}
}

// A count that cannot be read is not a count of zero, and not a pass either.
func TestVerifyImageRefusesADamagedCount(t *testing.T) {
	s := newTestStore(t)
	imageDir := publishTestRootfs(t, s)
	for _, stamp := range []string{"", "garbage", "-1", "3 4"} {
		if err := os.WriteFile(filepath.Join(imageDir, rootfsEntriesFile), []byte(stamp), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := s.VerifyImage(testDigest); !errors.Is(err, ErrImageDamaged) {
			t.Errorf("stamp %q: VerifyImage = %v, want ErrImageDamaged", stamp, err)
		}
	}
}

// An image is free to ship a directory its owner cannot list (unpack applies
// the image's own modes), and the count must neither fail the pull over it
// nor disagree with itself later.
func TestCountRootfsEntriesSkipsAnUnlistableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists every directory, so there is nothing to skip")
	}
	s := newTestStore(t)
	imageDir := publishTestRootfs(t, s)
	locked := filepath.Join(imageDir, "rootfs", "etc", "private")
	if err := os.MkdirAll(locked, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "key"), []byte("x"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0755) })

	// The directory counts; what it hides does not. The other six are the
	// tree publishTestRootfs wrote.
	n, err := CountRootfsEntries(imageDir)
	if err != nil {
		t.Fatalf("CountRootfsEntries: %v", err)
	}
	if n != 7 {
		t.Errorf("CountRootfsEntries = %d, want 7", n)
	}
	if err := WriteRootfsEntries(imageDir); err != nil {
		t.Fatalf("WriteRootfsEntries: %v", err)
	}
	if err := s.VerifyImage(testDigest); err != nil {
		t.Errorf("an image with an unlistable directory failed verification: %v", err)
	}
}

// Only a rootfs that is not there is an error.
func TestCountRootfsEntriesFailsWithoutARootfs(t *testing.T) {
	s := newTestStore(t)
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	if _, err := CountRootfsEntries(imageDir); err == nil {
		t.Error("CountRootfsEntries on a missing rootfs returned no error")
	}
}

// An entry added beside the image's own is not a truncation. A Finder window
// leaves a .DS_Store in any directory it shows, and that must not cost a
// re-unpack of the whole image.
func TestVerifyImageToleratesAnExtraEntry(t *testing.T) {
	s := newTestStore(t)
	imageDir := publishTestRootfs(t, s)
	if err := os.WriteFile(filepath.Join(imageDir, "rootfs", ".DS_Store"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.VerifyImage(testDigest); err != nil {
		t.Errorf("an extra entry was read as damage: %v", err)
	}
}

func TestImageHolders(t *testing.T) {
	s := newTestStore(t)
	for _, inst := range []struct{ id, digest, status string }{
		{"b", testDigest, "running"}, {"a", testDigest, "stopped"}, {"other", "sha256:" + strings.Repeat("0", 64), "running"},
	} {
		if _, err := s.CreateInstance(inst.id); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
		if err := s.SaveInstance(&InstanceState{ID: inst.id, ImageDigest: inst.digest, Status: inst.status}); err != nil {
			t.Fatalf("SaveInstance: %v", err)
		}
	}
	holders, err := s.ImageHolders(testDigest)
	if err != nil {
		t.Fatalf("ImageHolders: %v", err)
	}
	var got []string
	for _, h := range holders {
		got = append(got, h.ID+"="+h.Status)
	}
	if want := []string{"a=stopped", "b=running"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImageHolders = %v, want %v", got, want)
	}
}
