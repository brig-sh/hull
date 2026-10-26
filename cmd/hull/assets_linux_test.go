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

//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/bootassets"
)

// runAssets runs `hull assets` with args through the command tree a user
// runs, and returns what it printed.
func runAssets(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	app := newApp()
	app.Writer = &out
	err := app.Run(context.Background(), append([]string{"hull", "assets"}, args...))
	if err != nil {
		t.Fatalf("hull assets %v: %v", args, err)
	}
	return out.String()
}

// brig asks `hull assets dir` where the boot assets live and parses one bare
// line. On Linux the answer is brig's own directory, and HULL_BOOT_ASSETS
// still wins.
func TestAssetsDirIsBrigs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")

	got := runAssets(t, "dir")
	want := filepath.Join(home, ".local", "share", "brig", "assets") + "\n"
	if got != want {
		t.Errorf("hull assets dir printed %q, want %q", got, want)
	}

	t.Setenv("HULL_BOOT_ASSETS", "/srv/hull-assets")
	got = runAssets(t, "dir")
	if got != "/srv/hull-assets\n" {
		t.Errorf("with HULL_BOOT_ASSETS set, hull assets dir printed %q, want %q", got, "/srv/hull-assets\n")
	}
}

// writeBootAssets puts a kernel and an initrd into dir, as a fetch would,
// with no provenance record beside them.
func writeBootAssets(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{bootassets.KernelName(), bootassets.InitrdName} {
		err := os.WriteFile(filepath.Join(dir, name), []byte("not a kernel"), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A pull with both files present keeps them and reaches no registry, which
// is what lets this run offline.
func TestAssetsPullKeepsPresentAssets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HULL_BOOT_ASSETS", dir)
	writeBootAssets(t, dir)

	got := runAssets(t, "pull")
	want := "boot assets are already present in " + dir + " (use --force to replace them)\n"
	if got != want {
		t.Errorf("hull assets pull printed %q, want %q", got, want)
	}
}

func TestAssetsShowListsTheFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HULL_BOOT_ASSETS", dir)
	t.Setenv("HULL_BOOT_ASSETS_REF", "")
	writeBootAssets(t, dir)

	got := runAssets(t, "show")
	for _, want := range []string{
		"directory: " + dir + "\n",
		"reference: " + bootassets.Ref() + "\n",
		"provenance: none",
		bootassets.KernelName() + " ",
		bootassets.InitrdName + " ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hull assets show printed\n%s\nwant it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "missing") {
		t.Errorf("hull assets show reported a file missing:\n%s", got)
	}
}
