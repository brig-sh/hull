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

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

// viewLogs prints an instance's console log from the path hull run gives
// it, and not the one the container's log-file label names: any client of
// the namespace can set a label, and logs would then print whatever file it
// pointed at.
func viewLogs(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()
	err := store.ValidateInstanceID(instanceID)
	if err != nil {
		return err
	}

	// The daemon only says whether the instance exists. A follow lasts
	// until hull rm deletes the log, and holds no connection meanwhile.
	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	_, err = c.LoadContainer(ctx, instanceID)
	_ = c.Close()
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("instance not found: %s", instanceID)
	case err != nil:
		return fmt.Errorf("instance %s: %w", instanceID, err)
	}

	logFile, err := instanceLogPath(containerd.ResolveNamespace(cmd.String("namespace")), instanceID)
	if err != nil {
		return err
	}
	// The log is the guest's console, and replayed onto a terminal it can
	// make the terminal type just as it could live. It goes through the
	// filter hull exec applies on macOS; see guestTerminalWriter.
	out := cmd.Writer
	f, ok := out.(*os.File)
	if ok {
		out = guestTerminalWriter(f)
	}
	return readLogs(out, logFile, cmd.Bool("follow"), cmd.Int("tail"))
}
