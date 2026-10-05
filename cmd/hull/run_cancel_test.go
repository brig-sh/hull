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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
)

// Run the real command in a separate process, with no test-installed signal
// handler: only production code can turn an OS signal into cleanup and an error.
func TestRunCancellationHelperProcess(t *testing.T) {
	if len(os.Args) < 5 || os.Args[2] != "--" || os.Args[3] != "hull-run-cancellation-helper" {
		return
	}
	cmd := &cli.Command{
		Name:     "hull-test",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir"}},
		Commands: []*cli.Command{runCommand()},
	}
	err := cmd.Run(context.Background(), append([]string{"hull-test"}, os.Args[4:]...))
	if err == nil {
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "hull-run-error: %v\n", err)
	fmt.Fprintf(os.Stderr, "hull-run-canceled: %t\n", errors.Is(err, context.Canceled))
	if os.Getenv("HULL_TEST_RUN_EXPECT_REAPED") == "1" {
		// The copy must reap its children before returning, not leave them for
		// init to reap after this process exits and accidentally hide the bug.
		var status syscall.WaitStatus
		pid, waitErr := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if !errors.Is(waitErr, syscall.ECHILD) {
			fmt.Fprintf(os.Stderr, "unreaped preparation child: pid=%d, err=%v\n", pid, waitErr)
			os.Exit(3)
		}
	}
	os.Exit(2)
}

// A PATH shim execs this helper in place of each tar process. Both announce
// readiness over an inherited pipe, then block without finishing preparation.
func TestRunCancellationTarHelperProcess(t *testing.T) {
	if len(os.Args) < 8 || os.Args[2] != "--" || os.Args[3] != "hull-cancellation-tar-helper" {
		return
	}
	args := os.Args[4:]
	if args[0] != "-C" || args[3] != "-" || (args[2] != "-cf" && args[2] != "-xf") {
		t.Fatalf("unexpected tar arguments: %q", args)
	}
	ready, control := os.NewFile(3, "tar-ready"), os.NewFile(4, "tar-control")
	defer func() { _ = ready.Close(); _ = control.Close() }()
	if _, err := fmt.Fprintf(ready, "%s %d %d\n", args[2], os.Getpid(), os.Getppid()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, control); err != nil {
		t.Fatal(err)
	}
}

func runCancellationStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	// Unix socket paths must stay short. This fixture never mounts a disk image.
	dir, err := os.MkdirTemp("/tmp", "hull-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ignored, err := storeIgnoresOwnership(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ignored {
		t.Skip("scratch filesystem ignores ownership; the store would need remounting")
	}
	if err := os.WriteFile(filepath.Join(dir, storeMarkerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	dockerConfig := t.TempDir()
	// Prevent both Docker credentials and the Podman fallback from being used.
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, dir, dockerConfig
}

type cancellationRun struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan struct{}
	err    error
}

func startCancellationRun(t *testing.T, storeDir, dockerConfig, image string, files []*os.File, extraEnv ...string) *cancellationRun {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := &cancellationRun{done: make(chan struct{})}
	run.cmd = exec.Command(executable, "-test.run=^TestRunCancellationHelperProcess$", "--",
		"hull-run-cancellation-helper", "--store-dir", storeDir, "run", "--name", "canceling",
		"--pull", "always", image)
	// Isolate any preparation children so assertion failures cannot leave them
	// behind. The test signals only the run PID for the actual regression.
	run.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A broken implementation may let tar children outlive run while still
	// holding its output pipe. Bound the I/O join after the owned leader exits.
	run.cmd.WaitDelay = 2 * time.Second
	run.cmd.ExtraFiles = files
	replacedEnv := map[string]bool{SleepChildEnv: true, SelfExecChildEnv: true, SelfExecLogEnv: true, "DOCKER_CONFIG": true}
	for _, entry := range extraEnv {
		key, _, _ := strings.Cut(entry, "=")
		replacedEnv[key] = true
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !replacedEnv[key] {
			run.cmd.Env = append(run.cmd.Env, entry)
		}
	}
	run.cmd.Env = append(run.cmd.Env, "DOCKER_CONFIG="+dockerConfig)
	run.cmd.Env = append(run.cmd.Env, extraEnv...)
	run.cmd.Stdout, run.cmd.Stderr = &run.output, &run.output
	if err := run.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		run.err = run.cmd.Wait()
		close(run.done)
	}()
	t.Cleanup(func() { run.stop(t) })
	return run
}

func (run *cancellationRun) stop(t *testing.T) {
	t.Helper()
	select {
	case <-run.done:
		return
	default:
		// Wait can still be copying output after the leader exits. Only signal
		// the isolated group while that owned leader still exists.
		if err := run.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Error("run helper did not exit during cleanup")
	}
}

func assertCreatingBeforeSignal(t *testing.T, s *store.Store) {
	t.Helper()
	state, err := s.GetInstance("canceling")
	if err != nil || state.Status != store.StatusCreating || state.PID != 0 || state.StartTime.IsZero() {
		t.Fatalf("instance before cancellation: %+v, err=%v", state, err)
	}
}

func assertRunCanceledAndRemoved(t *testing.T, run *cancellationRun, s *store.Store, signal syscall.Signal) {
	t.Helper()
	if err := run.cmd.Process.Signal(signal); err != nil {
		t.Fatalf("signal run: %v", err)
	}
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
	status, ok := run.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || status.Signaled() || status.ExitStatus() != 2 || run.err == nil ||
		!strings.Contains(run.output.String(), "hull-run-error:") || !strings.Contains(run.output.String(), "hull-run-canceled: true\n") {
		t.Errorf("signal did not become an ordinary run error: state=%v, err=%v, output=%q",
			run.cmd.ProcessState, run.err, run.output.String())
	}
	if _, err := os.Stat(s.InstanceDir("canceling")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cancellation left the instance directory: %v", err)
	}
	if state, err := s.GetInstance("canceling"); !errors.Is(err, store.ErrInstanceNotFound) {
		t.Errorf("cancellation left an instance record: %+v, err=%v", state, err)
	}
	if t.Failed() {
		return
	}
	if _, err := s.CreateInstance("canceling"); err != nil {
		t.Errorf("cancellation did not free the instance name: %v", err)
	}
}

func TestRunSignalsCancelBlockedPullAndRemoveCreating(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(signal.String(), func(t *testing.T) {
			s, dir, dockerConfig := runCancellationStore(t)
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enteredOnce, canceledOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-r.Context().Done():
					canceledOnce.Do(func() { close(canceled) })
				case <-release:
					http.Error(w, "test cleanup", http.StatusBadRequest)
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(unblock)
			run := startCancellationRun(t, dir, dockerConfig, strings.TrimPrefix(srv.URL, "http://")+"/test/image:latest", nil)
			select {
			case <-entered:
			case <-run.done:
				t.Fatalf("run ended before the registry request: %v, %q", run.err, run.output.String())
			case <-time.After(10 * time.Second):
				t.Fatal("run did not reach the registry")
			}
			assertCreatingBeforeSignal(t, s)
			assertRunCanceledAndRemoved(t, run, s, signal)
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Error("the blocked registry request was not canceled")
			}
		})
	}
}

func TestRunSignalsCancelLocalBundleCopyAndReapChildren(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(signal.String(), func(t *testing.T) {
			s, dir, dockerConfig := runCancellationStore(t)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "config.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			// exec preserves the PID and pipes; the shell never remains as a
			// separate child. Quote the executable path as shell code, not JSON.
			quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
			shim := "#!/bin/sh\nexec " + quoted + " -test.run=^TestRunCancellationTarHelperProcess$ -- hull-cancellation-tar-helper \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "tar"), []byte(shim), 0o700); err != nil {
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
			run := startCancellationRun(t, dir, dockerConfig, source, []*os.File{readyWrite, controlRead},
				"PATH="+bin+":"+os.Getenv("PATH"), "HULL_TEST_RUN_EXPECT_REAPED=1")
			_ = readyWrite.Close()
			_ = controlRead.Close()
			type tarChild struct {
				pid  int
				args []string
			}
			var children []tarChild
			isOurChild := func(child tarChild) bool {
				out, err := exec.Command("/bin/ps", "-ww", "-p", strconv.Itoa(child.pid), "-o", "command=").Output()
				return err == nil && strings.TrimSpace(string(out)) == strings.Join(child.args, " ")
			}
			t.Cleanup(func() {
				_ = controlWrite.Close()
				// If a broken run exited without stopping a tar child, verify its
				// unique argv before targeting it. Never signal an arbitrary PID.
				for _, child := range children {
					if isOurChild(child) {
						_ = syscall.Kill(child.pid, syscall.SIGKILL)
					}
				}
				run.stop(t)
			})
			if err := readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewScanner(readyRead)
			roles := make(map[string]bool)
			for len(children) < 2 {
				if !reader.Scan() {
					t.Fatalf("tar children did not become ready: %v", reader.Err())
				}
				fields := strings.Fields(reader.Text())
				if len(fields) != 3 {
					t.Fatalf("invalid tar readiness: %q", reader.Text())
				}
				pid, pidErr := strconv.Atoi(fields[1])
				parent, parentErr := strconv.Atoi(fields[2])
				role := fields[0]
				if pidErr != nil || parentErr != nil || pid <= 1 || pid == os.Getpid() ||
					parent != run.cmd.Process.Pid || (role != "-cf" && role != "-xf") || roles[role] {
					t.Fatalf("invalid tar readiness: %q", reader.Text())
				}
				args := []string{executable, "-test.run=^TestRunCancellationTarHelperProcess$", "--", "hull-cancellation-tar-helper", "-C"}
				if role == "-cf" {
					args = append(args, source, role, "-", ".")
				} else {
					args = append(args, s.InstanceBundleDir("canceling"), role, "-")
				}
				child := tarChild{pid: pid, args: args}
				children = append(children, child)
				if !isOurChild(child) {
					t.Fatalf("PID %d is not this test's tar helper", pid)
				}
				roles[role] = true
			}
			assertCreatingBeforeSignal(t, s)
			assertRunCanceledAndRemoved(t, run, s, signal)
			for _, child := range children {
				if err := syscall.Kill(child.pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Errorf("preparation child %d survived cancellation or was not reaped: %v", child.pid, err)
				}
			}
		})
	}
}
