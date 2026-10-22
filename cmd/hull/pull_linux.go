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
	"errors"
	"fmt"
	"runtime"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/imageplatform"
	"github.com/urfave/cli/v3"
)

// defaultPlatform is the host's own, so a pull with no --platform fetches
// an image the host can boot natively. macOS defaults to linux/arm64, the
// only architecture it boots without Rosetta.
var defaultPlatform = "linux/" + runtime.GOARCH

func pullCommand() *cli.Command {
	return &cli.Command{
		Name:  "pull",
		Usage: "pull an OCI image",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "platform",
				Value: defaultPlatform,
				Usage: "image platform to pull, e.g. linux/arm64",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return pullImage(ctx, cmd)
		},
	}
}

func pullImage(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("image reference required")
	}
	imageRef := args.First()

	platform, err := imageplatform.Parse(cmd.String("platform"))
	if err != nil {
		return err
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	stdout := cmd.Writer
	_, err = fmt.Fprintf(stdout, "Pulling %s...\n", imageRef)
	if err != nil {
		return err
	}
	img, err := containerd.Pull(ctx, c, imageRef, platform, cmd.ErrWriter)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "Successfully pulled image\nDigest: %s\nSize: %d bytes\n", img.Digest, img.Size)
	if err != nil {
		return err
	}
	return writeLabels(stdout, img.Labels)
}
