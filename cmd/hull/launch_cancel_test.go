// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// Announce readiness only after SIGTERM is handled, then exit successfully
// when the launcher requests shutdown. The control pipe also releases the
// helper if its parent's test fails before cancellation.
func TestLaunchCancelHelperProcess(t *testing.T) {
	if len(os.Args) != 4 || os.Args[2] != "--" || os.Args[3] != "hull-launch-cancel-helper" {
		return
	}
	ready := os.NewFile(3, "cancel-ready")
	control := os.NewFile(4, "cancel-control")
	defer func() { _ = ready.Close(); _ = control.Close() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	if _, err := fmt.Fprintln(ready, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, control); close(released) }()
	select {
	case <-signals:
		if _, err := fmt.Fprintln(ready, "SIGTERM"); err != nil {
			t.Fatal(err)
		}
	case <-released:
	}
}

func TestCanceledLaunchDoesNotSpawn(t *testing.T) {
	const id = "canceled-before-launch"
	s := storeWithInstance(t, id, &store.InstanceState{
		ID: id, Status: store.StatusCreating, StartTime: time.Now().Add(-time.Minute),
	})
	statePath := filepath.Join(s.InstanceDir(id), "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "spawned")
	state := &store.InstanceState{ID: id, LogFile: s.InstanceLogFile(id), BundleDir: s.InstanceBundleDir(id)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started, launchErr := launchVMM(ctx, &cli.Command{}, s, state,
		[]string{"/bin/sh", "-c", `printf 'spawned\n' > "$1"`, "hull-canceled-launch", marker},
		nil, hypervisors.VzVmm, true, "none", "")
	if started && state.PID > 1 && state.PID != os.Getpid() {
		// A regression can spawn this short-lived command. Reap it before
		// checking the marker, without signalling any recorded process.
		if child, err := os.FindProcess(state.PID); err == nil {
			_, _ = child.Wait()
		}
	}
	if started || !errors.Is(launchErr, context.Canceled) {
		t.Fatalf("canceled launch: started=%v, err=%v", started, launchErr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled launch executed the command: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("canceled launch changed the creating record: %v\nbefore: %s\nafter: %s", err, before, after)
	}
}

func TestCanceledDetachedLaunchStopsAndReapsChild(t *testing.T) {
	const id = "canceled-after-spawn"
	s := storeWithInstance(t, id, &store.InstanceState{ID: id, Status: store.StatusCreating})
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
	if err := readyRead.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var helperPID int
	args := []string{alias, "-test.run=^TestLaunchCancelHelperProcess$", "--", "hull-launch-cancel-helper"}
	helperIsOwned := func(pid int) bool {
		if pid <= 1 || pid == os.Getpid() {
			return false
		}
		out, err := exec.Command("/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "ppid=,command=").Output()
		line := strings.TrimSpace(string(out))
		separator := strings.IndexAny(line, " \t")
		return err == nil && separator >= 0 && line[:separator] == strconv.Itoa(os.Getpid()) &&
			strings.TrimSpace(line[separator:]) == strings.Join(args, " ")
	}
	t.Cleanup(func() {
		cancel()
		_ = controlWrite.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			if helperIsOwned(helperPID) {
				_ = syscall.Kill(helperPID, syscall.SIGKILL)
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("launcher did not return during cleanup")
				return
			}
		}
		if helperPID <= 1 || helperPID == os.Getpid() {
			return
		}
		// The launcher owns Wait on this path. Only repair a regression
		// after it has returned, so there is never a competing reaper.
		var status syscall.WaitStatus
		if pid, err := syscall.Wait4(helperPID, &status, syscall.WNOHANG, nil); err == nil && pid == 0 {
			_ = syscall.Kill(helperPID, syscall.SIGKILL)
			_, _ = syscall.Wait4(helperPID, &status, 0, nil)
		}
	})
	state := &store.InstanceState{
		ID: id, LogFile: s.InstanceLogFile(id), BundleDir: s.InstanceBundleDir(id),
		MAC: "cancellation-test", Backend: "vz",
	}
	var started bool
	command := &cli.Command{
		Name: "hull-launch-cancel-test",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "wait-ip"},
			&cli.IntFlag{Name: "stop-grace", Value: 1},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var err error
			started, err = launchVMM(ctx, cmd, s, state, args,
				[]*os.File{readyWrite, controlRead}, hypervisors.VzVmm, true, "nat", "")
			return err
		},
	}
	done := make(chan error, 1)
	go func() {
		defer close(finished)
		defer func() { _ = readyWrite.Close(); _ = controlRead.Close() }()
		done <- command.Run(ctx, []string{"hull-launch-cancel-test", "--wait-ip"})
	}()
	reader := bufio.NewScanner(readyRead)
	if !reader.Scan() {
		t.Fatalf("helper did not report readiness: %v", reader.Err())
	}
	pid, err := strconv.Atoi(reader.Text())
	if err != nil || pid <= 1 || pid == os.Getpid() {
		t.Fatalf("invalid helper PID %q: %v", reader.Text(), err)
	}
	// Validate the independently announced PID before making it a cleanup
	// target. The unique alias and exact argv identify only this helper.
	if !helperIsOwned(pid) {
		t.Fatalf("announced PID %d is not this test's helper", pid)
	}
	helperPID = pid
	cancel()
	select {
	case launchErr := <-done:
		if !started || !errors.Is(launchErr, context.Canceled) {
			t.Fatalf("canceled detached launch: started=%v, err=%v", started, launchErr)
		}
		var exitErr *exec.ExitError
		if errors.As(launchErr, &exitErr) {
			t.Fatalf("helper did not exit cleanly after cancellation: %v", launchErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled detached launch did not stop its child")
	}
	if !reader.Scan() || reader.Text() != "SIGTERM" {
		t.Fatalf("helper did not receive graceful shutdown: %q, %v", reader.Text(), reader.Err())
	}
	var status syscall.WaitStatus
	if pid, err := syscall.Wait4(helperPID, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("launcher did not reap its child: pid=%d, status=%v, err=%v", pid, status, err)
	}
	stopped, err := s.GetInstance(id)
	if err != nil || stopped.Status != "stopped" || stopped.PID != 0 || stopped.ExitedAt.IsZero() {
		t.Fatalf("canceled launch record: %+v, %v", stopped, err)
	}
	if _, err := os.Stat(s.InstanceDir(id)); err != nil {
		t.Fatalf("canceled post-spawn launch removed the instance directory: %v", err)
	}
}
