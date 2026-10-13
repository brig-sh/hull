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

package ociclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// An unprivileged Linux unpack cannot chown, so the image's ownership has to
// survive beside the file until the block image is built from it.
func TestGuestAttrRoundTripsOnLinux(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shadow")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := recordGuestAttr(p, 0o100640, 0, 42, false)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("%s has no user xattr support", dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	mode, uid, gid, ok := GuestAttr(p, false)
	if !ok || mode != 0o100640 || uid != 0 || gid != 42 {
		t.Fatalf("GuestAttr = %o %d %d %v, want 100640 0 42 true", mode, uid, gid, ok)
	}

	// No record is the root-unpack case: the caller reads the file itself.
	bare := filepath.Join(dir, "bare")
	if err := os.WriteFile(bare, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := GuestAttr(bare, false); ok {
		t.Error("a file with no record reported one")
	}

	// Linux refuses user.* on a symlink; recording one must not fail the
	// unpack, and reading one reports no record.
	link := filepath.Join(dir, "link")
	if err := os.Symlink("shadow", link); err != nil {
		t.Fatal(err)
	}
	if err := recordGuestAttr(link, 0o120777, 0, 0, true); err != nil {
		t.Errorf("recording a symlink failed: %v", err)
	}
	if _, _, _, ok := GuestAttr(link, true); ok {
		t.Error("a symlink reported a record")
	}
}

func TestRecordsOwnershipOnlyWhenNotRoot(t *testing.T) {
	if recordsOwnership != (os.Geteuid() != 0) {
		t.Fatalf("recordsOwnership = %v for euid %d", recordsOwnership, os.Geteuid())
	}
}
