// Copyright (c) 2023-2026, Nubificus LTD
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
