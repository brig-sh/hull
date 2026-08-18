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

package main

import (
	"os/exec"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// The reaper decides from a record read before it shells out to `ps`, and that
// `ps` costs ~50ms on a busy machine. If the instance finishes starting inside
// that window it is recorded "running" with a pid, and killing it then is
// killing a healthy VM.
func TestReapLeavesAVMMThatFinishedStartingAlone(t *testing.T) {
	cmd, args, exited := stubVMM(t)
	pid := cmd.Process.Pid

	// The record the reaper was called with: still starting, no pid.
	stale := &store.InstanceState{
		ID: "late", Status: store.StatusStarting, PID: 0, CmdLine: args,
	}
	s := storeFor(t, stale)

	// Meanwhile the run finished starting and recorded itself.
	if err := s.SaveInstance(&store.InstanceState{
		ID: "late", Status: "running", PID: pid, CmdLine: args, StartTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if reapOrphanVMM(s, stale) {
		t.Error("the reaper killed a VMM that had finished starting")
	}
	if !stillRunning(exited) {
		t.Error("the VMM was killed despite being recorded running")
	}
}

// And when the record really has not moved, it still reaps.
func TestReapStillKillsAGenuineOrphan(t *testing.T) {
	cmd, args, exited := stubVMM(t)
	_ = cmd
	state := &store.InstanceState{
		ID: "orphan", Status: store.StatusStarting, PID: 0, CmdLine: args,
	}
	s := storeFor(t, state)

	if !reapOrphanVMM(s, state) {
		t.Fatal("the reaper did not find a VMM whose argv it had recorded")
	}
	if !waitExit(exited, 5*time.Second) {
		t.Error("the orphan survived the reap")
	}
}

// The reaper must not signal something that is not one of our VMMs, even when
// the recorded argv matches it.
//
// The argv scan finds a process by its command line; between that scan and the
// kill, the pid can be recycled. The pre-kill identity check is what stops the
// signal going to whatever holds the number now, and without this test that
// check could be deleted with a green suite -- the other tests here are
// satisfied by the record re-read alone.
func TestReapDoesNotSignalANonVMM(t *testing.T) {
	// A process whose argv[0] is an innocent program. The record claims this
	// exact argv, so the scan will find it; only the identity check can stop
	// the kill.
	marker := t.TempDir()
	args := []string{"tail", "-c", "while :; do sleep 1; done # " + marker}
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done # "+marker)
	cmd.Args = args
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	time.Sleep(200 * time.Millisecond)

	state := &store.InstanceState{
		ID: "notours", Status: store.StatusStarting, PID: 0, CmdLine: args,
	}
	s := storeFor(t, state)

	if reapOrphanVMM(s, state) {
		t.Error("the reaper killed a process that is not one of our VMMs")
	}
	select {
	case <-exited:
		t.Error("the non-VMM process was killed")
	default:
	}
}
