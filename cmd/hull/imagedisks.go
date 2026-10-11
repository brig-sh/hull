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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const (
	// maxVMMDisks is how many disks hvi attaches to one VM, --disk and
	// --disk-ro together.
	maxVMMDisks = 4
	// overlayBlockRootDisks is what HULL_ROOTFS_MODE=overlay-block takes of
	// them: the root's lower and upper.
	overlayBlockRootDisks = 2
	// disksManifestName is the file in the instance directory that says
	// which serial is which disk.
	disksManifestName = "disks.json"

	diskRoleRootfsLower = "rootfs-lower"
	diskRoleRootfsUpper = "rootfs-upper"
	diskRoleImageLower  = "image-lower"
)

// diskRecord is one disk of the VM as the manifest lists it.
type diskRecord struct {
	// Serial is the virtio-blk serial the guest reads from
	// /sys/block/<dev>/serial: disk<N>, N the position on the VMM's
	// command line.
	Serial   string `json:"serial"`
	Role     string `json:"role"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly"`
	// Image and Digest name the image of an image-lower disk, and of the
	// root pair's lower. Image is the reference as it was given.
	Image  string `json:"image,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// diskManifest is <instance>/disks.json.
type diskManifest struct {
	Disks []diskRecord `json:"disks"`
}

// imageDisk is an extra image whose cached read-only ext4 lower is attached.
type imageDisk struct {
	ref    string
	digest string
	lower  string
}

// imageDiskBudget is how many --image-disk images the VM has room for: the
// VMM's disks, less the two an overlay-block root takes.
func imageDiskBudget(overlayBlock bool) int {
	if overlayBlock {
		return maxVMMDisks - overlayBlockRootDisks
	}
	return maxVMMDisks
}

// resolveImageDisks pulls each --image-disk image as the pull policy says and
// builds or reuses its cached read-only ext4 lower, exactly as the root
// image's lower is. Nothing is pulled when the list cannot fit in budget.
func resolveImageDisks(ctx context.Context, client *ociclient.Client, s *store.Store,
	refs []string, budget int, pullPolicy, platform string) ([]imageDisk, error) {
	if len(refs) > budget {
		return nil, fmt.Errorf("--image-disk given %d times, but the VM has room for %d of its %d disks (an overlay-block root takes %d)",
			len(refs), budget, maxVMMDisks, overlayBlockRootDisks)
	}
	var out []imageDisk
	seen := map[string]string{}
	for _, ref := range refs {
		digest, err := resolveImageDigest(ctx, client, s, ref, pullPolicy, platform)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[digest]; dup {
			return nil, fmt.Errorf("--image-disk %s and %s are the same image (%s); name it once", prev, ref, digest)
		}
		seen[digest] = ref
		rootfs := s.ImageRootfsDir(digest)
		if _, err := os.Stat(rootfs); err != nil {
			return nil, fmt.Errorf("--image-disk %s: unpacked rootfs: %w", ref, err)
		}
		lower, err := ensureLowerRootfs(ctx, filepath.Dir(rootfs))
		if err != nil {
			return nil, err
		}
		out = append(out, imageDisk{ref: ref, digest: digest, lower: lower})
	}
	return out, nil
}

// assembleDisks puts the extra image lowers after the root's disks and says
// which serial each disk got. root is the overlay-block pair (lower, upper),
// or nil when the root is not on disks. rootRef and rootDigest name the root
// image. The serial is the position in the returned list, which is the
// position on the VMM's command line.
func assembleDisks(root []types.BlockDevSpec, rootRef, rootDigest string, extra []imageDisk) ([]types.BlockDevSpec, []diskRecord, error) {
	if len(root)+len(extra) > maxVMMDisks {
		return nil, nil, fmt.Errorf("%d disks do not fit: the VMM attaches at most %d", len(root)+len(extra), maxVMMDisks)
	}
	disks := append([]types.BlockDevSpec{}, root...)
	var recs []diskRecord
	for i, d := range root {
		rec := diskRecord{Serial: fmt.Sprintf("disk%d", i), Path: d.Path, ReadOnly: d.ReadOnly}
		if i == 0 {
			rec.Role, rec.Image, rec.Digest = diskRoleRootfsLower, rootRef, rootDigest
		} else {
			rec.Role = diskRoleRootfsUpper
		}
		recs = append(recs, rec)
	}
	for _, e := range extra {
		disks = append(disks, types.BlockDevSpec{Path: e.lower, ReadOnly: true})
		recs = append(recs, diskRecord{
			Serial: fmt.Sprintf("disk%d", len(disks)-1), Role: diskRoleImageLower,
			Path: e.lower, ReadOnly: true, Image: e.ref, Digest: e.digest,
		})
	}
	return disks, recs, nil
}

// writeDisksManifest records the disk layout in the instance directory, so a
// caller can read which serial is which without counting argv.
func writeDisksManifest(instanceDir string, recs []diskRecord) error {
	b, err := json.MarshalIndent(diskManifest{Disks: recs}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(instanceDir, disksManifestName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", disksManifestName, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", disksManifestName, err)
	}
	return nil
}
