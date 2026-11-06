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

	"github.com/urfave/cli/v3"
)

func psCommand() *cli.Command {
	return &cli.Command{
		Name:  "ps",
		Usage: "list instances, stopped ones included",
		Flags: []cli.Flag{
			// docker and nerdctl list a stopped container only with -a,
			// and callers written for them, brig among them, pass it.
			// hull ps lists every instance whatever it is given, since a
			// stopped instance still holds its name, so the flag changes
			// nothing.
			&cli.BoolFlag{
				Name:    "all",
				Aliases: []string{"a"},
				Usage:   "accepted for compatibility: hull ps always lists stopped instances too",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return listInstances(ctx, cmd)
		},
	}
}
