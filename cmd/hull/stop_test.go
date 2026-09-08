//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// TestStopInstanceInLeavesAnUnrelatedProcessAlone pins the pid-identity gate
// in stopInstanceIn: a recorded pid is a number the kernel recycles. After a
// VMM crashed, or the host rebooted and reused the number, it can name a
// process that has nothing to do with hull. Without the processIsAVMM check
// stop.go relies on before signalling, `hull stop` would SIGTERM (and
// eventually SIGKILL) whatever now holds that pid -- this is stop's sibling
// to rm's TestRemoveForceLeavesAnUnrelatedProcessAlone (rm_test.go), which
// stop had no equivalent of before this test.
//
// It also pins that the record still gets its bookkeeping in that case: the
// operator asked to stop something, and the record must not silently claim
// the instance is still running just because the pid we had for it turned
// out to be a stranger.
func TestStopInstanceInLeavesAnUnrelatedProcessAlone(t *testing.T) {
	// argv[0] base name "sleep" -- not "qemu-system-*" and not in
	// vmmExecutables, so processIsAVMM must reject it by name alone.
	pid, done := startProcess(t, "/bin/sleep", "30")

	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const instance = "ghost"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "running", PID: pid,
	}); err != nil {
		t.Fatal(err)
	}

	if err := stopInstanceIn(s, instance, 2); err != nil {
		t.Fatalf("stopInstanceIn: %v", err)
	}

	// Bounded wait on the process itself, not a fixed sleep-then-check: if
	// stopInstanceIn signalled it, done closes well inside this window.
	select {
	case <-done:
		t.Fatalf("stopInstanceIn killed pid %d, which is not one of our VMMs", pid)
	case <-time.After(500 * time.Millisecond):
	}

	got, err := s.GetInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if got.PID != 0 {
		t.Errorf("PID = %d, want cleared", got.PID)
	}
}
