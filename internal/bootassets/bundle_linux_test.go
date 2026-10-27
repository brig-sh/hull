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
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundleGuest lays out a bundle's guest directory under dir, with a
// SHA256SUMS that lists the kernel and initrd as sums gives them, or as
// their contents hash when sums has no entry for one.
func writeBundleGuest(t *testing.T, dir string, sums map[string]string) {
	t.Helper()
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	var list strings.Builder
	for _, name := range []string{KernelName(), InitrdName} {
		data := []byte("contents of " + name)
		err = os.WriteFile(filepath.Join(dir, name), data, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		sum, ok := sums[name]
		if !ok {
			sum = fmt.Sprintf("%x", sha256.Sum256(data))
		}
		if sum != "" {
			fmt.Fprintf(&list, "%s  %s\n", sum, name)
		}
	}
	err = os.WriteFile(filepath.Join(dir, bundleSums), []byte(list.String()), 0o644)
	if err != nil {
		t.Fatal(err)
	}
}

// bundleHost gives the test a home and a system bundle directory of its
// own, with no bundle installed in either.
func bundleHost(t *testing.T) (userGuest, systemGuest string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HULL_BOOT_ASSETS", "")
	old := systemBundleGuest
	systemBundleGuest = filepath.Join(t.TempDir(), "guest")
	t.Cleanup(func() { systemBundleGuest = old })
	return filepath.Join(home, ".local", "share", "brig", "data", "share", "guest"), systemBundleGuest
}

func TestBundleGuest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root looks only at the system install")
	}
	t.Run("none installed", func(t *testing.T) {
		bundleHost(t)
		_, _, ok, err := BundleGuest()
		if err != nil || ok {
			t.Errorf("BundleGuest = %v, %v; want no bundle", ok, err)
		}
	})
	t.Run("user install first", func(t *testing.T) {
		user, system := bundleHost(t)
		writeBundleGuest(t, user, nil)
		writeBundleGuest(t, system, nil)
		kernel, initrd, ok, err := BundleGuest()
		if err != nil || !ok || kernel != filepath.Join(user, KernelName()) || initrd != filepath.Join(user, InitrdName) {
			t.Errorf("BundleGuest = %q, %q, %v, %v; want the user install's files", kernel, initrd, ok, err)
		}
	})
	t.Run("system install", func(t *testing.T) {
		_, system := bundleHost(t)
		writeBundleGuest(t, system, nil)
		kernel, _, ok, err := BundleGuest()
		if err != nil || !ok || kernel != filepath.Join(system, KernelName()) {
			t.Errorf("BundleGuest = %q, %v, %v; want the system install's kernel", kernel, ok, err)
		}
	})
	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		bundleHost(t)
		share := t.TempDir()
		t.Setenv("XDG_DATA_HOME", share)
		guest := filepath.Join(share, "brig", "data", "share", "guest")
		writeBundleGuest(t, guest, nil)
		kernel, _, ok, err := BundleGuest()
		if err != nil || !ok || kernel != filepath.Join(guest, KernelName()) {
			t.Errorf("BundleGuest = %q, %v, %v; want the kernel under XDG_DATA_HOME", kernel, ok, err)
		}
	})
	t.Run("changed initrd", func(t *testing.T) {
		user, _ := bundleHost(t)
		writeBundleGuest(t, user, map[string]string{InitrdName: strings.Repeat("0", 64)})
		_, _, ok, err := BundleGuest()
		if err != nil || !ok {
			t.Errorf("BundleGuest = %v, %v; want the files found, and checked only by VerifyBundleFile", ok, err)
		}
	})
	t.Run("HULL_BOOT_ASSETS wins", func(t *testing.T) {
		user, _ := bundleHost(t)
		writeBundleGuest(t, user, nil)
		t.Setenv("HULL_BOOT_ASSETS", t.TempDir())
		_, _, ok, err := BundleGuest()
		if err != nil || ok {
			t.Errorf("BundleGuest = %v, %v; want the bundle passed over", ok, err)
		}
	})
}

func TestVerifyBundleFile(t *testing.T) {
	dir := t.TempDir()
	writeBundleGuest(t, dir, nil)
	kernel, initrd := filepath.Join(dir, KernelName()), filepath.Join(dir, InitrdName)
	ok, err := VerifyBundleFile(kernel)
	if err != nil || !ok {
		t.Errorf("VerifyBundleFile = %v, %v; want the bundle's kernel accepted", ok, err)
	}

	err = os.WriteFile(initrd, []byte("changed"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = VerifyBundleFile(initrd)
	if err == nil || ok || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("changed initrd: VerifyBundleFile = %v, %v; want a mismatch", ok, err)
	}

	unlisted := t.TempDir()
	writeBundleGuest(t, unlisted, map[string]string{KernelName(): ""})
	ok, err = VerifyBundleFile(filepath.Join(unlisted, KernelName()))
	if err == nil || ok || !strings.Contains(err.Error(), "does not list") {
		t.Errorf("unlisted kernel: VerifyBundleFile = %v, %v; want the missing entry reported", ok, err)
	}

	elsewhere := filepath.Join(t.TempDir(), "kernel")
	ok, err = VerifyBundleFile(elsewhere)
	if err != nil || ok {
		t.Errorf("a file outside any bundle: VerifyBundleFile = %v, %v; want false, nothing checked", ok, err)
	}
}
