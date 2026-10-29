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

//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

func viewLogs(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}

	instanceID := args.First()
	follow := cmd.Bool("follow")
	tail := cmd.Int("tail")

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	// The log is the guest's own console output, replayed verbatim onto the
	// operator's terminal long after the guest wrote it -- an escape sequence
	// that solicits a reply works just as well from here as it does live, and
	// `hull logs` is often the first thing run on an instance that misbehaved.
	// Filter it exactly like an exec session; see guestTerminalWriter.
	return logsInstance(s, instanceID, guestTerminalWriter(os.Stdout), follow, tail)
}

// logsInstance is viewLogs without the CLI plumbing, so a record that has
// no console yet can be refused without opening a path.
func logsInstance(s *store.Store, instanceID string, out io.Writer, follow bool, tail int) error {
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return instanceReadError(instanceID, err)
	}
	// A creating record is written before the log path exists. Opening the
	// empty string reports "log file not found:" with a blank path, which
	// reads as a missing file rather than a pull still in progress.
	if state.Status == store.StatusCreating {
		return fmt.Errorf("instance %s is still being created; it has no console log yet", instanceID)
	}
	return readLogs(out, state.LogFile, follow, tail)
}
