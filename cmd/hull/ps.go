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
	"fmt"
	"os"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"
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

	instances, err := s.ListInstances()
	if err != nil {
		return err
	}

	if len(instances) == 0 {
		fmt.Println("No instances found")
		return nil
	}

	// Create table output
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tEXIT\tPID\tIP\tCREATED")

	for _, state := range instances {
		status := state.Status

		// Check if process is still alive
		if state.PID > 0 && state.Status == "running" {
			err := syscall.Kill(state.PID, 0)
			if err != nil {
				status = "stopped"
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

		// "-" is an honest rendering of "ended without a reportable
		// status"; a code only appears when one was actually observed.
		exitStr := "-"
		if state.ExitCode != nil {
			exitStr = strconv.Itoa(*state.ExitCode)
		}

		pidStr := ""
		if state.PID > 0 {
			pidStr = fmt.Sprintf("%d", state.PID)
		}

		ip := state.IP
		if ip == "" {
			ip = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			state.ID,
			status,
			exitStr,
			pidStr,
			ip,
			state.StartTime.Format("2006-01-02 15:04:05"),
		)
	}

	return w.Flush()
}
