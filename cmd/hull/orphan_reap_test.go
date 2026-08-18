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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// Recording the instance before spawning the VMM closed the half of the window
// where the VM was invisible. It left the other half: a VMM nothing could kill.
//
// A run killed between Start() and the pid write leaves "starting" with pid 0
// and a live process. stop marked that record stopped, rm deleted the instance
// directory -- the live VM's rootfs, disk image and console log -- and the name
// went back into circulation while the VM was still up.
//
// The argv is recorded before the spawn precisely so it can be found again.

// stubVMM starts a long-lived process whose argv is unique to this test, and
// returns the argv as it would have been recorded.
func stubVMM(t *testing.T) (*exec.Cmd, []string, <-chan struct{}) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "vz-runner-stub")
	// argv[0] is "vz-runner" because the identity check now looks at the
	// program name rather than searching the whole command line -- the
	// substring form matched an ordinary `tail -f /tmp/vz-runner-notes.log` and
	// got it killed. A loop rather than a bare `sleep`, because sh
	// exec-optimises a single-command -c and replaces its own argv, which is
	// precisely the argv being matched on.
	args := []string{"vz-runner", "-c", "while :; do sleep 1; done # " + marker}
	cmd := exec.Command("/bin/sh", args[1:]...)
	cmd.Args = args
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub VMM: %v", err)
	}
	// The stub is this process's child, so kill(0) keeps answering "alive"
	// for the zombie until somebody waits for it. Wait in the background and
	// report exit through a channel instead.
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	// Give ps a moment to see it.
	time.Sleep(200 * time.Millisecond)
	return cmd, args, exited
}

// waitExit reports whether the stub exited within the timeout.
func waitExit(exited <-chan struct{}, d time.Duration) bool {
	select {
	case <-exited:
		return true
	case <-time.After(d):
		return false
	}
}

func stillRunning(exited <-chan struct{}) bool {
	select {
	case <-exited:
		return false
	default:
		return true
	}
}

// storeFor builds a store holding this record, so reapOrphanVMM can re-read it
// before signalling -- the check that stops it killing a VMM which finished
// starting while `ps` was running.
func storeFor(t *testing.T, state *store.InstanceState) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(state.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReapOrphanVMMKillsTheProcessNobodyRecorded(t *testing.T) {
	_, args, exited := stubVMM(t)
	state := &store.InstanceState{
		ID:      "orphan-1",
		Status:  store.StatusStarting,
		PID:     0,
		CmdLine: args,
	}

	if !stillRunning(exited) {
		t.Fatal("the stub VMM died before the test ran")
	}
	s := storeFor(t, state)
	if !reapOrphanVMM(s, state) {
		t.Fatal("reapOrphanVMM did not find a VMM whose argv it had recorded")
	}
	if !waitExit(exited, 5*time.Second) {
		t.Error("the orphaned VMM survived the reap")
	}
}

// The match has to be the whole recorded argv. Anything looser is a licence to
// kill an unrelated process on a busy machine, and this runs from `rm`, which
// the user did not ask to have anything killed by.
func TestReapOrphanVMMLeavesUnrelatedProcessesAlone(t *testing.T) {
	_, args, exited := stubVMM(t)

	for _, tc := range []struct {
		name    string
		cmdLine []string
	}{
		{"no recorded argv", nil},
		{"a prefix of the argv", args[:2]},
		{"a different instance's argv", append(append([]string{}, args[:2]...), args[2]+"-other")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &store.InstanceState{
				ID: "orphan-2", Status: store.StatusStarting, PID: 0, CmdLine: tc.cmdLine,
			}
			s := storeFor(t, state)
			if reapOrphanVMM(s, state) {
				t.Error("reaped a process whose argv was not the one recorded")
			}
			if !stillRunning(exited) {
				t.Fatal("an unrelated process was killed")
			}
		})
	}
}

// rm must not delete a live VM's rootfs and disk image out from under it.
func TestRemoveReapsBeforeDeletingTheDirectory(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, args, exited := stubVMM(t)

	if _, err := s.CreateInstance("orphan-3"); err != nil {
		t.Fatal(err)
	}
	state := &store.InstanceState{
		ID: "orphan-3", Status: store.StatusStarting, PID: 0, CmdLine: args,
	}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}

	if err := removeInstanceIn(s, "orphan-3", false); err != nil {
		t.Fatalf("rm refused a starting record, which is what squats the name: %v", err)
	}

	if !waitExit(exited, 5*time.Second) {
		t.Error("rm deleted the instance directory while its VMM was still running")
	}
	if _, err := os.Stat(filepath.Join(dir, "instances", "orphan-3")); !os.IsNotExist(err) {
		t.Errorf("the instance directory was not removed: %v", err)
	}
}

// The staged copy is per instance and its source is image content, so it has to
// be bounded. Before this it was a bare io.Copy: an image shipping a sparse
// 8 GiB "kernel" wrote 8 GiB of real disk on every run and every supervisor
// restart, where previously N instances shared one file.
func TestStagingRefusesAnOversizedBootFile(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(rootfs, "boot", "vmlinuz")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse, so the test costs no disk: the limit must be enforced on bytes
	// copied, not on what the file claims to occupy.
	if err := f.Truncate(maxBootFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	instanceDir := t.TempDir()
	staged, err := stageImageBootFile(rootfs, "/boot/vmlinuz", instanceDir, stagedKernelName)
	if err == nil {
		fi, _ := os.Stat(staged)
		t.Fatalf("an oversized boot file was staged to %s (%d bytes on disk)", staged, fi.Size())
	}
	if _, err := os.Lstat(filepath.Join(instanceDir, stagedKernelName)); !os.IsNotExist(err) {
		t.Errorf("a refused staging left a partial file behind: %v", err)
	}
}
