// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bootassets

import (
	"path/filepath"
	"testing"
)

// Linux hull has no store, so its directory is the one brig uses.
func TestDirWithoutAStoreFollowsBrig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	for _, tc := range []struct {
		xdg  string
		want string
	}{
		{"", filepath.Join(home, ".local", "share", "brig", "assets")},
		{"/srv/share", filepath.Join("/srv/share", "brig", "assets")},
	} {
		t.Setenv("XDG_DATA_HOME", tc.xdg)
		got, err := Dir("")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("XDG_DATA_HOME=%q: Dir = %q, want %q", tc.xdg, got, tc.want)
		}
	}
}
