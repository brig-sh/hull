// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The one-shot pre-warm exists because a successful dial proves only that
// the host-side bridge is up: on Vz the unix socket accepts as soon as
// vz-runner serves it, while the guest agent binds its listener a beat
// later. These tests pin the two behaviors that make jobs survive that
// window: early EOFs are absorbed by the pre-warm, and a listener that
// never answers fails loudly without ever running the command.

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urunc-dev/urunc/pkg/agentproto"
)

// fakeAgent listens on the instance's agent socket. The first eofs
// connections are accepted and closed immediately — the bridge-up,
// guest-not-listening window. Later connections get a real session: the
// Open and CloseStdin frames are read and an Exit frame with code is sent.
func fakeAgent(t *testing.T, sockPath string, eofs int, code int) {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake agent listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		seen := 0
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			seen++
			if seen <= eofs {
				_ = conn.Close()
				continue
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				// Open, then CloseStdin.
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
}

// seedRunning creates a running instance whose agent socket lives under a
// short /tmp root, keeping the unix path inside darwin's sun_path budget.
func seedRunning(t *testing.T, id string) *store.Store {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "oneshot-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(id); err != nil {
		t.Fatal(err)
	}
	st := &store.InstanceState{
		ID:        id,
		Status:    "running",
		BundleDir: filepath.Join(root, "no-bundle"),
	}
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunOneShotSurvivesAgentStartupEOF(t *testing.T) {
	s := seedRunning(t, "job1")
	// Two early EOFs, then a working agent: the pre-warm must absorb the
	// window and the job must complete with the agent's exit code.
	fakeAgent(t, s.InstanceAgentSocket("job1"), 2, 0)

	res, err := runOneShot(s, "job1", "migrate", []string{"/bin/migrate"}, nil, 10*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if res.code != 0 {
		t.Fatalf("exit code = %d, want 0", res.code)
	}
	st, err := s.GetInstance("job1")
	if err != nil {
		t.Fatal(err)
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("recorded exit = %v, want 0", st.ExitCode)
	}
}

func TestRunOneShotUnreachableAgentFailsLoudly(t *testing.T) {
	s := seedRunning(t, "job2")
	// The listener never answers: every connection EOFs. The pre-warm must
	// exhaust its window and fail before the command is ever attempted.
	fakeAgent(t, s.InstanceAgentSocket("job2"), 1<<30, 0)

	_, err := runOneShot(s, "job2", "migrate", []string{"/bin/migrate"}, nil, 2*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("expected an error from an unreachable agent")
	}
	if !strings.Contains(err.Error(), "never became reachable") {
		t.Fatalf("error = %v, want the never-became-reachable wording", err)
	}
}
