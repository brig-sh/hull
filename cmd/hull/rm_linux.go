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

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

// forceKillGrace is how long `rm --force` waits for a SIGKILLed task to
// exit before giving up, as on macOS, and how long `stop` waits after the
// SIGKILL it sends once its timeout has passed. The container's snapshot is
// the instance's root filesystem, and it is removed only once the VMM is
// gone.
const forceKillGrace = 5 * time.Second

// removeInstance deletes an instance's container, with its task and its
// snapshot, and its log, and refuses a running one without --force, in the
// words macOS uses.
func removeInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()
	err := store.ValidateInstanceID(instanceID)
	if err != nil {
		return err
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// The path hull run computes for this name, and not the one the
	// container's log-file label names: any client of the namespace can set
	// a label, and rm would then delete whatever file it pointed at.
	logFile, logErr := instanceLogPath(containerd.ResolveNamespace(cmd.String("namespace")), instanceID)
	if logErr == nil {
		var unlock func()
		unlock, logErr = lockLogDir(filepath.Dir(logFile))
		switch {
		case errors.Is(logErr, os.ErrNotExist):
			// No run in the namespace has written a log, so this instance
			// has none. A log that appears from here on is a new run's.
			logFile = ""
			logErr = nil
		case logErr == nil:
			defer unlock()
		}
	}

	// The detach the instance's hook never ran, if it did not, goes while
	// the container, whose spec names the hook, is still there: a detach
	// that fails, or a hull rm killed before it, keeps the instance, and
	// the next hull rm runs it again.
	var taskGone func(context.Context, client.Container) error
	if logErr == nil && logFile != "" {
		dir := netDirOf(logFile)
		taskGone = func(ctx context.Context, ctr client.Container) error {
			left, err := attachmentOf(ctx, ctr, dir)
			if err == nil && left != nil {
				err = detachLeft(ctx, left)
			}
			if err == nil {
				return nil
			}
			if !cmd.Bool("force") {
				return fmt.Errorf("could not detach it from its network, so it is kept: %w; "+
					"hull rm tries again, and hull rm --force removes it without the detach", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrWriter, "warning: could not detach instance %s from its network: %s\n", instanceID, err)
			return nil
		}
	}

	err = containerd.RemoveInstance(ctx, c, instanceID, cmd.Bool("force"), forceKillGrace, taskGone)
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("instance not found: %s", instanceID)
	case errors.Is(err, containerd.ErrInstanceRunning):
		return errors.New("instance is running, use --force to stop it first")
	case err != nil:
		return fmt.Errorf("failed to remove instance: %w", err)
	}
	if logErr == nil && logFile != "" {
		logErr = os.Remove(logFile)
	}
	if logErr != nil && !errors.Is(logErr, os.ErrNotExist) {
		return fmt.Errorf("instance %s removed, but not its log: %w", instanceID, logErr)
	}
	// The network files sit beside the log, under the same lock. Their path
	// comes from the log's, the one run computes, and not from a label.
	if logFile != "" {
		warnDetachFailure(cmd.ErrWriter, instanceID, netDirOf(logFile))
		err = containerd.RemoveNetworkDir(netDirOf(logFile))
		if err != nil {
			return fmt.Errorf("instance %s removed, but not its network files: %w", instanceID, err)
		}
	}

	_, err = fmt.Fprintf(cmd.Writer, "Instance %s removed\n", instanceID)
	return err
}
