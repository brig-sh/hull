//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// startProcess runs argv and returns its pid together with a channel that
// receives when the process ends. Waiting on the channel rather than on
// kill(pid, 0) is deliberate: a killed child stays a zombie until it is
// reaped, and a zombie still answers signal 0, so the cheap liveness probe
// would report a process we just killed as alive.
func startProcess(t *testing.T, argv ...string) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	})
	return cmd.Process.Pid, done
}

// startFakeVMM runs a process whose argv looks like one of our VMMs, so the
// identity guard recognises it.
func startFakeVMM(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "vz-runner")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake vmm: %v", err)
	}
	return startProcess(t, script)
}

func storeWithInstance(t *testing.T, id string, state *store.InstanceState) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := s.CreateInstance(id); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if state != nil {
		if err := s.SaveInstance(state); err != nil {
			t.Fatalf("save instance: %v", err)
		}
	}
	return s
}

// The recorded pid is a number the kernel recycles. After a reboot, or after a
// VMM crashed, it names an unrelated process of this user -- and `rm --force`
// used to SIGKILL it without asking a single question about what it was.
func TestRemoveForceLeavesAnUnrelatedProcessAlone(t *testing.T) {
	pid, done := startProcess(t, "/bin/sleep", "30")
	s := storeWithInstance(t, "ghost", &store.InstanceState{
		ID: "ghost", Status: "running", PID: pid,
	})

	if err := removeInstanceIn(s, "ghost", true); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	select {
	case <-done:
		t.Fatalf("rm --force killed pid %d, which is not one of our VMMs", pid)
	case <-time.After(500 * time.Millisecond):
	}

	if _, err := s.GetInstance("ghost"); err == nil {
		t.Fatal("the stale record survived rm --force")
	}
}

// The guard must not turn --force into a no-op: a pid that really is our VMM
// still has to die, and the directory must not be deleted until it has.
func TestRemoveForceKillsTheRecordedVMM(t *testing.T) {
	pid, done := startFakeVMM(t)
	s := storeWithInstance(t, "web", &store.InstanceState{
		ID: "web", Status: "running", PID: pid,
	})

	if err := removeInstanceIn(s, "web", true); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("rm --force left the VMM (pid %d) running", pid)
	}
	if _, err := os.Stat(s.InstanceDir("web")); !os.IsNotExist(err) {
		t.Fatalf("instance directory survived: %v", err)
	}
}

// Without --force a running instance is still refused, and nothing is removed.
func TestRemoveRefusesARunningInstanceWithoutForce(t *testing.T) {
	pid, _ := startFakeVMM(t)
	s := storeWithInstance(t, "web", &store.InstanceState{
		ID: "web", Status: "running", PID: pid,
	})

	if err := removeInstanceIn(s, "web", false); err == nil {
		t.Fatal("rm removed a running instance without --force")
	}
	if _, err := s.GetInstance("web"); err != nil {
		t.Fatalf("the record was removed anyway: %v", err)
	}
}

// An instance directory with no state.json is the orphan wedge: the VMM was
// started and the process was killed before the record was written. `ps`
// cannot see it, `stop` cannot reach it, and the name stays taken forever
// because CreateInstance still fails with ErrInstanceExists. `rm` is the only
// way out, so it must clear the directory instead of answering "not found".
func TestRemoveClearsAnInstanceWithNoState(t *testing.T) {
	s := storeWithInstance(t, "wedged", nil)

	if err := removeInstanceIn(s, "wedged", false); err != nil {
		t.Fatalf("rm on a stateless instance directory: %v", err)
	}
	if _, err := os.Stat(s.InstanceDir("wedged")); !os.IsNotExist(err) {
		t.Fatalf("the wedged directory survived: %v", err)
	}
	// And the name is free again.
	if _, err := s.CreateInstance("wedged"); err != nil {
		t.Fatalf("the name is still squatted after rm: %v", err)
	}
}

// A corrupt state.json is the same wedge with a different cause: the record
// cannot be parsed, so nothing can act on the instance but rm.
func TestRemoveClearsAnInstanceWithCorruptState(t *testing.T) {
	s := storeWithInstance(t, "corrupt", nil)
	if err := os.WriteFile(filepath.Join(s.InstanceDir("corrupt"), "state.json"),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeInstanceIn(s, "corrupt", false); err != nil {
		t.Fatalf("rm on a corrupt record: %v", err)
	}
	if _, err := os.Stat(s.InstanceDir("corrupt")); !os.IsNotExist(err) {
		t.Fatalf("the corrupt instance directory survived: %v", err)
	}
}

// A name with no directory at all is still an error: recovering a wedge must
// not turn every typo into a success.
func TestRemoveStillReportsAnUnknownInstance(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := removeInstanceIn(s, "nope", true); err == nil {
		t.Fatal("rm reported success for an instance that does not exist")
	}
}
