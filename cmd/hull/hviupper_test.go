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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// HULL_HVI_ROOTFS_UPPER applies to an hvi container boot with a virtiofs root.
// Anywhere else it is ignored, and a value it does not know fails the run
// rather than booting the clone.
func TestHviRootfsUpper(t *testing.T) {
	cases := []struct {
		name, env  string
		boot       bool
		vmm        hypervisors.VmmType
		rootfsType string
		want       string
		wantErr    bool
	}{
		{"unset", "", true, hypervisors.HviVmm, "", "", false},
		{"virtiofs on hvi", "virtiofs", true, hypervisors.HviVmm, "", "virtiofs", false},
		{"block on hvi", "block", true, hypervisors.HviVmm, "virtiofs", "block", false},
		{"typo", "blok", true, hypervisors.HviVmm, "", "", true},
		{"typo where it does not apply", "blok", true, hypervisors.VzVmm, "", "", true},
		{"vz", "block", true, hypervisors.VzVmm, "", "", false},
		{"unikernel", "virtiofs", false, hypervisors.HviVmm, "", "", false},
		{"block rootfs", "virtiofs", true, hypervisors.HviVmm, "block", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HULL_HVI_ROOTFS_UPPER", c.env)
			got, err := hviRootfsUpper(c.boot, c.vmm, c.rootfsType)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want an error: %t", err, c.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), c.env) {
				t.Errorf("error %q does not name the value %q", err, c.env)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The upper holds everything the guest writes, so only its owner may read it,
// whatever the umask left on the file mke2fs created.
func TestRootfsUpperBlockIsPrivate(t *testing.T) {
	if _, err := os.Stat(mke2fsBin); err != nil {
		t.Skipf("needs e2fsprogs: %v", err)
	}
	disk, err := prepareRootfsUpper(t.TempDir(), "block")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("upper disk %s is %v, want 0600", filepath.Base(disk), got)
	}
}
