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

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// An image's 0555 and 0000 directories stop a plain RemoveAll by their owner,
// and a directory symlink in the tree must not be followed while opening the
// tree up: the chmod would land on a host directory the image chose.
func TestRemoveTreeRemovesReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so there is nothing to open up")
	}
	base := t.TempDir()
	tree := filepath.Join(base, "rootfs")
	readOnly := filepath.Join(tree, "usr", "lib", "sealed")
	if err := os.MkdirAll(readOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readOnly, "f"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(tree, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "host-dir")
	if err := os.Mkdir(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	if err := os.Symlink(outside, filepath.Join(tree, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{readOnly, filepath.Join(tree, "usr", "lib")} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	if err := RemoveTree(tree); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree is still there: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o500 {
		t.Errorf("the directory behind a symlink in the tree went from 0500 to %o", fi.Mode().Perm())
	}
}
