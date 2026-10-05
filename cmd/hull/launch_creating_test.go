// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// The helper reports its own PID independently of state.json, writes a console
// token, and remains alive until signalled or its inherited control pipe closes.
// Its executable alias has a real VMM basename so ps and stop exercise their
// identity checks. This tests process launch, not guest boot.
func TestLaunchCreatingHelperProcess(t *testing.T) {
	if len(os.Args) != 5 || os.Args[2] != "--" || os.Args[3] != "hull-launch-creating-helper" {
		return
	}
	ready := os.NewFile(3, "launch-ready")
	control := os.NewFile(4, "launch-control")
	defer func() { _ = ready.Close() }()
	defer func() { _ = control.Close() }()
	if _, err := fmt.Fprintln(ready, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, os.Args[4]); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(ready, os.Args[4]); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, control); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchCreatingPublishesRunningAndStops(t *testing.T) {
	const id = "launching"
	previous := &store.InstanceState{ID: id, Status: store.StatusCreating, StartTime: time.Now().Add(-time.Hour)}
	s := storeWithInstance(t, id, previous)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "vz-runner")
	if err := os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readyRead.Close(); _ = readyWrite.Close() })
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlRead.Close(); _ = controlWrite.Close() })
	var child *os.Process
	var childState *os.ProcessState
	var waitErr error
	exited := make(chan struct{})
	t.Cleanup(func() {
		_ = controlWrite.Close()
		if child == nil {
			return
		}
		select {
		case <-exited:
			return
		default:
			_ = child.Kill()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("launch helper did not exit during cleanup")
		}
	})

	const token = "hull-launch-creating-console"
	args := []string{alias, "-test.run=^TestLaunchCreatingHelperProcess$", "--", "hull-launch-creating-helper", token}
	reapHelper := func(pid int) bool {
		if pid <= 1 || pid == os.Getpid() {
			return false
		}
		// Only our own child with this test's unique executable path and full
		// argv may become a cleanup target, even if the recorded PID is wrong.
		out, err := exec.Command("/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "ppid=,command=").Output()
		line := strings.TrimSpace(string(out))
		separator := strings.IndexAny(line, " \t")
		if err != nil || separator < 0 || line[:separator] != strconv.Itoa(os.Getpid()) ||
			strings.TrimSpace(line[separator:]) != strings.Join(args, " ") {
			return false
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		child = process
		// Detached launch does not Wait. Reap exactly once so stop does not
		// mistake a terminated but unreaped child for a running process.
		go func() {
			childState, waitErr = process.Wait()
			close(exited)
		}()
		return true
	}
	state := &store.InstanceState{
		ID: id, ImageDigest: "sha256:launch-test", LogFile: s.InstanceLogFile(id),
		BundleDir: s.InstanceBundleDir(id), MAC: "52:54:00:12:34:56", IP: "10.87.0.2", Backend: "vz",
	}
	wantMetadata := *state
	before := time.Now()
	started, launchErr := launchVMM(&cli.Command{}, s, state, args,
		[]*os.File{readyWrite, controlRead}, hypervisors.VzVmm, true, "none", "")
	after := time.Now()
	// Establish safe cleanup before deadline/scanner operations can fail.
	// The readiness pipe below remains independent evidence of the actual PID.
	if started {
		_ = reapHelper(state.PID)
	}
	// Closing the parent's duplicate makes a pre-spawn failure produce EOF.
	_ = readyWrite.Close()
	_ = controlRead.Close()
	if err := readyRead.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewScanner(readyRead)
	if !reader.Scan() {
		t.Fatalf("helper did not report its PID: %v (started=%v, launch=%v)", reader.Err(), started, launchErr)
	}
	pid, err := strconv.Atoi(reader.Text())
	if err != nil || pid <= 1 || pid == os.Getpid() {
		t.Fatalf("invalid helper PID %q: %v", reader.Text(), err)
	}
	if child == nil && !reapHelper(pid) {
		t.Fatalf("announced PID %d is not this test's helper child", pid)
	}
	if child.Pid != pid {
		t.Fatalf("launch PID %d differs from independently announced PID %d", child.Pid, pid)
	}
	if !reader.Scan() || reader.Text() != token {
		t.Fatalf("helper did not become ready: %q, %v", reader.Text(), reader.Err())
	}
	if !started || launchErr != nil {
		t.Fatalf("launch: started=%v, err=%v", started, launchErr)
	}
	select {
	case <-exited:
		t.Fatal("helper exited before stop")
	default:
	}
	running, err := s.GetInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != "running" || running.PID != pid || !reflect.DeepEqual(running.CmdLine, args) {
		t.Fatalf("running record does not describe helper PID %d: %+v", pid, running)
	}
	if running.StartTime.IsZero() || running.StartTime.Before(before) || running.StartTime.After(after) {
		t.Fatalf("launch time outside [%s, %s]: %s", before, after, running.StartTime)
	}
	metadata := *running
	metadata.Status, metadata.PID, metadata.StartTime, metadata.CmdLine = "", 0, time.Time{}, nil
	if !reflect.DeepEqual(metadata, wantMetadata) {
		t.Fatalf("launch changed metadata: got %+v, want %+v", metadata, wantMetadata)
	}
	if !processIsAVMM(pid, running) {
		t.Fatal("the recorded PID is not the launched helper")
	}
	var output bytes.Buffer
	if err := logsInstance(s, id, &output, false, -1); err != nil || !strings.Contains(output.String(), token) {
		t.Fatalf("console token missing: output=%q, err=%v", output.String(), err)
	}
	if row := psRow(t, s, id); !strings.Contains(row, "running") {
		t.Fatalf("ps reaped the live helper: %q", row)
	}
	if running, err = s.GetInstance(id); err != nil || running.Status != "running" || running.PID != pid {
		t.Fatalf("ps changed the running record: %+v, %v", running, err)
	}
	if err := stopInstanceIn(s, id, 2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("stop returned while the helper remained alive")
	}
	if waitErr != nil || childState == nil || !childState.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatalf("helper was not terminated by stop: state=%v, err=%v", childState, waitErr)
	}
	stopped, err := s.GetInstance(id)
	if err != nil || stopped.Status != "stopped" || stopped.PID != 0 || !stopped.StoppedByUser || stopped.ExitedAt.IsZero() {
		t.Fatalf("stopped record: %+v, err=%v", stopped, err)
	}
	if _, err := os.Stat(s.InstanceDir(id)); err != nil {
		t.Fatalf("stop removed the instance directory: %v", err)
	}
	if _, err := s.CreateInstance(id); !errors.Is(err, store.ErrInstanceExists) {
		t.Fatalf("stop freed the instance name before rm: %v", err)
	}
	if err := removeInstanceIn(s, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(id); err != nil {
		t.Fatalf("rm did not free the instance name: %v", err)
	}
}

func TestFailedLaunchRestoresCreatingRecord(t *testing.T) {
	const id = "failed-launch"
	previous := &store.InstanceState{ID: id, Status: store.StatusCreating, StartTime: time.Now().Add(-time.Minute)}
	s := storeWithInstance(t, id, previous)
	statePath := filepath.Join(s.InstanceDir(id), "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state := &store.InstanceState{ID: id, ImageDigest: "sha256:failed-launch-test", LogFile: s.InstanceLogFile(id), BundleDir: s.InstanceBundleDir(id)}
	started, err := launchVMM(&cli.Command{}, s, state,
		[]string{filepath.Join(t.TempDir(), "no-such-vmm")}, nil, hypervisors.VzVmm, true, "none", "")
	if started || err == nil || !strings.Contains(err.Error(), "failed to start VMM:") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing executable: started=%v, err=%v", started, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("failed launch did not restore the creating record:\nbefore: %s\nafter: %s", before, after)
	}
	if info, err := os.Stat(s.InstanceDir(id)); err != nil || !info.IsDir() {
		t.Fatalf("failed launch removed the directory owned by runInstance: %v", err)
	}
}
