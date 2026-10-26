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
	"io"
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
//
// On Linux there is no `hull run` yet, so there this command, run by hand or
// by brig, is the only thing that fetches them.
func assetsCommand() *cli.Command {
	return &cli.Command{
		Name:  "assets",
		Usage: "manage the boot assets used to run images that carry no kernel",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show where the boot assets are and whether they are present",
				Action: func(_ context.Context, cmd *cli.Command) error {
					return showBootAssets(cmd.Writer, assetsStoreDir(cmd))
				},
			},
			{
				Name:  "dir",
				Usage: assetsDirUsage,
				// Now that the assets live under the store, nothing outside
				// hull can work out where they are: it would have to know the
				// store dir, the default store location and the precedence of
				// two environment variables, and re-derive all three correctly
				// forever. brig needs exactly this path -- it checks whether
				// the assets are present and passes them to the runtime as
				// annotations -- so hull answers rather than making it guess.
				//
				// On Linux there is no store and the directory is brig's own,
				// but the reason stands: HULL_BOOT_ASSETS and BRIG_BOOT_ASSETS
				// still move it, and brig asks hull rather than resolving them
				// itself, as it does on macOS.
				//
				// One bare line, no decoration, so a shell can consume it:
				//	assets=$(hull assets dir)
				Action: func(_ context.Context, cmd *cli.Command) error {
					dir, err := bootassets.Dir(assetsStoreDir(cmd))
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(cmd.Writer, dir)
					return err
				},
			},
			{
				Name:      "pull",
				Usage:     "download the boot assets for this host",
				ArgsUsage: "[REF]",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "force",
						Usage: "download even when the assets are already present",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					ref := bootassets.Ref()
					if cmd.Args().Len() > 0 {
						ref = cmd.Args().First()
					}
					storeDir := assetsStoreDir(cmd)
					err := prepareAssetsStore(storeDir)
					if err != nil {
						return err
					}
					if !cmd.Bool("force") && bootassets.Present(storeDir) {
						dir, err := bootassets.Dir(storeDir)
						if err != nil {
							return err
						}
						_, err = fmt.Fprintf(cmd.Writer, "boot assets are already present in %s (use --force to replace them)\n", dir)
						return err
					}
					err = bootassets.Fetch(ctx, ref, storeDir)
					if err != nil {
						return err
					}
					return showBootAssets(cmd.Writer, storeDir)
				},
			},
		},
	}
}

func showBootAssets(stdout io.Writer, storeDir string) error {
	dir, err := bootassets.Dir(storeDir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "directory: %s\n", dir)
	_, _ = fmt.Fprintf(stdout, "reference: %s\n", bootassets.Ref())

	// Whether these bytes were signature-checked when they arrived is the one
	// thing about them nobody can otherwise find out, and it is the question
	// worth asking of a kernel. An empty verified digest is printed as plainly
	// as a full one: "we did not check" is information, and hiding it behind a
	// missing line is how it stops being noticed.
	switch record, err := bootassets.Record(storeDir); {
	case errors.Is(err, bootassets.ErrNoProvenance):
		_, _ = fmt.Fprintf(stdout, "provenance: none (fetched by a hull that predates the record)\n\n")
	case err != nil:
		_, _ = fmt.Fprintf(stdout, "provenance: unreadable: %v\n\n", err)
	case record.VerifiedDigest != "":
		_, _ = fmt.Fprintf(stdout, "fetched from: %s\nsignature: verified against %s\n\n", record.Ref, record.VerifiedDigest)
	default:
		_, _ = fmt.Fprintf(stdout, "fetched from: %s\nsignature: NOT checked when these were fetched "+
			"(no cosign on the host, an unsigned bundle, or %s=off)\n\n",
			record.Ref, bootassets.VerifyModeEnv)
	}

	// Writes to a tabwriter go to its buffer, so the only error that can
	// actually surface is Flush's -- which is returned. Ignoring them
	// explicitly says that, rather than leaving errcheck to guess.
	w := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
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
