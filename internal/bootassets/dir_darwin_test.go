// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package bootassets

import (
	"path/filepath"
	"testing"
)

// With no store to hang them off, fall back to what brig looks for.
func TestDirWithoutAStoreIsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	got, err := Dir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".hull", "assets")
	if got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
}
