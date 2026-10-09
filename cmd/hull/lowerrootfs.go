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

//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// lowerRootfsName is the read-only ext4 image of an image's rootfs,
	// kept beside it in the store and shared by every instance of the image.
	lowerRootfsName = "rootfs.ext4"
	// unpackSchemaStamp is the file the unpack writes last into an image
	// directory (store.WriteUnpackSchema). A lower older than it was built
	// from a rootfs that has since been rewritten.
	unpackSchemaStamp = "unpack-schema"
	// ext4BlockSize is the block size mke2fs picks for these image sizes.
	ext4BlockSize = 4096
)

// ensureLowerRootfs returns the read-only ext4 image of imageDir's rootfs,
// building it on first use.
//
// The image is built once per digest with the block path's tools (mke2fs -d,
// then the debugfs ownership pass and repair), into a temporary file renamed
// into place, under an flock on <imageDir>/rootfs.ext4.lock so concurrent runs
// of the same image build it once. It is rebuilt when it is missing or older
// than the image's unpack-schema stamp. It carries no per-instance content,
// has no journal, and is made read-only on the host as well.
func ensureLowerRootfs(ctx context.Context, imageDir string) (string, error) {
	disk := filepath.Join(imageDir, lowerRootfsName)
	if lowerRootfsFresh(disk, imageDir) {
		return disk, nil
	}

	unlock, err := lockFile(ctx, disk+".lock")
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another run may have built it while this one waited for the lock.
	if lowerRootfsFresh(disk, imageDir) {
		return disk, nil
	}

	rootfsDir := filepath.Join(imageDir, "rootfs")
	sizeMB, inodes, err := lowerRootfsGeometry(rootfsDir)
	if err != nil {
		return "", fmt.Errorf("build read-only rootfs image %s: %w", disk, err)
	}

	// A staging file left by a run that died is garbage; start over.
	staging := disk + ".tmp"
	_ = os.Remove(staging)
	started := time.Now()
	if err := buildLowerRootfs(ctx, staging, rootfsDir, sizeMB, inodes); err != nil {
		_ = os.Remove(staging)
		return "", fmt.Errorf("build read-only rootfs image %s: %w", disk, err)
	}
	if err := os.Rename(staging, disk); err != nil {
		_ = os.Remove(staging)
		return "", fmt.Errorf("install read-only rootfs image: %w", err)
	}
	log.Debugf("Built read-only rootfs image %s (%d MiB apparent) in %s",
		disk, sizeMB, time.Since(started).Round(time.Millisecond))
	return disk, nil
}

// buildLowerRootfs writes the read-only image at diskPath.
func buildLowerRootfs(ctx context.Context, diskPath, rootfsDir string, sizeMB int, inodes int64) error {
	if err := mkfsFromDir(ctx, diskPath, rootfsDir, sizeMB,
		"-O", "^has_journal",
		"-N", fmt.Sprint(inodes),
		"-E", "assume_storage_prezeroed=1,root_owner=0:0"); err != nil {
		return err
	}
	if err := os.Chmod(diskPath, 0o600); err != nil {
		return fmt.Errorf("failed to restrict the read-only rootfs image: %w", err)
	}
	// Scratch files go to the system temp dir, not the store image dir, so a
	// run that dies here leaves nothing in the image.
	if err := applyBlockFixups(ctx, diskPath, rootfsDir, nil, ""); err != nil {
		return err
	}
	// Nothing writes it again: every VMM attaches it read-only.
	if err := os.Chmod(diskPath, 0o400); err != nil {
		return fmt.Errorf("failed to make the rootfs image read-only: %w", err)
	}
	return nil
}

// lowerRootfsFresh reports whether disk exists and is not older than the
// image's unpack-schema stamp. An image with no stamp has nothing to be
// compared against, so an existing disk is used.
func lowerRootfsFresh(disk, imageDir string) bool {
	info, err := os.Stat(disk)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	stamp, err := os.Stat(filepath.Join(imageDir, unpackSchemaStamp))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return !info.ModTime().Before(stamp.ModTime())
}

// lowerRootfsGeometry sizes the read-only image from the rootfs it holds:
// every file's size rounded up to a block, one block per directory, symlink
// and other entry, plus 20% and 64 MiB of slack for the filesystem's own
// metadata. The file is sparse, so the slack costs no host space. The inode
// count is the entry count plus 20% and 4096, since an image of many small
// files runs out of inodes before blocks at mke2fs's default ratio.
func lowerRootfsGeometry(rootfsDir string) (sizeMB int, inodes int64, err error) {
	var bytes, entries int64
	walkErr := filepath.WalkDir(rootfsDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if d.Type().IsRegular() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			bytes += (info.Size() + ext4BlockSize - 1) / ext4BlockSize * ext4BlockSize
		} else {
			bytes += ext4BlockSize
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("size image rootfs %s: %w", rootfsDir, walkErr)
	}
	total := bytes + bytes/5 + 64<<20
	return int((total + 1<<20 - 1) >> 20), entries + entries/5 + 4096, nil
}

// lockFile takes an exclusive flock on path, creating it if needed, and
// returns the function that releases it. It polls so a cancelled run stops
// waiting.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
