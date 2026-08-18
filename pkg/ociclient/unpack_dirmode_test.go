// Copyright (c) 2023-2026, Nubificus LTD
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

package ociclient

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A directory only has to be widened for as long as the removal takes, and the
// widening must not outlive it.
//
// removeForReplace used to relax the parent by 0300 and leave it that way. That
// is invisible whenever a later layer re-states the directory, because
// applyDirModes then puts the image's mode back -- but an image is free to ship
// a 0500 directory in its base layer, replace one file inside it in a later
// layer, and never mention the directory again. Nothing corrects it then, and
// the rootfs the guest boots has a 0700 directory the image never asked for.
// vz reports the host's mode straight through to the guest, so this is a
// permission change the workload can observe.
func TestRemoveForReplaceRestoresTheParentMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode fs.FileMode
	}{
		// Every mode here must be missing 0300, or removeForReplace returns
		// before it ever widens anything and the case proves nothing. That is
		// what the sticky and setgid rows used to do: 0777 and 0750 both
		// already contain 0300, so the first Remove succeeded, the restore
		// path was never entered, and dropping setuid/setgid/sticky from the
		// mask the restore uses -- the reason this test exists -- passed the
		// whole package.
		{"read-only directory", 0o500},
		{"sticky read-only directory", 0o500 | fs.ModeSticky},
		{"setgid read-only directory", 0o500 | fs.ModeSetgid},
		{"setuid read-only directory", 0o500 | fs.ModeSetuid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "d")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("old"), 0o444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}

			root, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			if err := removeForReplace(root, filepath.Join("d", "f")); err != nil {
				t.Fatalf("removeForReplace: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
				t.Fatalf("the entry was not removed: %v", err)
			}

			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			if got != tc.mode {
				t.Errorf("parent is %v after the replace, want %v: the widening leaked "+
					"into the rootfs the guest boots", got, tc.mode)
			}
		})
	}
}
