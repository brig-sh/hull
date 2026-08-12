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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urunc-dev/urunc/pkg/qmp"
)

func stopCommand() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "stop a running instance",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "timeout,t",
				Value: 10,
				Usage: "timeout in seconds before force kill",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return stopInstance(ctx, cmd)
		},
	}
}

// markStopped records a deliberate stop: the instance ended, and it ended
// because someone asked. Every stop path goes through here so the
// StoppedByUser marker cannot be forgotten on one of them — the project
// supervisor refuses to restart a service carrying it, so a path that
// skipped it would have its stop undone within a poll.
// markIntent persists "a person asked for this" without touching Status, so
// the supervisor sees the marker for the whole duration of the stop.
func markIntent(s *store.Store, state *store.InstanceState) {
	if state.StoppedByUser {
		return
	}
	state.StoppedByUser = true
	_ = s.SaveInstance(state)
}

// clearIntent undoes markIntent when the stop did not finish, so a failed
// stop cannot leave a service permanently exempt from its restart policy.
func clearIntent(s *store.Store, instanceID string) {
	st, err := s.GetInstance(instanceID)
	if err != nil || !st.StoppedByUser || st.Status == "stopped" {
		return
	}
	st.StoppedByUser = false
	_ = s.SaveInstance(st)
}

func markStopped(s *store.Store, state *store.InstanceState, clearPID bool) {
	state.Status = "stopped"
	state.ExitedAt = time.Now()
	state.StoppedByUser = true
	if clearPID {
		state.PID = 0
	}
	_ = s.SaveInstance(state)
	// A detached VM (compose service, agent sandbox) reports its
	// lifetime here -- nothing was attached to observe the exit. Guarded
	// to fire once, so a foreground run that is also `stop`ped does not
	// double-report.
	sendEndOnce(s, state)
}

func stopInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	return stopInstanceIn(s, args.First(), int(cmd.Int("timeout")))
}

// stopInstanceIn is stopInstance without the CLI plumbing, so the signal
// paths can be exercised by a test. Every path that ends an instance here
// goes through markStopped: a path that skipped it would have its stop
// undone by the project supervisor within one poll.
func stopInstanceIn(s *store.Store, instanceID string, timeoutSecs int) error {
	timeout := int64(timeoutSecs)

	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	if state.Status == "stopped" {
		// Still record the intent. The natural operator sequence is: the
		// service crashed, 'ps' reaped the record, and 'stop' is run to keep
		// it down -- and without the marker the supervisor undoes that at the
		// next tick.
		if !state.StoppedByUser {
			state.StoppedByUser = true
			if state.ExitedAt.IsZero() {
				state.ExitedAt = time.Now()
			}
			_ = s.SaveInstance(state)
		}
		fmt.Printf("Instance %s is already stopped\n", instanceID)
		return nil
	}

	// Record the intent before the first signal, not after the VM is
	// confirmed dead: the gap between them is one poll interval wide, and the
	// supervisor restarts a service it sees dead without a marker. Status is
	// deliberately left alone until the process really ends -- claiming
	// "stopped" on a live VM would invite a duplicate boot.
	markIntent(s, state)
	// If this stop does not complete, the marker must not linger: it is the
	// only thing standing between a crashed service and its restart policy,
	// and nothing else in the codebase ever clears it.
	stopped := false
	defer func() {
		if !stopped {
			clearIntent(s, instanceID)
		}
	}()

	if state.PID <= 0 {
		markStopped(s, state, false)
		stopped = true
		fmt.Printf("Instance %s marked as stopped\n", instanceID)
		return nil
	}

	// --timeout is the TOTAL graceful budget (docker semantics): the QMP
	// stage and the SIGTERM stage split whatever remains, instead of each
	// getting the full value (which made the worst case ~2x the timeout).
	budgetDeadline := time.Now().Add(time.Duration(timeout) * time.Second)

	// Verify the recorded pid still belongs to our VMM before signaling —
	// a crashed VMM plus pid reuse would otherwise TERM an innocent process.
	if !vmmProcessMatches(state.PID) {
		markStopped(s, state, true)
		stopped = true
		fmt.Printf("Instance %s stopped (process already gone)\n", instanceID)
		return nil
	}

	// Try graceful shutdown via QMP first
	if state.QMPSocket != "" {
		log.Debugf("Attempting graceful shutdown via QMP")
		qmpClient, err := qmp.Dial(state.QMPSocket, 5*time.Second)
		if err == nil {
			defer func() { _ = qmpClient.Close() }()
			if err := qmpClient.SystemPowerdown(); err == nil {
				if waitForExit(state.PID, time.Until(budgetDeadline)/2) {
					markStopped(s, state, true)
					stopped = true
					fmt.Printf("Instance %s stopped\n", instanceID)
					return nil
				}
				// Timeout - fall through to SIGTERM/SIGKILL
				log.Warnf("QMP shutdown timeout for instance %s", instanceID)
			}
		}
	}

	// Graceful shutdown via SIGTERM: vz-runner traps it and calls
	// VZVirtualMachine.requestStop (ACPI-style), and QEMU also exits
	// cleanly. Previously the Vz path went straight to SIGKILL.
	log.Debugf("Sending SIGTERM to PID %d", state.PID)
	if err := syscall.Kill(state.PID, syscall.SIGTERM); err == nil {
		if waitForExit(state.PID, time.Until(budgetDeadline)) {
			markStopped(s, state, true)
			stopped = true
			fmt.Printf("Instance %s stopped\n", instanceID)
			return nil
		}
		log.Warnf("SIGTERM timeout for instance %s, using SIGKILL", instanceID)
	}

	// Fall back to SIGKILL
	log.Debugf("Sending SIGKILL to PID %d", state.PID)
	err = syscall.Kill(state.PID, syscall.SIGKILL)
	if err != nil && !os.IsNotExist(err) {
		// Check if process exists first
		if err := syscall.Kill(state.PID, 0); err != nil {
			markStopped(s, state, true)
			stopped = true
			return nil
		}
		return fmt.Errorf("failed to kill process: %w", err)
	}

	// Wait a bit for process to be reaped
	time.Sleep(100 * time.Millisecond)

	markStopped(s, state, true)
	stopped = true
	fmt.Printf("Instance %s stopped\n", instanceID)
	return nil
}

// waitForExit polls until the process is gone or the timeout elapses.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// vmmProcessMatches reports whether pid's argv looks like one of our VMMs,
// guarding stop against pid reuse after a VMM crash.
func vmmProcessMatches(pid int) bool {
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	argv := string(out)
	return strings.Contains(argv, "vz-runner") || strings.Contains(argv, "qemu-system")
}
