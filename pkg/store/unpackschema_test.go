// Copyright (c) 2023-2026, Nubificus LTD
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
	"os"
	"path/filepath"
	"testing"
)

// The reported bug, reduced. A user's store held a rootfs unpacked before hull
// recorded ownership: usr/bin/sudo was a plain 0755 file owned by the host
// user, with no guest-attr record anywhere in the tree. Nothing about that
// directory looks wrong -- image.json is there, rootfs is a populated
// directory -- so every later run served it happily and sudo never worked.
// A complete-looking image from an older layout must be a cache miss.
func TestOlderLayoutIsNotAcachehit(t *testing.T) {
	s := newTestStore(t)
	imageDir := saveTestImage(t, s, "example.com/img:tag")
	if err := os.MkdirAll(filepath.Join(imageDir, "rootfs", "usr", "bin"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Exactly what the stale store contained: present, readable, and wrong.
	if err := os.WriteFile(filepath.Join(imageDir, "rootfs", "usr", "bin", "sudo"), []byte("x"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := ReadUnpackSchema(imageDir); got != 1 {
		t.Errorf("an unstamped image should read as schema 1, got %d", got)
	}
	if s.ImageComplete(testDigest) {
		t.Fatal("a rootfs unpacked by an older hull must not count as cached: " +
			"it is missing the ownership records and setuid bits the guest needs")
	}

	// And once it has been rewritten by a current unpack, it is a hit again.
	if err := WriteUnpackSchema(imageDir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
	if !s.ImageComplete(testDigest) {
		t.Error("an image stamped with the current schema should be cached")
	}
}

// A stamp that is not the current version -- from a future hull, or damaged --
// is not a hit either. Only an exact match is.
func TestOnlyTheCurrentSchemaIsAHit(t *testing.T) {
	for _, stamp := range []string{"1", "0", "99", "", "garbage", "2 3"} {
		t.Run("stamp="+stamp, func(t *testing.T) {
			s := newTestStore(t)
			imageDir := saveTestImage(t, s, "example.com/img:tag")
			if err := os.MkdirAll(filepath.Join(imageDir, "rootfs"), 0755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(filepath.Join(imageDir, unpackSchemaFile), []byte(stamp), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if s.ImageComplete(testDigest) {
				t.Errorf("schema stamp %q must not count as the current layout %d", stamp, UnpackSchema)
			}
		})
	}
}

// The stamp round-trips, including the trailing newline it is written with.
func TestUnpackSchemaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteUnpackSchema(dir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
	if got := ReadUnpackSchema(dir); got != UnpackSchema {
		t.Errorf("ReadUnpackSchema = %d, want %d", got, UnpackSchema)
	}
	fi, err := os.Stat(filepath.Join(dir, unpackSchemaFile))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("stamp mode = %04o, want 0600", perm)
	}
}
