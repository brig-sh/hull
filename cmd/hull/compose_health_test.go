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

// healthProbeBudget, waitHealthyExec and runServiceHooks are darwin-only
// (exec_compose.go carries //go:build darwin), so this file does too, same
// as the other cmd/hull test files that reach into that half of the
// package. It could not be run on this host; see the task report for the
// reasoned mutations that would make each test fail.

package main

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urunc-dev/urunc/pkg/agentproto"
)

func durPtr(d time.Duration) *types.Duration {
	td := types.Duration(d)
	return &td
}

func u64Ptr(n uint64) *uint64 { return &n }

// TestHealthProbeBudgetDefaults pins healthProbeBudget's exec-healthcheck
// defaults (30s interval, 30s timeout, 3 retries) against the comment at
// exec_compose.go:81-88 ("docker-ish defaults ... 30s interval and timeout,
// 3 retries per the compose spec"). Unlike x-healthcheck-tcp's ProbeBudget
// (internal/compose/extensions.go, defaults published in docs/compose.md:105-106),
// docs/compose.md and docs/compose-support.md do not state these exec-form
// numbers anywhere; this test pins them as current behavior, not a
// documented contract. A regression here silently changes how long a
// `healthcheck:`-gated dependency is waited on before compose gives up.
func TestHealthProbeBudgetDefaults(t *testing.T) {
	cases := []struct {
		name                                 string
		hc                                   *types.HealthCheckConfig
		wantInterval, wantTimeout, wantTotal time.Duration
	}{
		{
			name:         "nil healthcheck uses full defaults",
			hc:           nil,
			wantInterval: 30 * time.Second,
			wantTimeout:  30 * time.Second,
			wantTotal:    180 * time.Second,
		},
		{
			name:         "non-nil but all fields unset matches nil",
			hc:           &types.HealthCheckConfig{},
			wantInterval: 30 * time.Second,
			wantTimeout:  30 * time.Second,
			wantTotal:    180 * time.Second,
		},
		{
			name:         "interval overridden, timeout and retries default",
			hc:           &types.HealthCheckConfig{Interval: durPtr(2 * time.Second)},
			wantInterval: 2 * time.Second,
			wantTimeout:  30 * time.Second,
			wantTotal:    (2*time.Second + 30*time.Second) * 3,
		},
		{
			// *Retries != nil but *Retries == 0 does not override: the guard
			// is "> 0", so an explicit zero still yields the default of 3.
			name:         "explicit zero retries pointer still defaults to 3",
			hc:           &types.HealthCheckConfig{Retries: u64Ptr(0)},
			wantInterval: 30 * time.Second,
			wantTimeout:  30 * time.Second,
			wantTotal:    180 * time.Second,
		},
		{
			name: "start period adds to the total untouched",
			hc: &types.HealthCheckConfig{
				StartPeriod: durPtr(9 * time.Second),
				Interval:    durPtr(time.Second),
				Timeout:     durPtr(time.Second),
				Retries:     u64Ptr(2),
			},
			wantInterval: time.Second,
			wantTimeout:  time.Second,
			wantTotal:    9*time.Second + (time.Second+time.Second)*2,
		},
		{
			name:         "negative interval treated as unset",
			hc:           &types.HealthCheckConfig{Interval: durPtr(-5 * time.Second)},
			wantInterval: 30 * time.Second,
			wantTimeout:  30 * time.Second,
			wantTotal:    180 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interval, timeout, total := healthProbeBudget(tc.hc)
			if interval != tc.wantInterval {
				t.Errorf("interval = %v, want %v", interval, tc.wantInterval)
			}
			if timeout != tc.wantTimeout {
				t.Errorf("timeout = %v, want %v", timeout, tc.wantTimeout)
			}
			if total != tc.wantTotal {
				t.Errorf("total = %v, want %v", total, tc.wantTotal)
			}
		})
	}
}

// probeAgent is fakeAgent (oneshot_prewarm_test.go:40) with one addition:
// the accepted-connection count is returned to the caller instead of being
// kept private, because several tests below need to assert on retry counts,
// not just the terminal outcome. Like fakeAgent, the counter tracks
// ACCEPTED CONNECTIONS, not completed sessions -- a transport-level failure
// (closed before any frame is read) still counts as one accept.
func probeAgent(t *testing.T, sockPath string, eofs int, code int) *int32 {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("probe agent listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var accepted int32
	go func() {
		seen := 0
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			seen++
			atomic.AddInt32(&accepted, 1)
			if seen <= eofs {
				_ = conn.Close()
				continue
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if _, err := agentproto.ReadFrame(c); err != nil {
					return
				}
				if _, err := agentproto.ReadFrame(c); err != nil {
					return
				}
				_ = agentproto.WriteJSON(c, agentproto.TypeExit, 1, agentproto.Exit{Code: code})
			}(conn)
		}
	}()
	return &accepted
}

func execHealthCheck() *types.HealthCheckConfig {
	return &types.HealthCheckConfig{Test: []string{"CMD", "/bin/true"}}
}

// TestWaitHealthyExecRetriesTransportFailureThenSucceeds proves the
// contract stated at exec_compose.go:207-213: a probe that "cannot run"
// (transport failure -- the agent not listening yet) is a failed attempt,
// not a fatal error, so waitHealthyExec must keep polling and report
// healthy once the agent comes up. If waitHealthyExec instead treated the
// first transport error as fatal (e.g. returning immediately instead of
// looping), this test would see a non-nil error and probeAgent would have
// accepted only 1 connection instead of 3.
func TestWaitHealthyExecRetriesTransportFailureThenSucceeds(t *testing.T) {
	s := seedRunning(t, "hook-retry")
	accepted := probeAgent(t, s.InstanceAgentSocket("hook-retry"), 2, 0)

	hc := execHealthCheck()
	hc.Interval = durPtr(5 * time.Millisecond)
	hc.Timeout = durPtr(50 * time.Millisecond)
	hc.Retries = u64Ptr(5)

	if err := waitHealthyExec(s, "hook-retry", hc); err != nil {
		t.Fatalf("waitHealthyExec: %v, want nil (2 transport EOFs then a real exit-0 session)", err)
	}
	if got := atomic.LoadInt32(accepted); got != 3 {
		t.Fatalf("accepted connections = %d, want 3 (2 failed attempts + the one that succeeded)", got)
	}
}

// TestWaitHealthyExecGivesUpOnPersistentTransportFailure is the
// discriminating arm for the test above: without it, a waitHealthyExec that
// simply ignored every transport error and returned nil unconditionally
// would also pass the retry test. A probe that never succeeds must
// eventually surface an error once the budget is spent.
func TestWaitHealthyExecGivesUpOnPersistentTransportFailure(t *testing.T) {
	s := seedRunning(t, "hook-never")
	probeAgent(t, s.InstanceAgentSocket("hook-never"), 1<<30, 0)

	hc := execHealthCheck()
	hc.Interval = durPtr(5 * time.Millisecond)
	hc.Timeout = durPtr(20 * time.Millisecond)
	hc.Retries = u64Ptr(3)

	start := time.Now()
	err := waitHealthyExec(s, "hook-never", hc)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitHealthyExec: want an error, got nil for an agent that never accepts a real session")
	}
	if !errors.Is(err, errAgentTransport) {
		t.Fatalf("error = %v, want it to wrap errAgentTransport", err)
	}
	// Budget is (5ms+20ms)*3 = 75ms; bound generously to catch a budget that
	// is silently ignored (e.g. an accidental infinite retry loop).
	if elapsed > time.Second {
		t.Fatalf("waitHealthyExec took %v, want well under the ~75ms budget plus scheduling slack", elapsed)
	}
}

// TestWaitHealthy drives the real waitHealthy (compose.go:1725) against a
// stub http.Server on a unix socket, the seam the function's plain-argument
// signature (apiSock, addr string) already provides.
//
// The task brief for this test also asked for a "context cancelled while
// probing" case. waitHealthy takes no context.Context -- its only
// stop condition is the timeout/deadline argument -- so that case does not
// apply to the real signature. The closest equivalent, and what actually
// matters operationally, is exercised by neverHealthy below: a deadline
// that is not honored would either hang or run far past its budget.
func TestWaitHealthy(t *testing.T) {
	start := func(t *testing.T, h http.HandlerFunc) (sock, addr string) {
		t.Helper()
		dir := t.TempDir()
		sock = filepath.Join(dir, "gw.sock")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		srv := &http.Server{Handler: h}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
		return sock, "10.87.0.5:9999"
	}

	t.Run("healthy on first probe", func(t *testing.T) {
		var calls int32
		sock, addr := start(t, func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusOK)
		})
		if err := waitHealthy(sock, addr, 5*time.Millisecond, 200*time.Millisecond); err != nil {
			t.Fatalf("waitHealthy: %v, want nil", err)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("probe count = %d, want exactly 1 (retried past a first success)", got)
		}
	})

	t.Run("healthy only after several probes, proving it retries", func(t *testing.T) {
		const wantCalls = 4
		var calls int32
		sock, addr := start(t, func(w http.ResponseWriter, _ *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n < wantCalls {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		// Budget (200ms) comfortably outlives 3 failed 5ms-interval probes.
		if err := waitHealthy(sock, addr, 5*time.Millisecond, 200*time.Millisecond); err != nil {
			t.Fatalf("waitHealthy: %v, want nil", err)
		}
		if got := atomic.LoadInt32(&calls); got != wantCalls {
			t.Fatalf("probe count = %d, want exactly %d", got, wantCalls)
		}
	})

	t.Run("never healthy: gives up, names the address, returns promptly", func(t *testing.T) {
		var calls int32
		sock, addr := start(t, func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		t0 := time.Now()
		err := waitHealthy(sock, addr, 5*time.Millisecond, 30*time.Millisecond)
		elapsed := time.Since(t0)

		if err == nil {
			t.Fatal("waitHealthy: want an error, the probe never returns 200")
		}
		if !strings.Contains(err.Error(), addr) {
			t.Fatalf("error = %q, want it to name the probed address %q", err.Error(), addr)
		}
		if atomic.LoadInt32(&calls) < 1 {
			t.Fatal("probe count = 0, want at least one attempt before giving up")
		}
		// A budget of 30ms should never take anywhere near a second; a
		// deadline that were silently ignored would run far longer (or hang).
		if elapsed > time.Second {
			t.Fatalf("waitHealthy took %v to give up on a 30ms budget, want it to return promptly", elapsed)
		}
	})
}

// TestRunServiceHooksRejectsEmptyCommand pins the pre-network validation at
// exec_compose.go:246-248: a hook with no command is rejected before ever
// dialing the guest agent.
func TestRunServiceHooksRejectsEmptyCommand(t *testing.T) {
	s := seedRunning(t, "hook-empty")
	err := runServiceHooks(s, "hook-empty", "web", "post_start", []types.ServiceHook{{}})
	if err == nil {
		t.Fatal("runServiceHooks: want an error for a hook with no command")
	}
	if !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("error = %q, want it to say a command is required", err.Error())
	}
}

// TestRunServiceHooksAllSucceed proves every hook in the list actually runs
// (not just that the function returns nil) by requiring the fake agent to
// have accepted one connection per hook.
func TestRunServiceHooksAllSucceed(t *testing.T) {
	s := seedRunning(t, "hook-ok")
	accepted := probeAgent(t, s.InstanceAgentSocket("hook-ok"), 0, 0)

	hooks := []types.ServiceHook{
		{Command: []string{"/bin/true"}},
		{Command: []string{"/bin/true"}},
	}
	if err := runServiceHooks(s, "hook-ok", "web", "post_start", hooks); err != nil {
		t.Fatalf("runServiceHooks: %v, want nil", err)
	}
	if got := atomic.LoadInt32(accepted); got != 2 {
		t.Fatalf("accepted connections = %d, want 2 (one per hook)", got)
	}
}

// TestRunServiceHooksFailureStopsRemainingHooks answers the brief's question
// for item 4: what happens when a hook command fails? Per the loop at
// exec_compose.go:244-262 and docs/compose.md:123-124 ("A failed hook fails
// the `up`"), a hook that exits non-zero returns an error immediately and
// no later hook runs. This is the caller-unwind behavior for post_start;
// runServiceHooks itself has no notion of post_start vs. pre_stop, so the
// degrade-to-warning behavior docs/compose.md:126-129 describes for
// pre_stop lives in the caller, not here, and is not exercised by this test.
func TestRunServiceHooksFailureStopsRemainingHooks(t *testing.T) {
	s := seedRunning(t, "hook-fail")
	accepted := probeAgent(t, s.InstanceAgentSocket("hook-fail"), 0, 1)

	hooks := []types.ServiceHook{
		{Command: []string{"/bin/false"}},
		{Command: []string{"/bin/true"}},
	}
	err := runServiceHooks(s, "hook-fail", "web", "post_start", hooks)
	if err == nil {
		t.Fatal("runServiceHooks: want an error, the first hook exits non-zero")
	}
	if !strings.Contains(err.Error(), "post_start[0]") {
		t.Fatalf("error = %q, want it to name post_start[0]", err.Error())
	}
	if got := atomic.LoadInt32(accepted); got != 1 {
		t.Fatalf("accepted connections = %d, want exactly 1: the second hook must not run after the first failed", got)
	}
}
