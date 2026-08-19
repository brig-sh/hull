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
	"errors"
	"fmt"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/urfave/cli/v3"
)

func pullCommand() *cli.Command {
	return &cli.Command{
		Name:  "pull",
		Usage: "pull an OCI image",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "platform",
				Value: ociclient.DefaultPlatform,
				Usage: "image platform to pull, e.g. linux/amd64 for the Rosetta path",
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

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	client := ociclient.New(s)

	fmt.Printf("Pulling %s...\n", imageRef)
	result, err := client.PullPlatform(ctx, imageRef, cmd.String("platform"))
	if err != nil {
		return err
	}

	fmt.Printf("Successfully pulled image\n")
	fmt.Printf("Digest: %s\n", result.Digest)
	fmt.Printf("Size: %d bytes\n", result.Size)

	if len(result.Labels) > 0 {
		fmt.Println("Labels:")
		for k, v := range result.Labels {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}

	return nil
}
