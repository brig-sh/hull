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

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// A portable instance may move between an Apple host and a Linux arm64
// host; a native one only to the same kind of host, and neither across
// architectures.
func TestHostKindMismatch(t *testing.T) {
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	same := &migrateProbe{OS: runtime.GOOS, Arch: runtime.GOARCH}
	otherOS := &migrateProbe{OS: other, Arch: runtime.GOARCH}
	otherArch := &migrateProbe{OS: runtime.GOOS, Arch: "riscv64"}
	if why := hostKindMismatch(same, ""); len(why) != 0 {
		t.Errorf("the same host kind: %v", why)
	}
	if why := hostKindMismatch(otherOS, ""); len(why) != 1 || !strings.Contains(why[0], portable) {
		t.Errorf("native to another OS: %v", why)
	}
	if why := hostKindMismatch(otherOS, portable); len(why) != 0 {
		t.Errorf("portable to another OS: %v", why)
	}
	if why := hostKindMismatch(otherArch, portable); len(why) != 1 {
		t.Errorf("portable to another architecture: %v", why)
	}
}

// The destination records the profile the source ran under, so its ps,
// inspect and next move see the same machine.
func TestIncomingInstanceKeepsTheProfile(t *testing.T) {
	s := newCmdTestStore(t)
	rec := &migrationRecord{TxID: "tx-profile", Role: roleDestination, Name: "vm-p",
		BundleDir: filepath.Join(s.InstanceDir("vm-p"), "checkpoint", "tx-profile"),
		Disk:      &migrateDisk{Path: filepath.Join(s.InstanceDir("vm-p"), "bundle", "rootfs.ext4")},
		Spec:      &migrateSpec{SourceHost: "src", Net: "none", Profile: portable}}
	if _, err := s.CreateInstance("vm-p"); err != nil {
		t.Fatal(err)
	}
	if err := writeIncomingInstance(s, rec, "/bin/hvi", "/root"); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetInstance("vm-p")
	if err != nil {
		t.Fatal(err)
	}
	if state.Profile != portable || state.Migration == nil ||
		state.Migration.State != store.MigrationIncoming {
		t.Fatalf("incoming instance = profile %q, migration %+v", state.Profile, state.Migration)
	}
	if slices.Contains(state.CmdLine, "--profile") {
		t.Fatalf("the restore command line carries --profile: %v", state.CmdLine)
	}
}

// A portable instance is not fenced for a destination whose hvi has no
// machine profiles: the probe refuses it first. A native instance still
// moves to that hvi.
func TestMovePortableNeedsAnHviWithProfiles(t *testing.T) {
	f := newMoveFixture(t, true)
	noProfiles := filepath.Join(t.TempDir(), "hvi")
	script := "#!/bin/sh\n" +
		`[ "$1 $2" = "boot --profile" ] && { echo 'hvi: unknown boot arg "--profile"' >&2; exit 1; }` + "\n" +
		`exec "` + f.hvi + `" "$@"` + "\n"
	if err := os.WriteFile(noProfiles, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	state := mustInstance(t, f.src, f.name)
	state.Profile = portable
	if err := f.src.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	peer := newLocalPeer(f.dst)
	opt := f.options(false)
	opt.remote.Hvi = noProfiles
	err := sourceMigrate(context.Background(), f.src, f.name, opt, peer)
	if err == nil || !strings.Contains(err.Error(), "has no machine profiles") {
		t.Fatalf("err = %v, want the probe's refusal", err)
	}
	if slices.Contains(f.vm.ops(), "checkpoint") {
		t.Fatal("the source was fenced for a destination that cannot restore it")
	}
	if _, err := os.Stat(f.src.InstanceMigrationRecord(f.name)); !os.IsNotExist(err) {
		t.Fatal("a refused probe left a move record")
	}

	state.Profile = ""
	if err := f.src.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	opt.dryRun = true
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatalf("a native instance to the same hvi: %v", err)
	}
}
