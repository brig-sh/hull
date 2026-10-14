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
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/urfave/cli/v3"
)

// assetsCommand manages the kernel and initrd that boot an image which was
// never built to be a guest.
//
// `hull run` fetches these on demand, so this command is for the cases it
// cannot cover: seeding a machine ahead of time, pinning a version, or finding
// out what is actually on disk when a boot goes wrong.
func assetsCommand() *cli.Command {
	return &cli.Command{
		Name:  "assets",
		Usage: "manage the boot assets used to run images that carry no kernel",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show where the boot assets are and whether they are present",
				Flags: []cli.Flag{assetsProfileFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					profile, err := bootassets.NormalizeProfile(cmd.String("profile"))
					if err != nil {
						return err
					}
					if profile != "" {
						return showProfileAssets(cmd.String("store-dir"), profile)
					}
					return showBootAssets(cmd.String("store-dir"))
				},
			},
			{
				Name:  "dir",
				Usage: "print the boot asset directory for this store, and nothing else",
				// Now that the assets live under the store, nothing outside
				// hull can work out where they are: it would have to know the
				// store dir, the default store location and the precedence of
				// two environment variables, and re-derive all three correctly
				// forever. brig needs exactly this path -- it checks whether
				// the assets are present and passes them to the runtime as
				// annotations -- so hull answers rather than making it guess.
				//
				// One bare line, no decoration, so a shell can consume it:
				//	assets=$(hull assets dir)
				//
				// With --profile it is that profile's kernel directory
				// instead; the initrd a profile boots stays in the plain one.
				Flags: []cli.Flag{assetsProfileFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					dir, err := assetsDir(cmd.String("store-dir"), cmd.String("profile"))
					if err != nil {
						return err
					}
					fmt.Println(dir)
					return nil
				},
			},
			{
				Name:      "pull",
				Usage:     "download the boot assets for this host, or install a profile's kernel",
				ArgsUsage: "[REF]",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "force",
						Usage: "download even when the assets are already present",
					},
					assetsProfileFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					profile, err := bootassets.NormalizeProfile(cmd.String("profile"))
					if err != nil {
						return err
					}
					if profile != "" {
						return pullProfileAssets(ctx, cmd, profile)
					}
					ref := bootassets.Ref()
					if cmd.Args().Len() > 0 {
						ref = cmd.Args().First()
					}
					storeDir := cmd.String("store-dir")
					// The assets live under the store, so the store has to be
					// real before anything is written into it. Skipping this
					// leaves the files on the bare mountpoint, and the next
					// command that opens the store refuses to mount over them
					// -- a fresh install would pull its assets and then be
					// unable to run anything.
					//
					// `show` and `dir` do not do this: they only read a path,
					// and mounting a 200GB image to answer a question would be
					// the same overreach that made every command mount one.
					if err := ensureStore(storeDir, defaultStoreDir()); err != nil {
						return err
					}
					if !cmd.Bool("force") && bootassets.Present(storeDir) {
						dir, err := bootassets.Dir(storeDir)
						if err != nil {
							return err
						}
						fmt.Printf("boot assets are already present in %s (use --force to replace them)\n", dir)
						return nil
					}
					if err := bootassets.Fetch(ctx, ref, storeDir); err != nil {
						return err
					}
					return showBootAssets(storeDir)
				},
			},
		},
	}
}

// assetsProfileFlag selects a machine profile whose kernel is a variant of
// the bundle (see internal/bootassets/profile.go).
func assetsProfileFlag() cli.Flag {
	return &cli.StringFlag{
		Name: "profile",
		Usage: "the machine profile whose kernel to act on: " + bootassets.PortableArm64V1 +
			" (default: native, the bundle itself)",
	}
}

// assetsDir is the directory `hull assets dir` prints.
func assetsDir(storeDir, name string) (string, error) {
	profile, err := bootassets.NormalizeProfile(name)
	if err != nil {
		return "", err
	}
	if profile == "" {
		return bootassets.Dir(storeDir)
	}
	return bootassets.ProfileDir(storeDir, profile)
}

// pullProfileAssets installs a profile's kernel: from REF when one is given,
// otherwise from HULL_PORTABLE_KERNEL or HULL_PORTABLE_KERNEL_REF. The
// default bundle is fetched too when it is missing, since the profile boots
// its initrd.
func pullProfileAssets(ctx context.Context, cmd *cli.Command, profile string) error {
	storeDir := cmd.String("store-dir")
	if _, err := bootassets.ProfileDir(storeDir, profile); err != nil {
		return err
	}
	if err := ensureStore(storeDir, defaultStoreDir()); err != nil {
		return err
	}
	if !bootassets.Present(storeDir) {
		if err := bootassets.Fetch(ctx, bootassets.Ref(), storeDir); err != nil {
			return err
		}
	}
	if !cmd.Bool("force") && bootassets.ProfileKernelPresent(storeDir, profile) {
		dir, err := bootassets.ProfileDir(storeDir, profile)
		if err != nil {
			return err
		}
		fmt.Printf("the %s kernel is already present in %s (use --force to replace it)\n", profile, dir)
		return nil
	}
	var err error
	if cmd.Args().Len() > 0 {
		err = bootassets.FetchProfile(ctx, cmd.Args().First(), storeDir, profile)
	} else {
		err = bootassets.InstallProfile(ctx, storeDir, profile)
	}
	if err != nil {
		return err
	}
	return showProfileAssets(storeDir, profile)
}

// showProfileAssets is `hull assets show --profile`: the profile's kernel,
// where it came from, and the default initrd it boots with.
func showProfileAssets(storeDir, profile string) error {
	dir, err := bootassets.ProfileDir(storeDir, profile)
	if err != nil {
		return err
	}
	kernel, initrd, err := bootassets.ProfilePaths(storeDir, profile)
	if err != nil {
		return err
	}
	fmt.Printf("profile: %s\ndirectory: %s\n", profile, dir)
	switch record, err := bootassets.ProfileRecord(storeDir, profile); {
	case errors.Is(err, bootassets.ErrNoProvenance):
		fmt.Printf("provenance: none\n")
	case err != nil:
		fmt.Printf("provenance: unreadable: %v\n", err)
	case record.VerifiedDigest != "":
		fmt.Printf("installed from: %s (%s)\nsignature: verified against %s\n", record.Ref,
			record.Digest, record.VerifiedDigest)
	default:
		fmt.Printf("installed from: %s (%s)\nsignature: none (a local file, or nothing was checked)\n",
			record.Ref, record.Digest)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	row := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}
	row("FILE\tSIZE\tSTATUS\n")
	for _, path := range []string{kernel, initrd} {
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			row("%s\t-\tmissing\n", path)
		case info.Size() == 0:
			row("%s\t0\tempty\n", path)
		default:
			row("%s\t%d\tok\n", path, info.Size())
		}
	}
	return w.Flush()
}

func showBootAssets(storeDir string) error {
	dir, err := bootassets.Dir(storeDir)
	if err != nil {
		return err
	}
	fmt.Printf("directory: %s\n", dir)
	fmt.Printf("reference: %s\n", bootassets.Ref())

	// Whether these bytes were signature-checked when they arrived is the one
	// thing about them nobody can otherwise find out, and it is the question
	// worth asking of a kernel. An empty verified digest is printed as plainly
	// as a full one: "we did not check" is information, and hiding it behind a
	// missing line is how it stops being noticed.
	switch record, err := bootassets.Record(storeDir); {
	case errors.Is(err, bootassets.ErrNoProvenance):
		fmt.Printf("provenance: none (fetched by a hull that predates the record)\n\n")
	case err != nil:
		fmt.Printf("provenance: unreadable: %v\n\n", err)
	case record.VerifiedDigest != "":
		fmt.Printf("fetched from: %s\nsignature: verified against %s\n\n", record.Ref, record.VerifiedDigest)
	default:
		fmt.Printf("fetched from: %s\nsignature: NOT checked when these were fetched "+
			"(no cosign on the host, an unsigned bundle, or %s=off)\n\n",
			record.Ref, bootassets.VerifyModeEnv)
	}

	// Writes to a tabwriter go to its buffer, so the only error that can
	// actually surface is Flush's -- which is returned. Ignoring them
	// explicitly says that, rather than leaving errcheck to guess.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	row := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}
	row("FILE\tSIZE\tSTATUS\n")
	// Exactly what a boot reads, and nothing else. The exec agent is not listed
	// because it is not a file here: it travels inside container-initrd, which
	// is where the guest takes it from.
	for _, name := range []string{bootassets.KernelName(), bootassets.InitrdName} {
		path := filepath.Join(dir, name)
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			row("%s\t-\tmissing\n", name)
		case info.Size() == 0:
			row("%s\t0\tempty\n", name)
		default:
			row("%s\t%d\tok\n", name, info.Size())
		}
	}
	return w.Flush()
}
