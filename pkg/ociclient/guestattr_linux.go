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

//go:build linux

package ociclient

import (
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

// guestAttrXattr is the darwin record's name in the user namespace, which is
// the only one an unprivileged process may write on Linux.
//
// Linux has the same gap macOS does whenever hull is not root: archive.Apply
// fails its first chown, the manual extractor skips chown altogether, and
// every file in the unpacked rootfs ends up owned by whoever ran the pull --
// /etc/shadow, setuid-root binaries and all. mke2fs copies that ownership into
// the block image verbatim, so without a record the guest boots with a root
// filesystem owned by uid 1000, where sudo refuses to run and a setuid binary
// drops its caller to an ordinary user. The record carries what the tar header
// said, and buildBlockRootfs writes it into the finished image through debugfs,
// exactly as on macOS.
//
// The layout is the darwin one, twelve bytes, mode then uid then gid. hvi's
// Linux backend has no virtio-fs server, so nothing outside hull reads it here
// and the name only has to agree with GuestAttr below.
//
// Symlinks get no record: Linux refuses user.* attributes on a symlink
// (EPERM), so a link keeps the unpacking user's ownership in the image. A
// symlink's owner is consulted only by the sticky-directory and
// protected_symlinks checks, which an image's own links do not meet in
// practice.
const guestAttrXattr = "user.com.nofire.hvi.linux-attr"

// CopiedRecordXattr names the record as it arrives in a block image built
// from the unpacked tree, so the builder can take it out again.
//
// mke2fs -d copies every user.* attribute it finds, so without that every
// file in a Linux-built guest image carries hull's host-side bookkeeping,
// visible to the guest through getfattr, `tar --xattrs` and `rsync -X`, and
// the image differs from the one a Mac builds from the same layers.
const CopiedRecordXattr = guestAttrXattr

// recordsOwnership is true when the unpack cannot chown, i.e. when hull is not
// root. A root unpack keeps the image's ownership in the filesystem itself,
// mke2fs then copies the real one, and there is nothing to record.
//
// It is decided by the effective uid, not by whether archive.Apply happened to
// fail: inside a user namespace containerd swallows the chown EPERM and
// reports success, and the files still belong to the mapped user.
var recordsOwnership = os.Geteuid() != 0

// hostFileMode is the mode an unpacked regular file is given on the host:
// the image's own, with owner read added when the unpack records ownership.
//
// Fedora, RHEL, Rocky, Alma, UBI and Amazon Linux ship /etc/shadow and
// /etc/gshadow at 0000, and Fedora's sudo is 04111. Put on the host as is, a
// file like that is unreadable to the user who unpacked it -- root is the only
// one DAC lets past 0000 -- and `mke2fs -d` stops on the first one with
// "Permission denied while populating file system". Block is the only rootfs
// on Linux, so that refused the whole image family. The image's mode is not
// lost by widening: the record beside the file holds it, and
// buildBlockRootfs writes it into the finished image.
//
// A root unpack keeps the mode untouched: root reads a 0000 file regardless,
// and there is no record to put the real mode back from.
func hostFileMode(mode int64) int64 {
	if recordsOwnership {
		return mode | 0o400
	}
	return mode
}

// hostDirMode is hostFileMode for directories, which need the owner's write
// and search bits as well as read: without them mke2fs cannot list a 0111
// directory, and nobody but root can delete a tree that has 0555 directories
// in it (Fedora has dozens under /usr/lib), so `hull rmi` and the pull's own
// sweeps would fail on it.
func hostDirMode(mode int64) int64 {
	if recordsOwnership {
		return mode | 0o700
	}
	return mode
}

// recordGuestAttr stores the ownership and mode an entry has inside the image.
// Errors are the caller's to ignore, as on darwin: a filesystem with no user
// xattr support (tmpfs before 6.6, some network filesystems) costs the guest
// correct ownership, not the pull.
func recordGuestAttr(path string, mode uint32, uid, gid uint32, symlink bool) error {
	if symlink {
		return nil
	}
	var value [12]byte
	binary.LittleEndian.PutUint32(value[0:4], mode&0o177777)
	binary.LittleEndian.PutUint32(value[4:8], uid)
	binary.LittleEndian.PutUint32(value[8:12], gid)
	return unix.Setxattr(path, guestAttrXattr, value[:], 0)
}

// GuestAttr returns the ownership and mode recorded for path by the unpack,
// and whether there was a record at all. A tree unpacked as root has none, and
// its files carry the image's ownership on disk, which is what the caller then
// gets by reading the file itself.
func GuestAttr(path string, symlink bool) (mode, uid, gid uint32, ok bool) {
	if symlink {
		return 0, 0, 0, false
	}
	value := make([]byte, 12)
	n, err := unix.Getxattr(path, guestAttrXattr, value)
	if err != nil || n != 12 {
		return 0, 0, 0, false
	}
	return binary.LittleEndian.Uint32(value[0:4]),
		binary.LittleEndian.Uint32(value[4:8]),
		binary.LittleEndian.Uint32(value[8:12]), true
}
