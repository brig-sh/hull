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
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urunc-dev/urunc/pkg/agentproto"
)

func TestSendEndOnceIsIdempotent(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &store.InstanceState{
		ID:        "end-once",
		Backend:   "vz",
		StartTime: time.Now().Add(-5 * time.Minute),
	}
	if _, err := s.CreateInstance(st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}

	// telemetryClient is nil here, so Send is a no-op; we assert the
	// persisted guard, which is the exactly-once contract.
	sendEndOnce(s, st)
	if !st.TelemetryEndSent {
		t.Fatal("first sendEndOnce must set the guard")
	}
	reloaded, err := s.GetInstance(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TelemetryEndSent {
		t.Fatal("the guard must be persisted, not just in-memory")
	}

	// A second call (retried stop, or foreground exit after stop) must
	// not re-fire: it returns before touching state.
	before := reloaded.TelemetryEndSent
	sendEndOnce(s, reloaded)
	if reloaded.TelemetryEndSent != before {
		t.Fatal("second sendEndOnce must be a no-op")
	}
}

func TestErrorClassIsCoarseAndStable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "canceled"},
		{"not found", fmt.Errorf("open x: %w", os.ErrNotExist), "not-found"},
		{"permission", fmt.Errorf("open y: %w", os.ErrPermission), "permission"},
		{"network", &net.OpError{Op: "dial", Err: fmt.Errorf("refused")}, "network"},
		{"anything else", fmt.Errorf("instance not found: /Users/someone/secret"), "other"},
	}
	for _, tc := range cases {
		if got := errorClass(tc.err); got != tc.want {
			t.Errorf("%s: errorClass = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadyEventFieldsAreWholeMilliseconds(t *testing.T) {
	cases := []struct {
		name  string
		since time.Duration
		want  string
	}{
		{"sub-millisecond truncates to 0", 900 * time.Microsecond, "0"},
		{"whole", 1234 * time.Millisecond, "1234"},
		{"fraction is truncated, not rounded", 1234*time.Millisecond + 999*time.Microsecond, "1234"},
		{"a stepped clock reports 0, never negative", -3 * time.Second, "0"},
	}
	for _, tc := range cases {
		got := readyEventFields("hvi", tc.since)
		if got["ready_ms"] != tc.want {
			t.Errorf("%s: ready_ms = %q, want %q", tc.name, got["ready_ms"], tc.want)
		}
		if got["backend"] != "hvi" {
			t.Errorf("%s: backend = %q, want hvi", tc.name, got["backend"])
		}
		// The schema page lists exactly these two fields; anything else
		// here is an undocumented collection.
		if len(got) != 2 {
			t.Errorf("%s: unexpected fields in ready event: %v", tc.name, got)
		}
	}
}

// fakeRefusingAgent is an agent that is up but refuses every command with
// an Error frame -- a minimal rootfs with no /bin/true answers this way.
func fakeRefusingAgent(t *testing.T, sockPath string) {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake agent listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if _, err := agentproto.ReadFrame(c); err != nil {
					return
				}
				if _, err := agentproto.ReadFrame(c); err != nil {
					return
				}
				_ = agentproto.WriteJSON(c, agentproto.TypeError, 1,
					agentproto.Error{Message: "exec: /bin/true: no such file"})
			}(conn)
		}
	}()
}

func TestProbeGuestReadyAbsorbsTheStartupWindow(t *testing.T) {
	s := seedRunning(t, "ready1")
	// Two bridge-up-but-guest-not-listening EOFs, then a real answer: the
	// probe must ride out the window and report the guest as up.
	fakeAgent(t, s.InstanceAgentSocket("ready1"), 2, 0)
	if !probeGuestReady(s, "ready1", 10*time.Second) {
		t.Fatal("probe gave up on an agent that answered after two EOFs")
	}
}

func TestProbeGuestReadyCountsARefusalAsUp(t *testing.T) {
	s := seedRunning(t, "ready2")
	fakeRefusingAgent(t, s.InstanceAgentSocket("ready2"))
	if !probeGuestReady(s, "ready2", 10*time.Second) {
		t.Fatal("an agent that answers with an error is up; the probe must say so")
	}
}

func TestProbeGuestReadyGivesUpWithinBudget(t *testing.T) {
	s := seedRunning(t, "ready3")
	// Every connection EOFs: the host bridge is up and the guest never
	// binds. The probe must stop at its budget and report no answer, so
	// the caller sends nothing rather than a made-up number.
	fakeAgent(t, s.InstanceAgentSocket("ready3"), 1<<30, 0)
	began := time.Now()
	if probeGuestReady(s, "ready3", 2*time.Second) {
		t.Fatal("probe reported ready for an agent that never answered")
	}
	if took := time.Since(began); took > 6*time.Second {
		t.Fatalf("probe overran its 2s budget: took %v", took)
	}
}

func TestProbeGuestReadyStopsWhenTheInstanceIsGone(t *testing.T) {
	s := seedRunning(t, "ready4")
	st, err := s.GetInstance("ready4")
	if err != nil {
		t.Fatal(err)
	}
	st.Status = "stopped"
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}
	// A VM that exited before the guest came up is not a transport
	// failure to retry against: the probe must return at once, unready.
	began := time.Now()
	if probeGuestReady(s, "ready4", time.Minute) {
		t.Fatal("probe reported a stopped instance as ready")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("probe kept retrying a stopped instance: took %v", took)
	}
}
