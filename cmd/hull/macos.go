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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/urfave/cli/v3"
)

// macosBundleFiles are what a macOS guest directory must hold. This is Tart's
// layout, so a VM that `tart clone` or `tart create` made boots as it is:
// config.json carries the hardware model and machine identifier the system was
// installed under, nvram.bin the auxiliary storage that install wrote, and
// disk.img the system itself.
var macosBundleFiles = []string{"config.json", "disk.img", "nvram.bin"}

// macosRunOptions is what `hull macos run` passes on to vz-runner. A zero
// CPUs or MemoryMB leaves the bundle's own sizing in place.
type macosRunOptions struct {
	GUI      bool
	GUITitle string
	CPUs     int
	MemoryMB int
	NoNet    bool
}

// checkMacOSBundle reports the first file dir is missing, so a wrong path is
// named here rather than by a framework error once the runner is up.
func checkMacOSBundle(dir string) error {
	for _, name := range macosBundleFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s is not a macOS guest directory: %w", dir, err)
		}
	}
	return nil
}

// macosRunnerArgs builds the vz-runner arguments for a macOS guest.
func macosRunnerArgs(dir string, opts macosRunOptions) []string {
	args := []string{"--macos-bundle", dir}
	if opts.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(opts.CPUs))
	}
	if opts.MemoryMB > 0 {
		args = append(args, "--mem", strconv.Itoa(opts.MemoryMB))
	}
	if opts.NoNet {
		args = append(args, "--no-net")
	}
	if opts.GUI {
		args = append(args, "--gui")
		if opts.GUITitle != "" {
			args = append(args, "--gui-title", opts.GUITitle)
		}
	}
	return args
}

// vzRunnerPath finds vz-runner next to this executable, the same place the
// Vz backend looks for it.
func vzRunnerPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	runner := filepath.Join(filepath.Dir(exe), "vz-runner")
	if _, err := os.Stat(runner); err != nil {
		return "", fmt.Errorf("vz-runner not found next to hull: %w", err)
	}
	return runner, nil
}

// macosCommand is the experimental entry point for macOS guests.
//
// `hull run` starts from an OCI image and records an instance that ps, exec,
// stop and checkpoint use. A macOS guest is a disk with an identity, and that
// code does not handle it yet. This command therefore boots the directory in
// the foreground and records no instance. The runner replaces the hull
// process, so Ctrl-C reaches it and asks the guest to stop.
func macosCommand() *cli.Command {
	return &cli.Command{
		Name:  "macos",
		Usage: "run a macOS guest from a VM directory (experimental, Vz only)",
		Commands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "boot a macOS guest in the foreground",
				ArgsUsage: "<vm-dir>",
				Description: "Boots the macOS guest in <vm-dir>, a directory in Tart's layout\n" +
					"(config.json, disk.img, nvram.bin), for example ~/.tart/vms/<name>.\n" +
					"The guest is not recorded as an instance: hull ps, exec, stop and\n" +
					"checkpoint do not see it. Stop it with Ctrl-C or from inside the guest.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "gui", Usage: "show the guest display in a window"},
					&cli.StringFlag{Name: "gui-title", Usage: "window title in --gui mode (default: the directory name)"},
					&cli.IntFlag{Name: "cpus", Usage: "CPU count (default: the bundle's own)"},
					&cli.IntFlag{Name: "memory", Usage: "memory in MB (default: the bundle's own)"},
					&cli.StringFlag{Name: "net", Value: "nat", Usage: "network: nat, or none for no network device"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() != 1 {
						return errors.New("usage: hull macos run <vm-dir>")
					}
					dir, err := filepath.Abs(cmd.Args().First())
					if err != nil {
						return err
					}
					if err := checkMacOSBundle(dir); err != nil {
						return err
					}
					opts := macosRunOptions{
						GUI:      cmd.Bool("gui"),
						GUITitle: cmd.String("gui-title"),
						CPUs:     int(cmd.Int("cpus")),
						MemoryMB: int(cmd.Int("memory")),
					}
					switch cmd.String("net") {
					case "nat":
					case "none":
						opts.NoNet = true
					default:
						return fmt.Errorf("--net %s: a macOS guest takes nat or none", cmd.String("net"))
					}
					if opts.GUI && opts.GUITitle == "" {
						opts.GUITitle = filepath.Base(dir)
					}
					runner, err := vzRunnerPath()
					if err != nil {
						return err
					}
					argv := append([]string{runner}, macosRunnerArgs(dir, opts)...)
					return syscall.Exec(runner, argv, os.Environ())
				},
			},
		},
	}
}
