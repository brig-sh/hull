//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// A state file that is not JSON still occupies the name. ps has to show
// that, and it must not write a stub back over the only copy of the record.
func TestPsShowsAnUnreadableRecordWithoutRewritingIt(t *testing.T) {
	s := storeWithInstance(t, "broken", &store.InstanceState{
		ID: "broken", Status: "running", PID: 1,
	})
	path := filepath.Join(s.InstanceDir("broken"), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	row := psRow(t, s, "broken")
	if !strings.Contains(row, store.StatusUnreadable) || strings.Contains(row, "running") {
		t.Fatalf("broken record: %q", row)
	}
	// CREATED has no time to show. Year 1 reads as a real timestamp; "-"
	// matches the empty IP cell.
	if strings.Contains(row, "0001-01-01") || !strings.HasSuffix(strings.TrimRight(row, " \t"), "-") {
		t.Fatalf("unreadable CREATED column: %q", row)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "{not json" {
		t.Fatalf("ps rewrote the broken record: %q", after)
	}
}
