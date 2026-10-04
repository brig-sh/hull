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

package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// legacyHome gives a test a home of its own, with a state file at the old
// location inside the default store when one is given.
func legacyHome(t *testing.T, old *state) (newDir, oldDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	newDir, oldDir = filepath.Join(home, ".hull"), filepath.Join(home, ".hull", "store")
	if old != nil {
		if err := os.MkdirAll(oldDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeState(oldDir, old); err != nil {
			t.Fatal(err)
		}
	}
	return newDir, oldDir
}

func TestDefaultStateDirIsOutsideTheStore(t *testing.T) {
	newDir, _ := legacyHome(t, nil)
	if got := DefaultStateDir(); got != newDir {
		t.Fatalf("DefaultStateDir = %q, want %q", got, newDir)
	}
}

// The install id and the answer move once, from the store to its parent. A no
// stays a no, whatever version it answered.
func TestLegacyAnswerMovesOutOfTheStore(t *testing.T) {
	no := false
	newDir, _ := legacyHome(t, &state{InstallID: "11111111-2222-4333-8444-555555555555", Consent: &no, ConsentVersion: 1})

	c := Init(Config{StoreDir: newDir})

	if c.Enabled() || c.InstallID() != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("enabled = %v, install id = %q; want the old no and the old id", c.Enabled(), c.InstallID())
	}
	if _, err := os.Stat(statePath(newDir)); err != nil {
		t.Fatalf("the state did not move: %v", err)
	}
}

// A yes to the old consent version came with the old list, and is asked again
// like any other.
func TestLegacyYesToAnOlderVersionIsAskedAgain(t *testing.T) {
	yes := true
	newDir, _ := legacyHome(t, &state{InstallID: newUUID(), Consent: &yes, ConsentVersion: 1})
	if got, _ := Effective(newDir); got != Outdated {
		t.Fatalf("Effective = %v, want Outdated", got)
	}
	if _, err := os.Stat(statePath(newDir)); !os.IsNotExist(err) {
		t.Fatalf("asking for the answer moved the state: %v", err)
	}
}

// A hull from before the move reads the old file. A no recorded now has to
// reach it too.
func TestOptOutReachesTheLegacyFile(t *testing.T) {
	yes := true
	newDir, oldDir := legacyHome(t, &state{InstallID: newUUID(), Consent: &yes, ConsentVersion: 1})

	if err := SetConsent(newDir, false); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(statePath(oldDir))
	if err != nil {
		t.Fatal(err)
	}
	var old state
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if old.Consent == nil || *old.Consent {
		t.Fatalf("the old file still reads %s", data)
	}
}

// Only the default store had its state moved. Any other directory keeps its
// own, and a store/ under it is nobody's old state.
func TestOtherDirectoriesHaveNoLegacyState(t *testing.T) {
	legacyHome(t, nil)
	dir := t.TempDir()
	no := false
	if err := os.MkdirAll(filepath.Join(dir, "store"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(dir, "store"), &state{InstallID: "11111111-2222-4333-8444-555555555555", Consent: &no}); err != nil {
		t.Fatal(err)
	}
	if c := Init(Config{StoreDir: dir}); c.InstallID() == "11111111-2222-4333-8444-555555555555" {
		t.Fatal("a directory other than the default one adopted state from under it")
	}
}

// An older hull reads and writes only the old file. A no it records after the
// move has to stop this hull and brig too.
func TestALaterNoFromAnOlderHullIsRead(t *testing.T) {
	newDir, oldDir := legacyHome(t, nil)
	if err := SetConsent(newDir, true); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeState(oldDir, &state{InstallID: newUUID(), Consent: &no, ConsentVersion: 1}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(statePath(oldDir), later, later); err != nil {
		t.Fatal(err)
	}

	if got, _ := Effective(newDir + "/"); got != Off {
		t.Fatalf("Effective = %v, want Off", got)
	}
	if c := Init(Config{StoreDir: newDir}); c.Enabled() {
		t.Fatal("a later no from an older hull left telemetry on")
	}
}

// The other way round, a yes given here after an old no is the answer.
func TestAnEarlierLegacyNoDoesNotOverrideALaterYes(t *testing.T) {
	no := false
	newDir, oldDir := legacyHome(t, &state{InstallID: newUUID(), Consent: &no, ConsentVersion: 1})
	earlier := time.Now().Add(-time.Minute)
	if err := os.Chtimes(statePath(oldDir), earlier, earlier); err != nil {
		t.Fatal(err)
	}
	if err := SetConsent(newDir, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := Effective(newDir); got != On {
		t.Fatalf("Effective = %v, want On", got)
	}
}
