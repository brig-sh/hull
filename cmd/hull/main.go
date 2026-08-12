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
	"path/filepath"
	"runtime/debug"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

var (
	version string
	log     = logrus.WithField("subsystem", "hull")
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			handlePanic(r, debug.Stack())
		}
	}()

	// Default store directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fatal(fmt.Errorf("failed to get home directory: %w", err))
	}
	defaultStoreDir := filepath.Join(homeDir, ".hull")

	app := &cli.Command{
		Name:    "hull",
		Usage:   "native macOS container CLI for unikernels",
		Version: version,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "enable debug logging",
			},
			&cli.StringFlag{
				Name:  "store-dir",
				Value: defaultStoreDir,
				Usage: "directory for storing images and instance state",
			},
			&cli.BoolFlag{
				Name:  "unattended",
				Usage: "skip the telemetry consent prompt (telemetry stays on; combine with --dnt to opt out)",
			},
			&cli.BoolFlag{
				Name:  "dnt",
				Usage: "record a telemetry opt-out (equivalent to DO_NOT_TRACK=1, persisted)",
			},
		},
		Before: func(_ context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Bool("debug") {
				logrus.SetLevel(logrus.DebugLevel)
			}
			initTelemetry(cmd)
			return nil, nil
		},
		Commands: []*cli.Command{
			pullCommand(),
			runCommand(),
			execCommand(),
			psCommand(),
			stopCommand(),
			checkpointCommand(),
			restoreCommand(),
			rmCommand(),
			logsCommand(),
			inspectCommand(),
			imagesCommand(),
			composeCommand(),
			networkGatewayCommand(),
			telemetryCommand(),
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fatal(err)
	}
	sendCommandEvent("ok", "")
}

// globalStore returns the store instance from the CLI context
func globalStore(cmd *cli.Command) (*store.Store, error) {
	storeDir := cmd.String("store-dir")
	return store.New(storeDir)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	sendCommandEvent("error", errorClass(err))
	os.Exit(1)
}
