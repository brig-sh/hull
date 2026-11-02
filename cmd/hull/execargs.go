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

func execCommand() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "run a command in a running instance",
		ArgsUsage: "<instance-id> <command> [args...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "tty",
				Aliases: []string{"t"},
				Usage:   "allocate a pseudo-terminal in the guest",
			},
			&cli.StringFlag{
				Name:  "cwd",
				Usage: "working directory inside the guest",
			},
			&cli.StringFlag{
				Name:    "user",
				Aliases: []string{"u"},
				Usage:   execUserUsage,
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "set an environment variable in the guest: KEY=VALUE, or a bare KEY to inherit it from the host environment and keep the value out of argv (repeatable)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return execInstance(ctx, cmd)
		},
	}
}
