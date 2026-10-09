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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const e2fsckBin = "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck"

func requireE2fsprogs(t *testing.T) {
	t.Helper()
	for _, bin := range []string{mke2fsBin, debugfsBin, e2fsckBin} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s is not installed: %v", bin, err)
		}
	}
}

// newImageDir lays out a store image directory: a rootfs with a setuid binary
// owned by root and a root:shadow file, both recorded the way the unpack
// records them, and the unpack-schema stamp.
func newImageDir(t *testing.T) string {
	t.Helper()
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	for _, dir := range []string{"usr/bin", "etc"} {
		if err := os.MkdirAll(filepath.Join(rootfs, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sudo := filepath.Join(rootfs, "usr/bin/sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordAttr(t, sudo, 0o104755, 0, 0)
	if err := syscall.Chmod(sudo, 0o4755); err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(rootfs, "etc/shadow")
	if err := os.WriteFile(shadow, []byte("root:*:1::::::\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	recordAttr(t, shadow, 0o100640, 0, 42)
	if err := os.WriteFile(filepath.Join(imageDir, unpackSchemaStamp), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return imageDir
}

func debugfsStat(t *testing.T, disk, path string) string {
	t.Helper()
	out, err := exec.Command(debugfsBin, "-R", "stat "+path, disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat %s: %s: %v", path, out, err)
	}
	return string(out)
}

// The lower is the image as the guest must see it: setuid kept, the recorded
// ownership instead of the host user's, a clean filesystem, and no journal.
func TestLowerRootfsCarriesImageOwnership(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the host ownership already matches")
	}
	requireE2fsprogs(t)
	imageDir := newImageDir(t)

	disk, err := ensureLowerRootfs(context.Background(), imageDir)
	if err != nil {
		t.Fatalf("ensureLowerRootfs: %v", err)
	}
	if want := filepath.Join(imageDir, lowerRootfsName); disk != want {
		t.Fatalf("lower at %s, want %s", disk, want)
	}
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("lower is %v, want read-only for the owner", info.Mode().Perm())
	}
	if _, err := os.Stat(disk + ".tmp"); err == nil {
		t.Error("the staging file was left behind")
	}
	if left, _ := filepath.Glob(filepath.Join(imageDir, "block-inject-*")); len(left) > 0 {
		t.Errorf("debugfs scratch staged in the store image dir: %v", left)
	}

	out := debugfsStat(t, disk, "/usr/bin/sudo")
	if !strings.Contains(out, "Mode:  04755") || !strings.Contains(out, "User:     0") {
		t.Errorf("sudo lost its setuid bit or its owner:\n%s", out)
	}
	out = debugfsStat(t, disk, "/etc/shadow")
	if !strings.Contains(out, "Group:    42") {
		t.Errorf("shadow lost its group:\n%s", out)
	}
	out = debugfsStat(t, disk, "/")
	if !strings.Contains(out, "User:     0   Group:     0") {
		t.Errorf("the root directory is not root's:\n%s", out)
	}

	feats, err := exec.Command(debugfsBin, "-R", "features", disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs features: %s: %v", feats, err)
	}
	if strings.Contains(string(feats), "has_journal") {
		t.Errorf("the read-only lower has a journal: %s", feats)
	}
	if fsck, err := exec.Command(e2fsckBin, "-fn", disk).CombinedOutput(); err != nil {
		t.Errorf("e2fsck -fn on the lower: %s: %v", fsck, err)
	}
}

// A lower newer than the unpack stamp is reused; one older than it, or a
// missing one, is built again.
func TestLowerRootfsRebuildsOnlyWhenStale(t *testing.T) {
	requireE2fsprogs(t)
	imageDir := newImageDir(t)
	ctx := context.Background()

	disk, err := ensureLowerRootfs(ctx, imageDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ensureLowerRootfs(ctx, imageDir); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, again) {
		t.Error("a fresh lower was rebuilt")
	}

	// A re-unpack rewrites the stamp after the lower was built.
	stamp := filepath.Join(imageDir, unpackSchemaStamp)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(stamp, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureLowerRootfs(ctx, imageDir); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, rebuilt) {
		t.Error("a lower older than the unpack stamp was reused")
	}

	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stamp, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureLowerRootfs(ctx, imageDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Errorf("a missing lower was not rebuilt: %v", err)
	}
}

// Runs of the same image that start together build the lower once and all
// get the same file.
func TestLowerRootfsConcurrentRunsShareOneBuild(t *testing.T) {
	requireE2fsprogs(t)
	imageDir := newImageDir(t)

	const runs = 4
	var wg sync.WaitGroup
	infos := make([]os.FileInfo, runs)
	errs := make([]error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			disk, err := ensureLowerRootfs(context.Background(), imageDir)
			if err != nil {
				errs[i] = err
				return
			}
			infos[i], errs[i] = os.Stat(disk)
		}(i)
	}
	wg.Wait()
	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if !os.SameFile(infos[0], infos[i]) {
			t.Errorf("run %d got a different lower than run 0", i)
		}
	}
}

// A staging file left by a run that died is replaced, not trusted, and does
// not outlive the build.
func TestLowerRootfsReplacesACrashLeftover(t *testing.T) {
	requireE2fsprogs(t)
	imageDir := newImageDir(t)
	staging := filepath.Join(imageDir, lowerRootfsName+".tmp")
	if err := os.WriteFile(staging, []byte("not an ext4 image"), 0o400); err != nil {
		t.Fatal(err)
	}

	disk, err := ensureLowerRootfs(context.Background(), imageDir)
	if err != nil {
		t.Fatalf("ensureLowerRootfs over a leftover staging file: %v", err)
	}
	if _, err := os.Stat(staging); err == nil {
		t.Error("the leftover staging file is still there")
	}
	if fsck, err := exec.Command(e2fsckBin, "-fn", disk).CombinedOutput(); err != nil {
		t.Errorf("e2fsck -fn on the rebuilt lower: %s: %v", fsck, err)
	}
}

// A build that fails reports it, leaves no staging file, and releases the
// lock, so the next run fails the same way instead of hanging.
func TestLowerRootfsFailureLeavesNothingBehind(t *testing.T) {
	requireE2fsprogs(t)
	imageDir := t.TempDir() // no rootfs/ in it
	staging := filepath.Join(imageDir, lowerRootfsName+".tmp")

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := ensureLowerRootfs(ctx, imageDir)
		cancel()
		if err == nil {
			t.Fatalf("call %d: built a lower from a missing rootfs", i)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: blocked on the lock: %v", i, err)
		}
		if !strings.Contains(err.Error(), "build read-only rootfs image") {
			t.Errorf("call %d: error carries no context: %v", i, err)
		}
		if _, statErr := os.Stat(staging); statErr == nil {
			t.Errorf("call %d: a staging file was left behind", i)
		}
	}
	if _, err := os.Stat(filepath.Join(imageDir, lowerRootfsName)); err == nil {
		t.Error("a failed build installed a lower")
	}
}

// A cancelled run stops waiting for a lock another build holds.
func TestLockFileHonoursCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); err == nil {
		t.Fatal("took a lock that is already held")
	}
}

// The size covers block-rounded file contents with room to spare, and the
// inode count covers every entry.
func TestLowerRootfsGeometry(t *testing.T) {
	rootfs := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(rootfs, "f"+string(rune('a'+i))), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sizeMB, inodes, err := lowerRootfsGeometry(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if sizeMB < 64 {
		t.Errorf("size %d MiB leaves no room for metadata", sizeMB)
	}
	if inodes < 11 {
		t.Errorf("%d inodes for 11 entries", inodes)
	}
}
