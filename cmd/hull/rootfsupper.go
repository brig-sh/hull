package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// rootfsUpperDiskSize is the apparent size of an ext4 upper layer. The file is
// sparse, so it costs only what the guest writes, and it caps what one instance
// can add on top of its image.
const rootfsUpperDiskSize = "16G"

// prepareRootfsUpper returns the instance's overlay upper layer, creating it on
// first boot and reusing it on every later one.
//
// kind "virtiofs" is a directory hvi exports read-write; vz-init makes the
// upper and work directories inside it. kind "block" is a sparse ext4 image
// hvi attaches as the guest's only disk. mke2fs is told the storage is already
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
		if err := os.Rename(staging, disk); err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("install overlay upper disk: %w", err)
		}
		return disk, nil
	}
	return "", fmt.Errorf("HULL_HVI_ROOTFS_UPPER must be virtiofs or block (got %q)", kind)
}
