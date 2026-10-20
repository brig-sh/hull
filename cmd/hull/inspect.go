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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

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

func inspectInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}

	instanceID := args.First()

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	state, err := s.GetInstance(instanceID)
	if err != nil {
		return instanceReadError(instanceID, err)
	}

	// An instance name can carry a C1 control; printJSON keeps it off the
	// terminal. vmState is hvi's own answer for a running hvi instance:
	// "status" says the VMM is alive, not that its guest runs.
	return printJSON(os.Stdout, inspectView(state))
}

// inspectedInstance is the record as inspect prints it, with the VM's live
// state alongside.
type inspectedInstance struct {
	*store.InstanceState
	VMState string `json:"vmState,omitempty"`
}

func inspectView(state *store.InstanceState) inspectedInstance {
	return inspectedInstance{InstanceState: state, VMState: hviVMState(state)}
}

// instanceReadError reports a GetInstance failure.
//
// Only ErrInstanceNotFound becomes "instance not found". Every other
// failure, including an unreadable state file, keeps its own text. Callers
// match that sentence when they decide a VM is gone, so a broken record
// must not wear it.
func instanceReadError(id string, err error) error {
	if errors.Is(err, store.ErrInstanceNotFound) {
		return fmt.Errorf("instance not found: %s", id)
	}
	return fmt.Errorf("instance %s: %w", id, err)
}
