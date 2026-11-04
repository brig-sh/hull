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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// Extra image lowers follow the root's pair, read-only, and the serial is the
// position: with an overlay-block root they are disk2 and disk3.
func TestAssembleDisksNumbersImageLowersAfterTheRoot(t *testing.T) {
	root := []types.BlockDevSpec{{Path: "/img/root.ext4", ReadOnly: true}, {Path: "/inst/upper.ext4"}}
	extra := []imageDisk{
		{ref: "nginx:alpine", digest: "sha256:aa", lower: "/img/nginx.ext4"},
		{ref: "redis", digest: "sha256:bb", lower: "/img/redis.ext4"},
	}
	disks, recs, err := assembleDisks(root, "busybox", "sha256:00", extra)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/img/root.ext4", "/inst/upper.ext4", "/img/nginx.ext4", "/img/redis.ext4"}
	wantRO := []bool{true, false, true, true}
	wantRole := []string{diskRoleRootfsLower, diskRoleRootfsUpper, diskRoleImageLower, diskRoleImageLower}
	if len(disks) != 4 || len(recs) != 4 {
		t.Fatalf("got %d disks and %d records, want 4", len(disks), len(recs))
	}
	for i := range disks {
		if disks[i].Path != wantPaths[i] || disks[i].ReadOnly != wantRO[i] {
			t.Errorf("disk %d is %+v, want %s ro=%t", i, disks[i], wantPaths[i], wantRO[i])
		}
		if recs[i].Serial != "disk"+string(rune('0'+i)) || recs[i].Role != wantRole[i] || recs[i].Path != wantPaths[i] {
			t.Errorf("record %d is %+v", i, recs[i])
		}
	}
	if recs[0].Image != "busybox" || recs[2].Image != "nginx:alpine" || recs[3].Digest != "sha256:bb" {
		t.Errorf("records lost their images: %+v", recs)
	}
	if len(root) != 2 {
		t.Error("assembleDisks changed the root's slice")
	}
}

// Without an overlay-block root the image lowers start at disk0.
func TestAssembleDisksWithoutARootPair(t *testing.T) {
	_, recs, err := assembleDisks(nil, "busybox", "sha256:00", []imageDisk{{ref: "nginx", digest: "sha256:aa", lower: "/n.ext4"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Serial != "disk0" || recs[0].Role != diskRoleImageLower {
		t.Errorf("records %+v, want one image-lower at disk0", recs)
	}
}

// The VMM takes four disks; a layout that needs more is refused, and so is a
// request that could never fit, before anything is pulled.
func TestImageDiskBudgetIsTheVMMLimit(t *testing.T) {
	if got := imageDiskBudget(true); got != 2 {
		t.Errorf("overlay-block budget %d, want 2", got)
	}
	if got := imageDiskBudget(false); got != 4 {
		t.Errorf("plain budget %d, want 4", got)
	}
	root := []types.BlockDevSpec{{Path: "a", ReadOnly: true}, {Path: "b"}}
	three := []imageDisk{{lower: "1"}, {lower: "2"}, {lower: "3"}}
	if _, _, err := assembleDisks(root, "r", "d", three); err == nil || !strings.Contains(err.Error(), "do not fit") {
		t.Errorf("five disks: err = %v, want a refusal", err)
	}
	_, err := resolveImageDisks(context.Background(), nil, nil, []string{"a", "b", "c"}, 2, "missing", "linux/arm64")
	if err == nil || !strings.Contains(err.Error(), "room for 2") {
		t.Errorf("three images in a budget of two: err = %v, want a refusal that names the budget", err)
	}
}

// The manifest is what a caller reads to learn the serials.
func TestWriteDisksManifest(t *testing.T) {
	dir := t.TempDir()
	_, recs, err := assembleDisks(
		[]types.BlockDevSpec{{Path: "/l", ReadOnly: true}, {Path: "/u"}}, "busybox", "sha256:00",
		[]imageDisk{{ref: "nginx:alpine", digest: "sha256:aa", lower: "/n"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDisksManifest(dir, recs); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, disksManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var m diskManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Disks) != 3 || m.Disks[2].Serial != "disk2" || m.Disks[2].Image != "nginx:alpine" || !m.Disks[2].ReadOnly {
		t.Errorf("manifest %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, disksManifestName+".tmp")); err == nil {
		t.Error("the staging file was left behind")
	}
}
