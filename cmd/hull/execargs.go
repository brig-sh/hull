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
	"os"

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

// execArgs returns the instance an exec names and the command to run in it.
func execArgs(cmd *cli.Command) (instanceID string, argv []string, err error) {
	args := cmd.Args()
	if args.Len() < 2 {
		return "", nil, errors.New("usage: exec [-t] <instance-id> <command> [args...]")
	}
	return args.First(), args.Slice()[1:], nil
}

// execOptions is what an exec's command gets from the flags and from hull's
// environment and terminal, whatever carries it to the guest.
type execOptions struct {
	env        []string
	tty        bool
	rows, cols uint16
	// The descriptor the size was measured from, re-read on every SIGWINCH.
	// Whether the session is interactive is a question about stdin, but the
	// window size belongs to whichever descriptor is really a terminal: callers
	// that pipe or capture stdout while leaving stdin on the terminal are
	// ordinary, and measuring stdout there silently yields 24x80.
	sizeFd           int
	sizeFdIsTerminal bool
}

// newExecOptions resolves --env, and with -t sizes the guest's terminal from
// the first of stdout, stdin and stderr that is a terminal.
func newExecOptions(cmd *cli.Command, stdout, stdin, stderr int) (execOptions, error) {
	env, err := resolveEnvEntries(cmd.StringSlice("env"), os.LookupEnv)
	if err != nil {
		return execOptions{}, err
	}
	o := execOptions{env: env, tty: cmd.Bool("tty"), rows: fallbackRows, cols: fallbackCols, sizeFd: -1}
	if o.tty {
		o.rows, o.cols, o.sizeFd, o.sizeFdIsTerminal = windowSize(stdout, stdin, stderr)
		// A default, not an override: an explicit --env TERM must win.
		o.env = appendEnvDefault(o.env, "TERM", os.Getenv("TERM"))
	}
	return o, nil
}
