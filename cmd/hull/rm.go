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
	"syscall"

	"github.com/urfave/cli/v3"
)

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

	instanceID := args.First()
	force := cmd.Bool("force")

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	// Check if running and force flag is set
	if state.Status == "running" && state.PID > 0 {
		if !force {
			return fmt.Errorf("instance is running, use --force to stop it first")
		}

		// Force stop by killing the process
		log.Debugf("Force stopping instance %s", instanceID)
		_ = syscall.Kill(state.PID, syscall.SIGKILL)
	}

	// Remove instance directory
	if err := s.DeleteInstance(instanceID); err != nil {
		return fmt.Errorf("failed to remove instance: %w", err)
	}

	fmt.Printf("Instance %s removed\n", instanceID)
	return nil
}
