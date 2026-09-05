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
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
)

// storeCommand manages the volume the image and instance store lives on.
//
// hull mounts that volume when it opens a store and has never unmounted it,
// which is fine for the default store -- one per machine, wanted for as long
// as hull is installed -- and wrong for every other one. A CI runner that
// gives each job its own --store-dir accumulates a mounted sparse image per
// job, and a script that points hull at a temp directory cannot delete it
// afterwards: rm will not remove a mount point.
func storeCommand() *cli.Command {
	return &cli.Command{
		Name:  "store",
		Usage: "manage the volume the image and instance store lives on",
		Commands: []*cli.Command{
			{
				Name:  "detach",
				Usage: "unmount the store volume, leaving its backing image on disk",
				Description: "Detaching keeps the sparse image, so re-running any hull command " +
					"remounts the same store with everything still in it. Nothing is deleted.",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "force",
						Usage: "detach even if something still has files open on it",
					},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return detachStore(cmd.String("store-dir"), cmd.Bool("force"))
				},
			},
		},
	}
}

// detachStore unmounts a store hull mounted.
//
// Deliberately not routed through globalStore: that ensures the store, so
// asking to detach would mount it first. This looks at the directory instead.
//
// Doing nothing is a success. Detaching is what a cleanup step runs after a
// job that may or may not have opened a store, and such a step should not fail
// the build because there was nothing to do.
func detachStore(storeDir string, force bool) error {
	if storeDir == "" {
		return fmt.Errorf("no store directory to detach")
	}
	marker := filepath.Join(storeDir, storeMarkerName)
	if _, err := os.Stat(marker); err != nil {
		if os.IsNotExist(err) {
			// Either nothing is mounted here, or what is mounted is not ours
			// to unmount. Both mean: leave it alone.
			fmt.Printf("no hull store mounted at %s\n", storeDir)
			return nil
		}
		return fmt.Errorf("check store mount: %w", err)
	}
	args := []string{"detach", storeDir}
	if force {
		args = append(args, "-force")
	}
	if err := runHdiutil(args...); err != nil {
		return fmt.Errorf("detach the store at %s: %w (something may still have files open on "+
			"it -- stop any running instances, or pass --force)", storeDir, err)
	}
	fmt.Printf("detached %s\n", storeDir)
	return nil
}
