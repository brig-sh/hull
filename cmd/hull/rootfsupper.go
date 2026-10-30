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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// rootfsUpperDiskSize is the apparent size of an ext4 upper layer. The file is
// sparse, so it costs only what the guest writes, and it caps what one instance
// can add on top of its image.
const rootfsUpperDiskSize = "16G"

// rootfsModeOverlayBlock is the HULL_ROOTFS_MODE value that boots a container
// from a read-only ext4 lower and a per-instance ext4 upper, both as disks.
const rootfsModeOverlayBlock = "overlay-block"

// hviRootfsUpper returns the upper layer kind HULL_HVI_ROOTFS_UPPER asks for,
// or "" when it is unset or does not apply to this run. A value it does not
// know is an error, and one set where it does not apply (another monitor, a
// unikernel, --rootfs-type block) is named in a warning, so neither a typo nor
// a run on the wrong backend boots the clone without a word.
func hviRootfsUpper(containerBoot bool, vmm hypervisors.VmmType, rootfsType string) (string, error) {
	kind := os.Getenv("HULL_HVI_ROOTFS_UPPER")
	switch kind {
	case "":
		return "", nil
	case "virtiofs", "block":
	default:
		return "", fmt.Errorf("HULL_HVI_ROOTFS_UPPER must be virtiofs or block (got %q)", kind)
	}
	if !containerBoot || vmm != hypervisors.HviVmm || rootfsType == "block" {
		log.Warnf("HULL_HVI_ROOTFS_UPPER=%s applies to hvi container boots with a virtiofs root only; ignored for this run", kind)
		return "", nil
	}
	return kind, nil
}

// prepareRootfsUpper returns the instance's overlay upper layer, creating it on
// first boot and reusing it on every later one.
//
// kind "virtiofs" is a directory hvi exports read-write; vz-init makes the
// upper and work directories inside it. kind "block" is a sparse ext4 image
// the VMM attaches as a disk. mke2fs is told the storage is already
// zeroed, which a fresh sparse file is, so it writes only the metadata it needs
// and the guest has no inode tables left to initialise.
func prepareRootfsUpper(instanceDir, kind string) (string, error) {
	switch kind {
	case "virtiofs":
		dir := filepath.Join(instanceDir, "rootfs-upper")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create overlay upper directory: %w", err)
		}
		return dir, nil
	case "block":
		disk := filepath.Join(instanceDir, "rootfs-upper.ext4")
		if _, err := os.Stat(disk); err == nil {
			return disk, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect overlay upper disk: %w", err)
		}
		staging := disk + ".tmp"
		_ = os.Remove(staging)
		out, err := exec.Command(mke2fsBin, "-q", "-F", "-t", "ext4", "-m", "0",
			"-E", "assume_storage_prezeroed=1,root_owner=0:0",
			staging, rootfsUpperDiskSize).CombinedOutput()
		if err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("create overlay upper disk: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// mke2fs creates the file under the umask, usually 0644. Everything
		// the guest writes lands on this disk, so only the owner may read it.
		if err := os.Chmod(staging, 0o600); err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("restrict overlay upper disk: %w", err)
		}
		if err := os.Rename(staging, disk); err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("install overlay upper disk: %w", err)
		}
		return disk, nil
	}
	return "", fmt.Errorf("HULL_HVI_ROOTFS_UPPER must be virtiofs or block (got %q)", kind)
}

// overlayBlockRootfs is the disk layout of HULL_ROOTFS_MODE=overlay-block.
type overlayBlockRootfs struct {
	// disks go on the VMM's command line in this order, so the lower is
	// serial disk0 and the upper disk1.
	disks []types.BlockDevSpec
	// lowerMarker and upperMarker are the contents of /urunc-rootfs-lower
	// and /urunc-rootfs-upper in the instance's initrd.
	lowerMarker string
	upperMarker string
}

// prepareOverlayBlockRootfs returns the overlay-block layout for an instance
// of the image whose store rootfs is imageRootfs: the image's cached read-only
// ext4 as the lower and the instance's own sparse ext4 as the upper.
//
// imagesDir is the store's images/ directory, and imageRootfs must be the
// rootfs of an image directly under it. A local OCI bundle (hull run ./dir)
// is copied into the instance, and its rootfs is named rootfs too: a lower
// built beside it would be rebuilt on every run, and a rootfs.ext4 the bundle
// ships, having no unpack-schema stamp to be older than, would be attached as
// the root.
func prepareOverlayBlockRootfs(ctx context.Context, imagesDir, imageRootfs, instanceDir string) (overlayBlockRootfs, error) {
	if !storeImageRootfs(imagesDir, imageRootfs) {
		return overlayBlockRootfs{}, fmt.Errorf("HULL_ROOTFS_MODE=%s needs an image from the store, got %s",
			rootfsModeOverlayBlock, imageRootfs)
	}
	lower, err := ensureLowerRootfs(ctx, filepath.Dir(imageRootfs))
	if err != nil {
		return overlayBlockRootfs{}, err
	}
	upper, err := prepareRootfsUpper(instanceDir, "block")
	if err != nil {
		return overlayBlockRootfs{}, err
	}
	return overlayBlockRootfs{
		disks: []types.BlockDevSpec{
			{Path: lower, ReadOnly: true},
			{Path: upper},
		},
		lowerMarker: "block:disk0:ext4",
		upperMarker: "block:disk1",
	}, nil
}

// storeImageRootfs reports whether imageRootfs is <imagesDir>/<image>/rootfs,
// symlinks resolved on both sides.
func storeImageRootfs(imagesDir, imageRootfs string) bool {
	if filepath.Base(imageRootfs) != "rootfs" {
		return false
	}
	images, err := filepath.EvalSymlinks(imagesDir)
	if err != nil {
		return false
	}
	rootfs, err := filepath.EvalSymlinks(imageRootfs)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(images, filepath.Dir(rootfs))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") &&
		!strings.ContainsRune(rel, filepath.Separator)
}
