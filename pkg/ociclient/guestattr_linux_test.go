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
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// An unprivileged unpack keeps what mke2fs and rmi need on the host -- owner
// read on files, owner rwx on directories -- while the record keeps the mode
// the image asked for, which is what the guest gets. Fedora's /etc/shadow is
// 0000 and its sudo 04111; put on the host as is, neither can be read by the
// user who owns it.
func TestUnprivilegedUnpackKeepsFilesReadableAndRecordsTheImageMode(t *testing.T) {
	if !recordsOwnership {
		t.Skip("a root unpack keeps the image's modes on the host")
	}
	rootfs := t.TempDir()
	layer := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{hdr: tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o000, Gid: 42}, body: "root:!::0:::::\n"},
		tarEntry{hdr: tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4111}, body: "#!/bin/sh\n"},
		tarEntry{hdr: tar.Header{Name: "usr/lib/sealed/", Typeflag: tar.TypeDir, Mode: 0o555}},
		tarEntry{hdr: tar.Header{Name: "usr/lib/sealed/f", Typeflag: tar.TypeReg, Mode: 0o444}, body: "x"},
	)
	if err := extractTarIgnoreChown(layer, rootfs); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		hostPerm os.FileMode
		recorded uint32
	}{
		{"etc/shadow", 0o400, 0o000},
		{"usr/bin/sudo", 0o511 | os.ModeSetuid, 0o4111},
		{"usr/lib/sealed", 0o755, 0o555},
	} {
		p := filepath.Join(rootfs, tc.name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode() & (os.ModePerm | os.ModeSetuid); got != tc.hostPerm {
			t.Errorf("%s is %v on the host, want %v", tc.name, got, tc.hostPerm)
		}
		mode, _, _, ok := GuestAttr(p, false)
		if !ok {
			t.Skipf("%s has no user xattr support", rootfs)
		}
		if mode&0o7777 != tc.recorded {
			t.Errorf("%s recorded as %o, want %o", tc.name, mode&0o7777, tc.recorded)
		}
	}
	if _, err := os.ReadFile(filepath.Join(rootfs, "etc/shadow")); err != nil {
		t.Errorf("the owner cannot read the unpacked /etc/shadow, so mke2fs could not either: %v", err)
	}
}

// Linux cannot put a user.* record on a symlink, so the unpack sets symlink
// ownership aside beside the rootfs, and a later layer's link at the same name
// wins.
func TestUnprivilegedUnpackSetsSymlinkOwnershipAside(t *testing.T) {
	if !recordsOwnership {
		t.Skip("a root unpack chowns the links themselves")
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	first := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "busybox"}},
		tarEntry{hdr: tar.Header{Name: "home/app/.profile", Typeflag: tar.TypeSymlink, Linkname: "/etc/profile", Uid: 1000, Gid: 1000}},
	)
	second := rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "bash", Uid: 0, Gid: 7}},
	)
	for _, layer := range []io.Reader{first, second} {
		if err := extractTarIgnoreChown(layer, rootfs); err != nil {
			t.Fatal(err)
		}
	}
	owners, err := LinkOwners(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]LinkOwner{
		"bin/sh":            {UID: 0, GID: 7},
		"home/app/.profile": {UID: 1000, GID: 1000},
	}
	if !reflect.DeepEqual(owners, want) {
		t.Errorf("LinkOwners = %v, want %v", owners, want)
	}
	// Beside the rootfs, never in it: anything in it is in the guest image.
	if rel, err := filepath.Rel(rootfs, linkOwnersPath(rootfs)); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("the symlink record is at %s, inside the rootfs", linkOwnersPath(rootfs))
	}
}
