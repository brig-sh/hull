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
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// forceKillGrace is how long `rm --force` waits for a SIGKILLed VMM to
// actually disappear. The wait exists so the instance directory is never
// deleted from under a process that still has the rootfs image, the log and
// the sockets open: a VM that keeps writing into a half-removed store is a
// worse outcome than a slow `rm`.
const forceKillGrace = 5 * time.Second

func rmCommand() *cli.Command {
	return &cli.Command{
		Name:  "rm",
		Usage: "remove a stopped instance",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "force,f",
				Usage: "force stop running instance before removal",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return removeInstance(ctx, cmd)
		},
	}
}

func removeInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	return removeInstanceIn(s, args.First(), cmd.Bool("force"))
}

// removeInstanceIn is removeInstance without the CLI plumbing, so the signal
// and recovery paths can be exercised by a test.
func removeInstanceIn(s *store.Store, instanceID string, force bool) error {
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return removeWedgedInstance(s, instanceID, err)
	}

	// Check if running and force flag is set. "starting" counts as running the
	// moment a pid is on the record, because by then there really is a VMM.
	if (state.Status == "running" || state.Status == store.StatusStarting) && state.PID > 0 {
		if !force {
			return fmt.Errorf("instance is running, use --force to stop it first")
		}
		if err := forceKillVMM(instanceID, state.PID); err != nil {
			return err
		}
	} else if state.Status == store.StatusStarting {
		// A record still at "starting" is a run that died between spawning its
		// VMM and recording the pid. There is no pid to check or to kill, so
		// this cannot be treated as running -- and it must not be refused
		// either, because refusing is what left the name squatted with no way
		// out. Remove it, and say plainly that a stray VMM is possible, in the
		// same terms removeWedgedInstance uses for the record that is missing
		// entirely.
		log.Warnf("instance %s never got past starting, so no pid was recorded; removing it. "+
			"If a VMM was started for it, check for a stray process with `ps ax | grep vz-runner`",
			instanceID)
	}

	// Remove instance directory
	if err := s.DeleteInstance(instanceID); err != nil {
		return fmt.Errorf("failed to remove instance: %w", err)
	}

	fmt.Printf("Instance %s removed\n", instanceID)
	return nil
}

// forceKillVMM ends the VMM recorded for an instance, but only once it has
// confirmed that the recorded pid is still that VMM.
//
// The pid in state.json is evidence of nothing on its own: the kernel recycles
// pid numbers, so after a host reboot or a crashed VMM that number names some
// unrelated process belonging to the same user -- an editor, a browser, the
// shell running hull. Signalling it unconditionally is what `rm --force` used
// to do, which made it a way to SIGKILL an arbitrary host process. `stop` and
// `checkpoint` both check the identity before signalling; this path simply
// never did.
func forceKillVMM(instanceID string, pid int) error {
	if !vmmProcessMatches(pid) {
		// Nothing of ours is there any more, so there is nothing to kill and
		// the record is stale: removing it is exactly what was asked for.
		log.Debugf("instance %s: pid %d is not one of our VMMs, not signalling it", instanceID, pid)
		return nil
	}

	log.Debugf("Force stopping instance %s (pid %d)", instanceID, pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to kill VMM for %s (pid %d): %w", instanceID, pid, err)
	}
	if !waitForExit(pid, forceKillGrace) {
		return fmt.Errorf("VMM for %s (pid %d) is still alive after SIGKILL; "+
			"refusing to delete its directory while it may still be writing to it",
			instanceID, pid)
	}
	return nil
}

// removeWedgedInstance clears an instance directory whose state.json cannot be
// read, and is the only way out of the orphan-instance wedge.
//
// A run that is killed between CreateInstance and the first SaveInstance --
// or while the state file is being written -- leaves a directory with no
// usable record. That instance is invisible to `ps`, unreachable by `stop`,
// and its name stays squatted forever, because CreateInstance still fails with
// ErrInstanceExists while `rm` used to answer "instance not found". Treat a
// directory that exists as something to remove, and reserve "not found" for a
// name that really has no directory.
func removeWedgedInstance(s *store.Store, instanceID string, readErr error) error {
	if _, statErr := os.Stat(s.InstanceDir(instanceID)); statErr != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	// There is no pid to check, so nothing can be said about a VMM that may
	// have been started for this name. Say so rather than implying the host is
	// now clean.
	log.Warnf("instance %s has no readable state (%v); removing the directory. "+
		"If a VMM was started for it, check for a stray process with `ps ax | grep vz-runner`",
		instanceID, readErr)

	if err := s.DeleteInstance(instanceID); err != nil {
		return fmt.Errorf("failed to remove instance: %w", err)
	}

	fmt.Printf("Instance %s removed (its state file was missing or unreadable)\n", instanceID)
	return nil
}
