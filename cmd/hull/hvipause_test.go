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

package main

import (
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// fakeHviInstance records a running hvi instance whose VMM is a sleep that
// reports itself as hvi, with stub as its control socket.
func fakeHviInstance(t *testing.T, s *store.Store, id, sock string) (*store.InstanceState, <-chan struct{}) {
	t.Helper()
	pid, done := startProcessAs(t, "hvi", "/bin/sleep", "30")
	dir, err := s.CreateInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	root, err := hviCheckpointRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := &store.InstanceState{
		ID: id, Status: "running", PID: pid, Backend: "hvi", ControlSock: sock,
		CmdLine: []string{"/x/hvi", "boot", "--kernel", "/k", "--disk", "/r.img", "--checkpoint-root", root},
	}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	return state, done
}

// stop on an hvi instance goes through the control socket before any signal.
func TestStopAsksHviOverTheControlSocket(t *testing.T) {
	var pid atomic.Int64
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		if req["op"] == "stop" {
			_ = syscall.Kill(int(pid.Load()), syscall.SIGKILL)
		}
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"stopping"}`}
	})
	s := newCmdTestStore(t)
	state, done := fakeHviInstance(t, s, "vm1", stub.path)
	pid.Store(int64(state.PID))

	start := time.Now()
	if err := stopInstanceIn(s, "vm1", 10); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the VMM is still running")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stop took %s; the socket stop should not wait out the signal budget", took)
	}
	reqs := stub.seen()
	if len(reqs) != 1 || reqs[0]["op"] != "stop" || reqs[0]["v"] != float64(1) {
		t.Errorf("requests = %v, want one stop", reqs)
	}
	if got := mustInstance(t, s, "vm1"); got.Status != "stopped" || got.PID != 0 {
		t.Errorf("record = %s pid %d", got.Status, got.PID)
	}
}

// A socket that is not there (an hvi from before the socket, or one that
// already went away) must not stop the stop: the signals still follow.
func TestStopFallsBackToSignalsWithoutASocket(t *testing.T) {
	s := newCmdTestStore(t)
	_, done := fakeHviInstance(t, s, "vm1", filepath.Join(shortSocketDir(t), "gone.sock"))
	if err := stopInstanceIn(s, "vm1", 4); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the VMM is still running")
	}
}

func newCmdTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustInstance(t *testing.T, s *store.Store, id string) *store.InstanceState {
	t.Helper()
	st, err := s.GetInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
