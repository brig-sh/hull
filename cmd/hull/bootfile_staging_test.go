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

// --- review finding: the imageFileHostPath TOCTOU ----------------------------
//
// rootfs_containment_test.go holds the containment: an image cannot make
// imageFileHostPath name a file outside its own rootfs. What it does not hold
// is time. The helper opens the name, checks it, closes it and returns a
// string; the VMM opens that string again later. Two resolutions of the same
// name, and in between the image rootfs is an ordinary directory on disk that a
// concurrent `hull pull` republishing the tag -- or, where the rootfs is shared
// writable, the guest itself -- can change. The file that boots is then not the
// file that was checked.
//
// The fix is to stop handing out the name at all for anything that boots: copy
// the bytes off the descriptor the check already validated, into the instance
// directory, and give the VMM that. These tests hold both halves -- that the
// window is real on the path form, and that it is closed on the staged form.

// The source guard that keeps the boot paths on the staged form lives beside
// the other one, in rootfs_containment_test.go.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapKernelForVictim replaces the image's kernel with a symlink to a host file
// -- exactly what a malicious image gets a second chance at once hull has
// approved the name and gone away to build a command line.
func swapKernelForVictim(t *testing.T, rootfs, rel, target string) {
	t.Helper()
	full := filepath.Join(rootfs, rel)
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func TestStagedBootFileSurvivesARootfsSwapAfterValidation(t *testing.T) {
	base, rootfs := newRootfs(t)
	v := plantVictim(t, filepath.Join(base, "secrets"), "id_ed25519", "ssh-ed25519 AAAAsecret\n")
	if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("REAL-KERNEL-BYTES\n")
	writeKernel := func() {
		if err := os.WriteFile(filepath.Join(rootfs, "boot", "vmlinuz"), kernel, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeKernel()

	// The window, demonstrated. This is not the behaviour under test, it is the
	// premise: if this ever stops holding, the staged assertion below stops
	// meaning anything and the test should be re-read rather than trusted.
	pathForm, err := imageFileHostPath(rootfs, "boot/vmlinuz")
	if err != nil {
		t.Fatal(err)
	}
	swapKernelForVictim(t, rootfs, "boot/vmlinuz", v.path)
	if got, err := os.ReadFile(pathForm); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, v.content) {
		t.Fatalf("premise no longer holds: a validated path no longer follows a later swap (%q)", got)
	}
	t.Logf("a validated path re-reads as %q after the image swaps the entry", v.content)

	// The same sequence against the staged form: validate, let the image swap
	// the entry, then read what the VMM would be given.
	if err := os.Remove(filepath.Join(rootfs, "boot", "vmlinuz")); err != nil {
		t.Fatal(err)
	}
	writeKernel()
	instanceDir := t.TempDir()
	staged, err := stageImageBootFile(rootfs, "boot/vmlinuz", instanceDir, stagedKernelName)
	if err != nil {
		t.Fatal(err)
	}
	swapKernelForVictim(t, rootfs, "boot/vmlinuz", v.path)

	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, kernel) {
		t.Errorf("the VMM would boot %q after the swap, want the validated kernel %q", got, kernel)
	}
	if strings.HasPrefix(staged, rootfs+string(os.PathSeparator)) {
		t.Errorf("staged boot file %q is still inside the image rootfs %q; "+
			"the image can still change what that name resolves to", staged, rootfs)
	}
	if want := filepath.Join(instanceDir, stagedKernelName); staged != want {
		t.Errorf("staged boot file is %q, want it in the instance directory at %q "+
			"so `hull rm` clears it with everything else", staged, want)
	}
	v.mustBeUntouched(t)
}

// Staging inherits the containment, because it resolves the name the same way:
// a planted symlink or a traversal is refused before anything is copied, and it
// is still distinguishable as an image having planted something.
func TestStagingRefusesWhatTheResolverRefuses(t *testing.T) {
	base, rootfs := newRootfs(t)
	v := plantVictim(t, filepath.Join(base, "secrets"), "id_ed25519", "ssh-ed25519 AAAAsecret\n")
	if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v.path, filepath.Join(rootfs, "boot", "initrd")); err != nil {
		t.Fatal(err)
	}
	instanceDir := t.TempDir()

	if got, err := stageImageBootFile(rootfs, "/boot/initrd", instanceDir, stagedInitrdName); err == nil {
		t.Errorf("no error: a planted symlink staged to %q, which would be copied into guest RAM", got)
	} else {
		mustRefuse(t, err, "/boot/initrd")
	}
	if _, err := os.Lstat(filepath.Join(instanceDir, stagedInitrdName)); !os.IsNotExist(err) {
		t.Errorf("a refused name still left a staged file behind: %v", err)
	}
	v.mustBeUntouched(t)
}

// A copy that cannot be made is not the same thing as an image that has no
// initrd, and the callers tell them apart by this sentinel: without it a full
// disk would silently drop the image's initrd and share the rootfs directory
// instead, which boots something else entirely.
func TestStagingFailureIsDistinguishableFromAMissingFile(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.WriteFile(filepath.Join(rootfs, "vmlinuz"), []byte("ELF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingDir := filepath.Join(t.TempDir(), "no-such-instance")

	_, err := stageImageBootFile(rootfs, "vmlinuz", missingDir, stagedKernelName)
	if !errors.Is(err, errStageBootFile) {
		t.Errorf("a failed copy must carry errStageBootFile, got %v", err)
	}
	if errors.Is(err, errImagePlantedPath) {
		t.Errorf("a failed copy must not read as a containment refusal: %v", err)
	}

	// And the other way round: a name that is simply not in the image is an
	// ordinary missing file, which is the case the initrd caller falls through
	// on.
	_, err = stageImageBootFile(rootfs, "boot/initrd", t.TempDir(), stagedInitrdName)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file must stay a missing file: %v", err)
	}
	if errors.Is(err, errStageBootFile) {
		t.Errorf("a missing file must not read as a staging failure: %v", err)
	}
}
