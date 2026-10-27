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

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

func inspectCommand() *cli.Command {
	return &cli.Command{
		Name:  "inspect",
		Usage: "inspect instance details",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return inspectInstance(ctx, cmd)
		},
	}
}

// inspectInstance prints the record instanceRecord builds for one
// container, in the JSON form macOS prints. The fields containerd has no
// counterpart for, such as cmdLine and logFile, are empty.
func inspectInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()
	err := store.ValidateInstanceID(instanceID)
	if err != nil {
		return instanceReadError(instanceID, err)
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	inst, err := containerd.LoadInstance(ctx, c, instanceID)
	if errdefs.IsNotFound(err) {
		// instanceReadError reports only this error as "instance not
		// found", the words brig matches to decide an instance is gone.
		err = store.ErrInstanceNotFound
	}
	if err != nil {
		return instanceReadError(instanceID, err)
	}
	return printJSON(cmd.Writer, instanceRecord(inst))
}
