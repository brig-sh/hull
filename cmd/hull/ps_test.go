//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// psRow returns the rendered row for id.
func psRow(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	var out bytes.Buffer
	if err := renderInstances(s, &out); err != nil {
		t.Fatalf("ps: %v", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, id+" ") || strings.HasPrefix(line, id+"\t") {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", id, out.String())
	return ""
}

// `ps` used to ask only whether *some* process holds the recorded pid. Pid
// numbers are recycled, so after a reboot a dead sandbox whose number was
// handed to an unrelated process reported "running" forever -- and never got
// reaped, because the reconciliation hangs off the same probe. brig's
// Running() parses this table.
func TestPsReportsStoppedWhenThePidIsNotOurVMM(t *testing.T) {
	pid, _ := startProcess(t, "/bin/sleep", "30")
	s := storeWithInstance(t, "ghost", &store.InstanceState{
		ID: "ghost", Status: "running", PID: pid,
	})

	row := psRow(t, s, "ghost")
	if !strings.Contains(row, "stopped") {
		t.Fatalf("a recycled pid still reports as running: %q", row)
	}

	// The reconciliation must be persisted, not just printed.
	state, err := s.GetInstance("ghost")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "stopped" || state.PID != 0 {
		t.Fatalf("record not reaped: status=%q pid=%d", state.Status, state.PID)
	}
	if state.ExitedAt.IsZero() {
		t.Fatal("record was reaped without an exit time")
	}
}

// The stricter check must not reap live instances: a pid that really is our
// VMM stays running.
func TestPsKeepsALiveVMMRunning(t *testing.T) {
	pid, _ := startFakeVMM(t)
	s := storeWithInstance(t, "web", &store.InstanceState{
		ID: "web", Status: "running", PID: pid,
	})

	row := psRow(t, s, "web")
	if !strings.Contains(row, "running") {
		t.Fatalf("a live VMM was reported as stopped: %q", row)
	}
	state, err := s.GetInstance("web")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" || state.PID != pid {
		t.Fatalf("a live instance was reaped: status=%q pid=%d", state.Status, state.PID)
	}
}
