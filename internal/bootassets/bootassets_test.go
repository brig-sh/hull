// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package bootassets

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKernelNameFollowsHostArch(t *testing.T) {
	got := KernelName()
	want := "Image"
	if runtime.GOARCH == "amd64" {
		want = "bzImage"
	}
	if got != want {
		t.Fatalf("KernelName() = %q, want %q on %s", got, want, runtime.GOARCH)
	}
}

// The assets belong to the store, so that --store-dir isolates them along with
// the images and instances. Two stores must not share a kernel.
func TestDirLivesUnderTheStore(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	got, err := Dir("/tmp/store-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/tmp/store-a", "assets") {
		t.Fatalf("Dir = %q, want it under the store", got)
	}
	other, err := Dir("/tmp/store-b")
	if err != nil {
		t.Fatal(err)
	}
	if other == got {
		t.Fatal("two different stores resolved to the same asset directory")
	}
}

// HULL_BOOT_ASSETS is what points hull at a local build of the assets repo, so
// it has to win over the store, the brig variable and the per-OS default.
func TestDirPrefersHullOverride(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS", "/brig/assets")
	t.Setenv("HULL_BOOT_ASSETS", "/hull/assets")
	got, err := Dir("/tmp/some-store")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/hull/assets" {
		t.Fatalf("Dir = %q, want the HULL_BOOT_ASSETS value", got)
	}
}

// A machine that already runs brig has the assets somewhere brig chose. Honour
// that rather than downloading a second copy.
func TestDirFallsBackToBrigOverride(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "/brig/assets")
	got, err := Dir("/tmp/some-store")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/brig/assets" {
		t.Fatalf("Dir = %q, want the BRIG_BOOT_ASSETS value", got)
	}
}

// With no store to hang them off, fall back to what brig looks for.
func TestDirWithoutAStoreMatchesTheRuntimes(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	got, err := Dir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(".hull", "assets")
	if runtime.GOOS != "darwin" {
		want = filepath.Join("brig", "assets")
	}
	if !strings.HasSuffix(got, want) {
		t.Fatalf("Dir = %q, want it to end in %q", got, want)
	}
}

// The platform lives in the tag, not in an index, so the default reference has
// to name this host exactly.
func TestRefNamesTheHostPlatform(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS_REF", "")
	got := Ref()
	want := DefaultRepo + ":" + runtime.GOOS + "-" + runtime.GOARCH
	if got != want {
		t.Fatalf("Ref() = %q, want %q", got, want)
	}
}

func TestRefHonoursOverride(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS_REF", "ghcr.io/example/assets:v9-darwin-arm64")
	if got := Ref(); got != "ghcr.io/example/assets:v9-darwin-arm64" {
		t.Fatalf("Ref() = %q, want the override", got)
	}
}

// An empty file is worse than a missing one: it satisfies a naive existence
// check and then fails at boot with no useful error.
func TestPresentRejectsEmptyAndMissingFiles(t *testing.T) {
	store := t.TempDir()
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	dir := filepath.Join(store, "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if Present(store) {
		t.Fatal("Present is true with an empty directory")
	}
	writeTestFile(t, filepath.Join(dir, KernelName()), "kernel")
	if Present(store) {
		t.Fatal("Present is true with no initrd")
	}
	writeTestFile(t, filepath.Join(dir, InitrdName), "")
	if Present(store) {
		t.Fatal("Present is true with a zero-length initrd")
	}
	writeTestFile(t, filepath.Join(dir, InitrdName), "initrd")
	if !Present(store) {
		t.Fatal("Present is false with both files in place")
	}
}

func TestCheckReportsNotPresent(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	_, _, err := Check(t.TempDir())
	if err == nil {
		t.Fatal("Check succeeded with no assets on disk")
	}
	// The caller distinguishes "nothing downloaded yet" from "the registry is
	// unreachable", so this has to stay an identifiable error.
	if !strings.Contains(err.Error(), ErrNotPresent.Error()) {
		t.Fatalf("Check error = %v, want it to wrap ErrNotPresent", err)
	}
}

func TestPathsUseTheAssetDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HULL_BOOT_ASSETS", dir)
	kernel, initrd, err := Paths("/tmp/ignored-store")
	if err != nil {
		t.Fatal(err)
	}
	if kernel != filepath.Join(dir, KernelName()) || initrd != filepath.Join(dir, InitrdName) {
		t.Fatalf("Paths = %q, %q", kernel, initrd)
	}
}

// Nothing in the bundle is run on the host, so a fetched blob should not come
// out runnable.
func TestFetchedFilesAreNotExecutable(t *testing.T) {
	if bundleFileMode&0o111 != 0 {
		t.Fatalf("bundleFileMode is %v, want no executable bit", bundleFileMode)
	}
}

// Goes through the real writeFile, so the atomic staging path is exercised by
// every test that needs a file on disk.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFile(path, strings.NewReader(content), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(got) != content {
		t.Fatalf("writeFile wrote %q, want %q", got, content)
	}
}
