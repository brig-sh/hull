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

func stopCommand() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "stop a running instance",
		Flags: []cli.Flag{
			stopTimeoutFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return stopInstance(ctx, cmd)
		},
	}
}

// stopInstance ends an instance's task and deletes it, in the words macOS
// uses. The container, its snapshot and its log stay until hull rm.
func stopInstance(ctx context.Context, cmd *cli.Command) error {
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

	timeout := time.Duration(cmd.Int("timeout")) * time.Second
	running, err := containerd.StopInstance(ctx, c, instanceID, timeout, forceKillGrace)
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("instance not found: %s", instanceID)
	case err != nil:
		return fmt.Errorf("failed to stop instance: %w", err)
	case !running:
		_, err = fmt.Fprintf(cmd.Writer, "Instance %s is already stopped\n", instanceID)
		return err
	}
	_, err = fmt.Fprintf(cmd.Writer, "Instance %s stopped\n", instanceID)
	return err
}
