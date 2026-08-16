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

//go:build darwin

package ociclient

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// guestAttrXattr carries what a file's ownership and mode are *in the image*,
// as opposed to what they are on the host.
//
// An image says /usr/bin/sudo is root-owned and setuid. macOS will not let an
// ordinary user say that on disk: chown to root needs privilege we do not
// have, so every file an unprivileged unpack writes ends up owned by whoever
// ran it. The information is not wrong, it just has nowhere to live -- so it
// lives here, and hvi's virtio-fs reads it back when it answers the guest.
//
// The name and the layout are hvi's, not ours: see HVI_XATTR_LINUX_ATTR and
// stored_guest_attr in hvi-vmm's src/virtio_fs.rs. Twelve bytes,
// little-endian, mode then uid then gid. Changing either side alone silently
// stops the other from reading it, which is why this comment names the file to
// change with it.
const guestAttrXattr = "com.nubificus.hvi.linux-attr"

// preferManualExtract keeps unpacking on the extractor that records the above.
//
// containerd's archive.Apply cannot run to completion here anyway: it chowns
// every entry, and an unprivileged process on macOS cannot chown to root, so
// it fails partway through and the manual extractor finishes the job. Going
// there directly saves decompressing each layer twice, and -- the reason this
// constant exists -- it is the only path that holds the tar header, which is
// the only place the image's true ownership is still known.
const preferManualExtract = true

// recordGuestAttr stores the ownership and mode an entry has inside the image.
//
// symlink is set for entries whose own attributes matter rather than their
// target's: following one here would stamp the attributes of a symlink onto
// whatever it points at, which for an absolute link inside a rootfs is another
// file in the image.
//
// Errors are the caller's to ignore. A filesystem with no xattr support costs
// the guest correct ownership, not the unpack, and failing the pull over it
// would be worse than the thing it reports.
func recordGuestAttr(path string, mode uint32, uid, gid uint32, symlink bool) error {
	var value [12]byte
	// Only the low bits survive the round trip: hvi takes the file-type bits
	// from the live file and the permission, setuid, setgid and sticky bits
	// from here.
	binary.LittleEndian.PutUint32(value[0:4], mode&0o177777)
	binary.LittleEndian.PutUint32(value[4:8], uid)
	binary.LittleEndian.PutUint32(value[8:12], gid)
	flags := 0
	if symlink {
		flags = unix.XATTR_NOFOLLOW
	}
	return unix.Setxattr(path, guestAttrXattr, value[:], flags)
}
