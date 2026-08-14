//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two stores under one parent must not resolve to the same backing image. They
// used to: the image was named after the parent, so the second attach failed
// with "Resource busy" because the first already held it.
func TestStoreImagePathIsPerStore(t *testing.T) {
	def := "/Users/x/.hull/store"
	a := storeImagePath("/work/store-a", def)
	b := storeImagePath("/work/store-b", def)
	if a == b {
		t.Fatalf("two stores share the image %q", a)
	}
}

// The default store keeps the path it has always had, or an existing install
// loses sight of every image it has pulled.
func TestStoreImagePathKeepsTheDefaultStable(t *testing.T) {
	def := "/Users/x/.hull/store"
	if got := storeImagePath(def, def); got != "/Users/x/.hull/hull-store.sparseimage" {
		t.Fatalf("default store image moved to %q", got)
	}
}

// The probe has to answer for the path, not for the volume in the abstract: a
// case-sensitive volume can be mounted anywhere.
func TestDirIsCaseSensitiveProbesTheRealPath(t *testing.T) {
	dir := t.TempDir()
	got, err := dirIsCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	// TMPDIR is on the boot volume, which is case-insensitive on a stock Mac.
	// Rather than assert a specific answer, check the probe agrees with what
	// the filesystem actually does.
	probe := filepath.Join(dir, "casetest")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(dir, "CASETEST"))
	insensitive := statErr == nil
	if got == insensitive {
		t.Fatalf("dirIsCaseSensitive said %v, but the filesystem says insensitive=%v", got, insensitive)
	}
}

// The probe must not leave anything behind: the store is listed by other code
// and a stray dotfile is a surprise at best.
func TestDirIsCaseSensitiveCleansUp(t *testing.T) {
	dir := t.TempDir()
	if _, err := dirIsCaseSensitive(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("probe left %v behind", names)
	}
}

// Mounting over a non-empty directory hides it: the files stay on disk,
// invisible and unused, and the store looks empty. Refusing is the whole point.
func TestEnsureStoreRefusesToHideExistingContents(t *testing.T) {
	dir := t.TempDir()
	sensitive, err := dirIsCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sensitive {
		t.Skip("TMPDIR is case-sensitive here, so no image would be mounted")
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}

	err = ensureStore(dir, "/Users/x/.hull/store")
	if err == nil {
		t.Fatal("ensureStore mounted over a non-empty directory")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
	// And it must not have created an image on the way out.
	if _, statErr := os.Stat(dir + ".sparseimage"); statErr == nil {
		t.Fatal("a sparse image was created despite the refusal")
	}
}

// A store hull already mounted is recognised by its marker and left alone,
// without shelling out to hdiutil on every command.
func TestEnsureStoreShortCircuitsOnTheMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeMarkerName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-empty and case-insensitive: without the marker check this would be
	// refused, so reaching nil proves the short circuit ran first.
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureStore(dir, "/Users/x/.hull/store"); err != nil {
		t.Fatalf("ensureStore did not accept an already-mounted store: %v", err)
	}
}

// Telemetry initialises from the root command, so its two files are already in
// the store directory by the time anything opens the store. Refusing to mount
// over hull's own bookkeeping would make every fresh --store-dir fail.
func TestEnsureStoreIgnoresHullsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	sensitive, err := dirIsCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sensitive {
		t.Skip("TMPDIR is case-sensitive here, so no image would be mounted")
	}
	for _, name := range []string{"telemetry.json", "telemetry.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := dirIsEmpty(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("hull's own bookkeeping counted as content worth protecting")
	}
}

// Anything else in there is a user's, and mounting would hide it.
func TestEnsureStoreStillProtectsRealContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty, err := dirIsEmpty(dir)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("a directory holding a store read as empty")
	}
}

// Detaching is what a cleanup step runs after a job that may or may not have
// opened a store. Finding nothing mounted is a success, not a failure -- an
// `if: always()` step must not turn a green build red because there was
// nothing to do.
func TestDetachStoreIsIdempotent(t *testing.T) {
	if err := detachStore(t.TempDir(), false); err != nil {
		t.Fatalf("detaching an unmounted directory failed: %v", err)
	}
}

// A directory that is not ours is left alone: no marker, no unmount. Detaching
// someone else's mount because it happened to be named as our store would be
// the runtime reaching well outside its own business.
func TestDetachStoreLeavesForeignMountsAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "somebody-elses-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := detachStore(dir, false); err != nil {
		t.Fatalf("detach touched a directory it does not own: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "somebody-elses-file")); err != nil {
		t.Fatalf("the file went away: %v", err)
	}
}

// An empty store dir is not something to detach, and saying so beats an
// hdiutil error about a path that was never a mount point.
func TestDetachStoreRefusesAnEmptyPath(t *testing.T) {
	if err := detachStore("", false); err == nil {
		t.Fatal("expected an empty store path to be rejected")
	}
}
