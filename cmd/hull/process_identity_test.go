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
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// "Is this one of our VMMs" must be a question about the PROGRAM, not about
// whether its command line happens to mention one.
//
// The substring form killed an innocent process on a developer machine: an
// ordinary `tail -f /tmp/vz-runner-notes.log` matched "vz-runner", and a stale
// record pointing at its recycled pid was enough for `hull stop` to SIGTERM and
// then SIGKILL it. Editors, greps, log tails -- and hull itself -- were all
// targets.
func TestProcessIdentityLooksAtTheProgramNotTheArguments(t *testing.T) {
	t.Run("a process merely mentioning a VMM is not one", func(t *testing.T) {
		// argv[0] is an innocent program; the ARGUMENTS mention a VMM, which
		// is exactly what the substring form matched on.
		for _, argv0 := range []string{"tail", "grep", "vim", "sh"} {
			pid, _ := startProcessAs(t, argv0, "/bin/sleep", "30")
			if processIsAVMM(pid, nil) {
				t.Errorf("%s was treated as one of our VMMs", argv0)
			}
		}
	})

	t.Run("a real VMM name is one", func(t *testing.T) {
		for _, name := range []string{"vz-runner", "qemu-system-aarch64", "qemu-system-x86_64", "hvi"} {
			pid, _ := startProcessAs(t, name, "/bin/sleep", "30")
			if !processIsAVMM(pid, nil) {
				t.Errorf("%s was not recognised as one of our VMMs", name)
			}
		}
	})

	t.Run("a full path is matched by its base name", func(t *testing.T) {
		pid, _ := startProcessAs(t, "/opt/homebrew/bin/vz-runner", "/bin/sleep", "30")
		if !processIsAVMM(pid, nil) {
			t.Error("a VMM invoked by absolute path was not recognised")
		}
	})
}

// Pid reuse: a process that started AFTER the instance record says the VMM did
// cannot be that VMM, whatever it is called.
//
// Nothing checked this before, so a recycled pid belonging to a coincidentally
// VMM-named process was signalled as though it were ours.
func TestProcessIdentityRefusesARecycledPid(t *testing.T) {
	pid, _ := startProcessAs(t, "vz-runner", "/bin/sleep", "30")

	// A record claiming its VMM started an hour ago cannot be describing a
	// process that started a moment ago.
	stale := &store.InstanceState{
		ID:        "stale",
		Status:    "running",
		PID:       pid,
		StartTime: time.Now().Add(-time.Hour),
	}
	if processIsAVMM(pid, stale) {
		t.Error("a process that started long after the record was accepted as its VMM")
	}

	// The same process against a record of the right age is ours.
	fresh := &store.InstanceState{
		ID:        "fresh",
		Status:    "running",
		PID:       pid,
		StartTime: time.Now(),
	}
	if !processIsAVMM(pid, fresh) {
		t.Error("a live VMM was rejected against a record of the right age")
	}
}

// The recorded argv is the strongest signal there is, and it should win even
// when the clock is unhelpful.
func TestProcessIdentityTrustsTheRecordedArgv(t *testing.T) {
	cmd, args, _ := stubVMM(t)
	pid := cmd.Process.Pid
	state := &store.InstanceState{
		ID:        "argv",
		Status:    "running",
		PID:       pid,
		CmdLine:   args,
		StartTime: time.Now().Add(-time.Hour),
	}
	if !processIsAVMM(pid, state) {
		t.Error("a process whose full argv matches the record was rejected")
	}
}
