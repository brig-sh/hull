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

package bootassets

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bundleSums is the checksum file the brig-standalone-linux bundle keeps
// beside its guest kernel and initrd, in sha256sum's format.
const bundleSums = "SHA256SUMS"

// systemBundleGuest is where a system install of the bundle keeps its guest
// files. A variable, so tests can point it at a directory of their own.
var systemBundleGuest = "/var/lib/brig/data/share/guest"

// bundleGuestDirs returns where a brig-standalone-linux bundle keeps its
// guest kernel and initrd, in the order to look: the bundle installed under
// this user's home, then the one installed for the whole host. A user
// install lives under $XDG_DATA_HOME/brig/data, or ~/.local/share/brig/data,
// and a system install under /var/lib/brig/data (the bundle's install.sh).
// root has only the system install.
func bundleGuestDirs() []string {
	var dirs []string
	if os.Geteuid() != 0 {
		share := os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(share) {
			home, err := os.UserHomeDir()
			if err == nil {
				share = filepath.Join(home, ".local", "share")
			}
		}
		if share != "" {
			dirs = append(dirs, filepath.Join(share, "brig", "data", "share", "guest"))
		}
	}
	return append(dirs, systemBundleGuest)
}

// BundleGuest returns where an installed brig-standalone-linux bundle keeps
// its guest kernel and initrd, and false when no bundle is installed. It
// does not check them: VerifyBundleFile does, for whatever a run boots.
//
// The bundle builds its initrd from the urunc it installs, and records the
// urunc and urunit commits in bundle.json beside it, so its urunc boots it.
// The linux-amd64 initrd Fetch downloads boots a guest under the bundle's
// urunc that cannot find the container's command.
//
// HULL_BOOT_ASSETS still wins: it names a local build to boot instead.
func BundleGuest() (kernel, initrd string, ok bool, err error) {
	if os.Getenv("HULL_BOOT_ASSETS") != "" {
		return "", "", false, nil
	}
	for _, dir := range bundleGuestDirs() {
		_, err := os.Stat(filepath.Join(dir, bundleSums))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		return filepath.Join(dir, KernelName()), filepath.Join(dir, InitrdName), true, nil
	}
	return "", "", false, nil
}

// VerifyBundleFile checks path against the SHA256SUMS of the directory it
// is in, as a runtime bundle's guest files are checked, and reports false,
// checking nothing, when the directory has no SHA256SUMS: it is no bundle's.
func VerifyBundleFile(path string) (bool, error) {
	dir := filepath.Dir(path)
	_, err := os.Stat(filepath.Join(dir, bundleSums))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = checkBundleSums(dir, filepath.Base(path))
	if err != nil {
		return false, err
	}
	return true, nil
}

// checkBundleSums hashes each of names in dir against the bundle's
// SHA256SUMS. A file the list does not name fails the check, as one whose
// contents differ does: the bundle records both files it ships.
func checkBundleSums(dir string, names ...string) error {
	f, err := os.Open(filepath.Join(dir, bundleSums))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	want := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sum, name, ok := strings.Cut(scanner.Text(), "  ")
		if ok {
			want[name] = sum
		}
	}
	err = scanner.Err()
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Join(dir, bundleSums), err)
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		sum, ok := want[name]
		if !ok {
			return fmt.Errorf("the runtime bundle's %s does not list %s", bundleSums, path)
		}
		got, _, err := hashFile(path)
		if err != nil {
			return err
		}
		if got != sum {
			return fmt.Errorf("%s does not match the runtime bundle's %s: "+
				"sha256 %s, recorded %s. Reinstall the bundle", path, bundleSums, got, sum)
		}
	}
	return nil
}
