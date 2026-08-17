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
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

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
	// Keep the image/instance store on the case-sensitive APFS volume mounted
	// at ~/.hull/store. Linux package trees contain names that differ only by
	// case (for example xt_CONNMARK.h and xt_connmark.h), which cannot coexist
	// on the default case-insensitive home volume.
	storeDirDefault := filepath.Join(homeDir, ".hull", "store")

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
			assetsCommand(),
			storeCommand(),
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

const defaultStoreImageSize = "200g"

// storeMarkerName marks a directory as a store hull mounted itself.
const storeMarkerName = ".case-sensitive-apfs"

// storeImagePath is where the backing sparse image for a store lives.
//
// The default store keeps the name it has always had, so an existing install
// finds its images after an upgrade. Any other store dir gets an image named
// after itself rather than after its parent: two stores under one parent --
// `_work/store-a` and `_work/store-b` -- would otherwise both resolve to
// `_work/hull-store.sparseimage`, and the second attach fails with "Resource
// busy" because the first already has it.
func storeImagePath(storeDir, defaultStoreDir string) string {
	if storeDir == defaultStoreDir {
		return filepath.Join(filepath.Dir(storeDir), "hull-store.sparseimage")
	}
	return storeDir + ".sparseimage"
}

// ensureStore guarantees that storeDir is case-sensitive, mounting a
// case-sensitive APFS sparse image there if it is not already.
//
// This runs for every store, not just the default one. It used to run only
// when --store-dir was left alone, which meant anyone passing an explicit path
// -- CI, most of all -- silently got a case-insensitive store on the boot
// volume: exactly the thing the default store exists to avoid. Nothing
// complained, because store.New only calls MkdirAll. The failure surfaced much
// later, as a guest that unpacks a Linux rootfs with names that differ only by
// case and then does not boot.
func ensureStore(storeDir, defaultStoreDir string) error {
	marker := filepath.Join(storeDir, storeMarkerName)
	if _, err := os.Stat(marker); err == nil {
		// Already ours and already mounted.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check store mount: %w", err)
	}
	if err := os.MkdirAll(storeDir, 0700); err != nil {
		return fmt.Errorf("create store mountpoint: %w", err)
	}

	// A store that is already case-sensitive needs nothing: the user pointed
	// --store-dir at a case-sensitive volume of their own, and nesting a 200G
	// image inside it would be waste and surprise.
	sensitive, err := dirIsCaseSensitive(storeDir)
	if err != nil {
		return fmt.Errorf("check whether %s is case-sensitive: %w", storeDir, err)
	}
	if sensitive {
		return nil
	}

	// Mounting hides whatever is already in the directory -- the files stay on
	// disk, invisible and unused, and the store looks empty. Someone with an
	// existing plain store dir would appear to lose every image and quietly
	// re-pull the lot. Refuse instead, and say what to do.
	empty, err := dirIsEmpty(storeDir)
	if err != nil {
		return fmt.Errorf("check whether %s is empty: %w", storeDir, err)
	}
	if !empty {
		return fmt.Errorf("%s is on a case-insensitive filesystem and is not empty, so hull "+
			"cannot mount a case-sensitive store over it without hiding what is there. "+
			"Move or remove its contents, or point --store-dir at an empty directory or a "+
			"case-sensitive volume", storeDir)
	}

	image := storeImagePath(storeDir, defaultStoreDir)
	if _, err := os.Stat(image); os.IsNotExist(err) {
		if err := runHdiutil("create", "-type", "SPARSE", "-fs", "Case-sensitive APFS", "-size", defaultStoreImageSize, "-volname", "hull-store", image); err != nil {
			return fmt.Errorf("create case-sensitive APFS store: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("check store image: %w", err)
	}
	if err := runHdiutil("attach", image, "-mountpoint", storeDir, "-nobrowse"); err != nil {
		return fmt.Errorf("mount case-sensitive APFS store at %s: %w", storeDir, err)
	}

	// Belt and braces: if the mount somehow produced a case-insensitive store
	// anyway, fail here with the reason rather than letting a guest boot on a
	// rootfs whose files have collided.
	if sensitive, err := dirIsCaseSensitive(storeDir); err != nil {
		return fmt.Errorf("verify the mounted store: %w", err)
	} else if !sensitive {
		return fmt.Errorf("%s is still case-insensitive after mounting %s", storeDir, image)
	}
	if err := os.WriteFile(marker, []byte("case-sensitive APFS\n"), 0600); err != nil {
		return fmt.Errorf("write store marker: %w", err)
	}
	return nil
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

// storeOwnFiles are files hull itself drops in the store directory before the
// store is opened, and which mounting over is therefore harmless: they are
// bookkeeping hull rewrites, not anything a user put there.
//
// telemetry is the reason this list exists. It initialises from the root
// command, so by the time a command opens the store its two files are already
// sitting in the directory -- and without this the emptiness check below would
// refuse to mount over hull's own footprints on a genuinely fresh store.
var storeOwnFiles = map[string]bool{
	"telemetry.json": true,
	"telemetry.lock": true,
	storeMarkerName:  true,
	".DS_Store":      true,
}

// dirIsEmpty reports whether mounting over dir would hide anything worth
// keeping. Hull's own bookkeeping does not count; anything else does.
func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !storeOwnFiles[e.Name()] {
			return false, nil
		}
	}
	return true, nil
}

func runHdiutil(args ...string) error {
	command := exec.Command("hdiutil", args...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("hdiutil %v: %w: %s", args, err, output)
	}
	return nil
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
