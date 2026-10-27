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
	"time"

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

// forceKillGrace is how long `rm --force` waits for a SIGKILLed task to
// exit before giving up, as on macOS. The container's snapshot is the
// instance's root filesystem, and it is removed only once the VMM is gone.
const forceKillGrace = 5 * time.Second

func rmCommand() *cli.Command {
	return &cli.Command{
		Name:  "rm",
		Usage: "remove a stopped instance",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "force",
				Aliases: []string{"f"},
				Usage:   "force stop running instance before removal",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return removeInstance(ctx, cmd)
		},
	}
}

// removeInstance deletes an instance's container, with its task and its
// snapshot, and refuses a running one without --force, in the words macOS
// uses.
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

	err = containerd.RemoveInstance(ctx, c, instanceID, cmd.Bool("force"), forceKillGrace)
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("instance not found: %s", instanceID)
	case errors.Is(err, containerd.ErrInstanceRunning):
		return errors.New("instance is running, use --force to stop it first")
	case err != nil:
		return fmt.Errorf("failed to remove instance: %w", err)
	}

	_, err = fmt.Fprintf(cmd.Writer, "Instance %s removed\n", instanceID)
	return err
}
