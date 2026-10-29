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

	"github.com/urfave/cli/v3"
)

func logsCommand() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "view instance logs",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "follow log output",
			},
			&cli.IntFlag{
				Name:    "tail",
				Aliases: []string{"n"},
				Value:   -1,
				Usage:   "number of recent lines to show (-1 for all)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return viewLogs(ctx, cmd)
		},
	}
}
