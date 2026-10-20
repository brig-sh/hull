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
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/brig-sh/hull/internal/buildinfo"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

// build is what this binary reports about itself: the version the Go
// toolchain derived from the checkout, the commit, and whether the tree was
// modified. Nothing is stamped at link time; see internal/buildinfo.
var (
	build = buildinfo.Read()
	log   = logrus.WithField("subsystem", "hull")
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
	// Keep the image/instance store on the case-sensitive APFS volume mounted
	// at ~/.hull/store. Linux package trees contain names that differ only by
	// case (for example xt_CONNMARK.h and xt_connmark.h), which cannot coexist
	// on the default case-insensitive home volume. On Linux the same path is a
	// plain directory; see ensureStore in storemount_linux.go.
	storeDirDefault := filepath.Join(homeDir, ".hull", "store")

	app := &cli.Command{
		Name:    "hull",
		Usage:   appUsage,
		Version: build.String(),
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "enable debug logging",
			},
			&cli.StringFlag{
				Name:  "store-dir",
				Value: storeDirDefault,
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
		Commands: platformCommands(),
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fatal(err)
	}
	sendCommandEvent("ok", "")
}

// dirIsCaseSensitive answers the only question that matters about a store: can
// it hold xt_CONNMARK.h and xt_connmark.h at the same time.
//
// Asked by probing rather than by reading filesystem metadata, because what
// matters is the behaviour at this path -- a case-sensitive volume can be
// mounted anywhere, and diskutil would have to be parsed to find out.
func dirIsCaseSensitive(dir string) (bool, error) {
	f, err := os.CreateTemp(dir, ".hull-case-probe-*a")
	if err != nil {
		return false, err
	}
	lower := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(lower) }()

	upper := strings.TrimSuffix(lower, "a") + "A"
	if _, err := os.Stat(upper); err == nil {
		// The upper-case name resolved to the file we just made.
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

// globalStore returns the store instance from the CLI context.
//
// The case-sensitive volume is ensured here rather than in the root Before
// hook, because that ran for every command: `hull version`, `hull --help` and
// `hull compose config` would each create and mount a 200GB sparse image for
// any --store-dir they had not seen before. `compose config` only wants the
// store's *path*, to resolve volume mounts into it, and it made the CI smoke
// test fail -- the script points hull at a temp directory and deletes it
// afterwards, and rm cannot remove a mount point.
//
// Opening the store is the moment it has to be real, and this is the one place
// that happens.
func globalStore(cmd *cli.Command) (*store.Store, error) {
	storeDir := cmd.String("store-dir")
	if err := ensureStore(storeDir, defaultStoreDir()); err != nil {
		return nil, err
	}
	return store.New(storeDir)
}

// defaultStoreDir is where the store lives when --store-dir says nothing.
func defaultStoreDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hull", "store")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	sendCommandEvent("error", errorClass(err))
	os.Exit(1)
}
