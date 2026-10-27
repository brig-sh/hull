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
	"io"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

func psCommand() *cli.Command {
	return &cli.Command{
		Name:  "ps",
		Usage: "list running instances",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return listInstances(ctx, cmd)
		},
	}
}

func listInstances(ctx context.Context, cmd *cli.Command) error {
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	return renderInstances(s, os.Stdout)
}

// renderInstances writes the instance table and reconciles the records it
// prints. Split out from the CLI action so the reconciliation can be tested
// without a running VM.
func renderInstances(s *store.Store, out io.Writer) error {
	instances, err := s.ListInstances()
	if err != nil {
		return err
	}

	for _, state := range instances {
		// A StatusStarting record is deliberately left alone by the
		// reconciliation below and shown as it is. There is no pid to probe,
		// so there is nothing to reconcile against, and guessing "stopped"
		// from age alone would race a run that is legitimately mid-spawn. It
		// shows as "starting" with an empty PID until someone stops or removes
		// it, which is the honest reading: hull knows a VMM was meant to
		// exist and does not know whether it does.
		//
		// Check whether the recorded VMM is still there. Asking `kill(pid, 0)`
		// alone answers a different question -- "does any process hold this
		// number" -- and pid numbers are recycled: after a host reboot a dead
		// sandbox whose pid was handed to some unrelated process reports
		// `running` forever, is never reaped here, and brig's Running() reads
		// exactly this table. `stop` already checks the argv before it acts on
		// a pid; the same check belongs on the read path.
		if state.PID > 0 && state.Status == "running" && !processIsAVMM(state.PID, state) {
			// Update state in store. The exit status stays nil: the VM
			// ending on its own carries no observable code.
			state.Status = "stopped"
			state.PID = 0
			if state.ExitedAt.IsZero() {
				state.ExitedAt = time.Now()
			}
			_ = s.SaveInstance(state)
		}
	}

	return writeInstanceTable(out, instances)
}
