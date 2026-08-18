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
	"syscall"

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
		// Already ours and already mounted -- but possibly mounted the way
		// hull used to mount it, with ownership ignored.
		return ensureStoreOwnership(storeDir, storeImagePath(storeDir, defaultStoreDir))
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
	// -owners on, because macOS mounts disk images with ownership ignored by
	// default (mount(8) prints it as "noowners"). On such a volume the kernel
	// does not enforce uid or mode at all, so every 0600 hull writes inside
	// the store -- state.json, urunit.conf, and the container-boot initrd that
	// carries the environment secrets handed to the guest -- is readable by
	// any other local account on the machine. The mode bits are the only
	// access control the store has; without this flag they are decoration.
	if err := runHdiutil(storeAttachArgs(image, storeDir)...); err != nil {
		return fmt.Errorf("mount case-sensitive APFS store at %s: %w", storeDir, err)
	}
	if err := ensureStoreOwnership(storeDir, image); err != nil {
		return err
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

// storeAttachArgs is the hdiutil command line that mounts a store image.
// Kept in one place so the ownership flag cannot be dropped from one call
// site and kept on another, and so a test can assert it is there.
func storeAttachArgs(image, storeDir string) []string {
	return []string{"attach", image, "-mountpoint", storeDir, "-nobrowse", "-owners", "on"}
}

// mntIgnoreOwnership is MNT_IGNORE_OWNERSHIP from <sys/mount.h>: the volume is
// mounted "noowners" and the kernel ignores the uid, gid and mode of every
// file on it. Not exported by the syscall package.
const mntIgnoreOwnership = 0x00200000

// storeIgnoresOwnership reports whether the volume backing dir was mounted
// with ownership disabled, i.e. whether the 0600 on the instance state, the
// urunit config and the container-boot initrd is actually being enforced.
func storeIgnoresOwnership(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return st.Flags&mntIgnoreOwnership != 0, nil
}

// ensureStoreOwnership repairs a store that an older hull mounted without
// ownership enforcement.
//
// Every store created before the -owners flag existed is still mounted
// "noowners" on every machine that has one, and the fix on the create path
// does nothing for them: the marker file short-circuits ensureStore. There is
// no unprivileged way to flip the flag on a live mount, so the repair is a
// detach and a re-attach. It is attempted rather than required, because the
// detach fails while another hull process (a running instance's `run`, a
// concurrent `pull`) has the volume busy -- and a store whose permissions are
// weaker than advertised is a reason to warn loudly, not a reason to refuse to
// run at all.
func ensureStoreOwnership(storeDir, image string) error {
	ignored, err := storeIgnoresOwnership(storeDir)
	if err != nil {
		return err
	}
	if !ignored {
		return nil
	}

	if _, err := os.Stat(image); err != nil {
		// Not a store hull mounted from an image it can name: nothing to
		// re-attach, so all that is left is to say what is wrong.
		log.Warnf("%s is mounted with ownership ignored (noowners), so the 0600 on the "+
			"instance state and the container-boot initrd inside it does not keep another "+
			"local account out. Remount that volume with owners enabled.", storeDir)
		return nil
	}

	log.Infof("%s was mounted without ownership enforcement; remounting it with -owners on", storeDir)
	if err := runHdiutil("detach", storeDir); err != nil {
		log.Warnf("could not remount %s with ownership enforced (%v). The store stays usable, "+
			"but the 0600 on the instance state and the container-boot initrd inside it does "+
			"not keep another local account out. Close other hull processes and re-run, or "+
			"remount by hand: hdiutil detach %s && hdiutil attach %s -mountpoint %s -nobrowse -owners on",
			storeDir, err, storeDir, image, storeDir)
		return nil
	}
	if err := runHdiutil(storeAttachArgs(image, storeDir)...); err != nil {
		return fmt.Errorf("re-mount case-sensitive APFS store at %s with ownership enforced: %w", storeDir, err)
	}
	if ignored, err := storeIgnoresOwnership(storeDir); err == nil && ignored {
		log.Warnf("%s still ignores ownership after remounting; the file modes inside the "+
			"store are not enforced against other local accounts", storeDir)
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
