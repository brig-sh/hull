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

	"github.com/brig-sh/hull/pkg/store"
)

// A store the user pointed at a case-sensitive volume of their own has no
// backing image of ours. Like detach, having nothing to do is a success, so a
// cleanup step can run this unconditionally.
func TestCompactStoreWithNoImageSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := compactStore(filepath.Join(dir, "store"), "/Users/x/.hull/store", false); err != nil {
		t.Fatalf("compactStore: %v", err)
	}
}

func TestCompactStoreNeedsAStoreDir(t *testing.T) {
	if err := compactStore("", "/Users/x/.hull/store", false); err == nil {
		t.Fatal("an empty store directory was accepted")
	}
}

// Compaction unmounts the volume, and a live VM's rootfs, log and sockets are
// all on it. Detaching under one with --force leaves it running against a
// filesystem that is no longer there, so this refuses before detaching rather
// than after -- and says which instance to stop.
func TestCompactRefusesWhileAnInstanceRuns(t *testing.T) {
	instances := []*store.InstanceState{
		{ID: "two", Status: "running", PID: 4242},
		{ID: "one", Status: store.StatusStarting, PID: 4243},
		{ID: "stopped", Status: "stopped", PID: 4244},
	}
	live := func(inst *store.InstanceState) bool { return inst.Status != "stopped" }
	err := refuseRunningInstances(instances, live)
	if err == nil {
		t.Fatal("compaction was allowed while an instance was running")
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "stopped,") {
		t.Errorf("a stopped instance was reported as running: %v", err)
	}
	// Two instances, so every variable part of the sentence is plural.
	for _, want := range []string{"instances", "are running", "their root filesystems are"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not read as plural (%q): %v", want, err)
		}
	}
}

// One instance, and the same sentence has to agree with itself.
func TestCompactRefusalAgreesWithOneInstance(t *testing.T) {
	instances := []*store.InstanceState{{ID: "solo", Status: "running", PID: 4242}}
	err := refuseRunningInstances(instances, func(*store.InstanceState) bool { return true })
	if err == nil {
		t.Fatal("compaction was allowed while an instance was running")
	}
	for _, want := range []string{"instance solo is running", "its root filesystem is", "Stop it first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not read as singular (%q): %v", want, err)
		}
	}
}

// A "starting" record is written with PID 0 before the VMM is spawned. Reading
// it as stopped let compaction unmount the volume under a live guest.
func TestCompactRefusesAStartingRecordWithNoPID(t *testing.T) {
	instances := []*store.InstanceState{{ID: "spawning", Status: store.StatusStarting, PID: 0}}
	if err := refuseRunningInstances(instances, instanceMayHaveVMM); err == nil {
		t.Fatal("compaction was allowed under a record that may have spawned a VMM")
	}
}

// A record saying "running" that a crashed host left behind must not make
// compaction impossible forever.
func TestCompactIgnoresAStaleRunningRecord(t *testing.T) {
	instances := []*store.InstanceState{{ID: "stale", Status: "running", PID: stalePID}}
	if err := refuseRunningInstances(instances, instanceMayHaveVMM); err != nil {
		t.Fatalf("a record whose pid is not a VMM blocked compaction: %v", err)
	}
}

// The same rule at the compact site: a "starting" record with a pid that is
// not ours must not block compaction for good.
func TestCompactIgnoresAStartingRecordWithAStalePID(t *testing.T) {
	instances := []*store.InstanceState{
		{ID: "recycled", Status: store.StatusStarting, PID: stalePID},
	}
	if err := refuseRunningInstances(instances, instanceMayHaveVMM); err != nil {
		t.Fatalf("a starting record with a dead pid blocked compaction: %v", err)
	}
}

func TestStoreIsMountedReadsTheMarker(t *testing.T) {
	dir := t.TempDir()
	mounted, err := storeIsMounted(dir)
	if err != nil {
		t.Fatalf("storeIsMounted: %v", err)
	}
	if mounted {
		t.Fatal("a plain directory was reported as a mounted store")
	}
	if err := os.WriteFile(filepath.Join(dir, storeMarkerName), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mounted, err = storeIsMounted(dir)
	if err != nil {
		t.Fatalf("storeIsMounted: %v", err)
	}
	if !mounted {
		t.Fatal("the marker was not recognised")
	}
}

func TestFileDiskUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	if err := os.WriteFile(path, make([]byte, 128*1024), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := fileDiskUsage(path); got < 128*1024 {
		t.Errorf("fileDiskUsage = %d, want at least the file's size", got)
	}
	if got := fileDiskUsage(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Errorf("fileDiskUsage of a missing file = %d", got)
	}
}

// A store full of stopped instances compacts: that is the state a machine is
// in after a day's work, and it is exactly when reclaiming space is wanted.
func TestCompactAllowsStoppedInstances(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := s.CreateInstance(id); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
		if err := s.SaveInstance(&store.InstanceState{ID: id, Status: "stopped"}); err != nil {
			t.Fatalf("SaveInstance: %v", err)
		}
	}
	if err := refuseWhileInstancesRun(dir); err != nil {
		t.Fatalf("stopped instances blocked compaction: %v", err)
	}
}
