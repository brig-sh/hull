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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guest finds its disks by serial, and the serial is the position on the
// VMM's command line: the image's read-only lower first, the instance's
// writable upper second, and markers that name them that way.
func TestOverlayBlockLayoutOrdersLowerThenUpper(t *testing.T) {
	requireE2fsprogs(t)
	imageDir := newImageDir(t)
	instanceDir := t.TempDir()

	layout, err := prepareOverlayBlockRootfs(context.Background(),
		filepath.Dir(imageDir), filepath.Join(imageDir, "rootfs"), instanceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(layout.disks))
	}
	lower, upper := layout.disks[0], layout.disks[1]
	if lower.Path != filepath.Join(imageDir, lowerRootfsName) || !lower.ReadOnly {
		t.Errorf("disk0 is %+v, want the image's cached lower, read-only", lower)
	}
	if upper.Path != filepath.Join(instanceDir, "rootfs-upper.ext4") || upper.ReadOnly {
		t.Errorf("disk1 is %+v, want the instance's upper, writable", upper)
	}
	if layout.lowerMarker != "block:disk0:ext4" || layout.upperMarker != "block:disk1" {
		t.Errorf("markers %q / %q do not name disk0 and disk1", layout.lowerMarker, layout.upperMarker)
	}
}

// Checkpoint refuses an overlay-block instance by name, rather than pointing
// at --rootfs-type block, which that mode refuses; the legacy cases keep
// their answers.
func TestRequireBlockRootfsNamesOverlayBlock(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"block rootfs", []string{"vz-runner", "--rootfs", "/i/rootfs.ext4"}, ""},
		{"overlay-block", []string{"vz-runner", "--disk-ro", "/img/rootfs.ext4", "--disk", "/i/upper.ext4"}, "HULL_ROOTFS_MODE=overlay-block"},
		{"virtiofs root", []string{"vz-runner", "--share-ro", "/img/rootfs", "rootfs"}, "--rootfs-type block"},
	}
	for _, c := range cases {
		err := requireBlockRootfs(c.argv)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: got %v, want an error naming %q", c.name, err, c.wantErr)
		}
	}
	if err := requireBlockRootfs([]string{"vz-runner", "--disk", "/i/upper.ext4"}); err == nil ||
		strings.Contains(err.Error(), "start the instance with") {
		t.Errorf("overlay-block got the generic advice: %v", err)
	}
}

// Only a store image rootfs has an image directory to cache the lower in. A
// local bundle copied into an instance is named rootfs too, and a rootfs.ext4
// it ships must not be attached as the root.
func TestOverlayBlockLayoutRefusesANonStoreRootfs(t *testing.T) {
	store := t.TempDir()
	images := filepath.Join(store, "images")
	bundleRootfs := filepath.Join(store, "instances", "i", "bundle", "rootfs")
	for _, dir := range []string{filepath.Join(images, "sha256-x", "rootfs"), bundleRootfs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shipped := filepath.Join(filepath.Dir(bundleRootfs), lowerRootfsName)
	if err := os.WriteFile(shipped, []byte("not ours"), 0o400); err != nil {
		t.Fatal(err)
	}
	for _, rootfs := range []string{
		filepath.Join(t.TempDir(), "elsewhere"),
		bundleRootfs,
		filepath.Join(images, "rootfs"),
		filepath.Join(images, "sha256-x", "rootfs", "nested", "rootfs"),
	} {
		if _, err := prepareOverlayBlockRootfs(context.Background(), images, rootfs, t.TempDir()); err == nil ||
			!strings.Contains(err.Error(), "needs an image from the store") {
			t.Errorf("%s: got %v, want it refused as not a store image", rootfs, err)
		}
	}
	if !storeImageRootfs(images, filepath.Join(images, "sha256-x", "rootfs")) {
		t.Error("a store image rootfs was not recognised")
	}
}
