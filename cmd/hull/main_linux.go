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
	"fmt"
	"os"

	"github.com/brig-sh/hull/internal/buildinfo"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

// On Linux, hull will be a containerd client: images, containers and tasks
// will live in containerd, not in a store directory as on macOS. A verb
// registered here before it works on Linux refuses with a reason.

var build = buildinfo.Read()

func main() {
	err := newApp().Run(context.Background(), os.Args)
	if err != nil {
		fatal(err)
	}
}

// newApp builds the command tree. It is separate from main so tests can run
// the commands as a user would, flags included.
func newApp() *cli.Command {
	return &cli.Command{
		Name:    "hull",
		Usage:   "run OCI images as VMs through containerd and urunc",
		Version: build.String(),
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "enable debug logging",
			},
		},
		Before: func(_ context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Bool("debug") {
				logrus.SetLevel(logrus.DebugLevel)
			}
			return nil, nil
		},
		Commands: []*cli.Command{
			imagesCommand(),
		},
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
