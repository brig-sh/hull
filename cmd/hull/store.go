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
	"sort"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
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
			{
				Name:  "compact",
				Usage: "shrink the store's backing sparse image, returning free space to the host",
				Description: "A sparse image only ever grows. Deleting an image inside the " +
					"store frees space on the store volume and gives nothing back to the " +
					"host, so `hull rmi` and `hull prune` need this to finish the job.\n\n" +
					"Compaction needs the volume unmounted, so this detaches it first and " +
					"leaves it detached; the next hull command mounts it again with " +
					"everything still in it. It refuses while an instance is running, " +
					"because detaching the volume under a live VM takes its rootfs away.",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "force",
						Usage: "detach even if something still has files open on it",
					},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return compactStore(cmd.String("store-dir"), defaultStoreDir(), cmd.Bool("force"))
				},
			},
		},
	}
}

// compactStore returns a store's unused space to the host.
//
// Nothing in hull ran `hdiutil compact`, so the backing image grew with every
// pull and never shrank: deleting an image inside the volume, by any means,
// changed the host's free space by nothing at all. That is half of what makes
// reclaiming space impossible, and no amount of `rmi` fixes it.
//
// A store on a case-sensitive volume the user provided has no backing image to
// compact, so the command succeeds with nothing to do, as detach does. A
// cleanup step can run it unconditionally.
func compactStore(storeDir, defaultDir string, force bool) error {
	if storeDir == "" {
		return fmt.Errorf("no store directory to compact")
	}
	image := storeImagePath(storeDir, defaultDir)
	if _, err := os.Stat(image); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("no store image at %s; nothing to compact\n", image)
			return nil
		}
		return fmt.Errorf("check store image: %w", err)
	}

	before := fileDiskUsage(image)

	mounted, err := storeIsMounted(storeDir)
	if err != nil {
		return err
	}
	if mounted {
		// Read the instance records before detaching, which is the only
		// moment they can still be read.
		if err := refuseWhileInstancesRun(storeDir); err != nil {
			return err
		}
		if err := detachStore(storeDir, force); err != nil {
			return err
		}
	}

	// Nothing locks the store between the detach and the compact: the lock
	// lives inside the volume, which is the thing being unmounted. Any hull
	// command started in that gap mounts the store again, and the compact then
	// fails on a busy image. Re-checking narrows the window to the syscall and
	// gives the operator the reason instead of hdiutil's.
	if remounted, err := storeIsMounted(storeDir); err == nil && remounted {
		return fmt.Errorf("%s was mounted again before it could be compacted, so nothing was "+
			"compacted. Another hull command opened the store; re-run with none in flight",
			storeDir)
	}

	if err := runHdiutil("compact", image); err != nil {
		if mounted {
			fmt.Printf("the store is detached; the next hull command mounts it again\n")
		}
		return fmt.Errorf("compact %s: %w", image, err)
	}

	after := fileDiskUsage(image)
	fmt.Printf("compacted %s: %s -> %s\n", image, formatSize(before), formatSize(after))
	if mounted {
		fmt.Printf("the store is detached; the next hull command mounts it again\n")
	}
	return nil
}

// fileDiskUsage is what a file actually occupies, which for a sparse image is
// not what its length says. Blocks, not size: that is the number the host's
// free space moves by.
func fileDiskUsage(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Blocks * 512
}

// storeIsMounted reports whether a store hull mounted is attached at storeDir.
func storeIsMounted(storeDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(storeDir, storeMarkerName)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check store mount: %w", err)
	}
	return true, nil
}

// refuseWhileInstancesRun stops a compaction that would pull the volume out
// from under a live VM.
//
// The guest's rootfs, its console log and its sockets are all on that volume.
// `hdiutil detach` without --force would fail on the open files, but with it
// the VM keeps running against a filesystem that is no longer there, which is
// a worse outcome than a refusal that names what to stop.
func refuseWhileInstancesRun(storeDir string) error {
	s, err := store.New(storeDir)
	if err != nil {
		return err
	}
	instances, err := s.ListInstances()
	if err != nil {
		return err
	}
	return refuseRunningInstances(instances, instanceMayHaveVMM)
}

// refuseRunningInstances is the decision refuseWhileInstancesRun makes, with
// the liveness check passed in so a test can exercise both answers without a
// VMM.
//
// A record saying "running" is not enough on its own: a host that crashed
// leaves those behind, and one of them must not make compaction impossible
// forever. The check takes the record, not a bare pid, so it compares the
// process start time the way stop does and counts a "starting" record with no
// pid yet. See instanceMayHaveVMM.
func refuseRunningInstances(instances []*store.InstanceState, live func(*store.InstanceState) bool) error {
	var running []string
	for _, inst := range instances {
		if live(inst) {
			running = append(running, inst.ID)
		}
	}
	if len(running) == 0 {
		return nil
	}
	sort.Strings(running)
	return fmt.Errorf("cannot compact while %s %s %s running: compaction unmounts the store, "+
		"and %s root %s on it. Stop %s first",
		plural(len(running), "instance", "instances"), strings.Join(running, ", "),
		plural(len(running), "is", "are"), plural(len(running), "its", "their"),
		plural(len(running), "filesystem is", "filesystems are"),
		plural(len(running), "it", "them"))
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
