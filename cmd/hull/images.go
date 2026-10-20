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
	"time"

	"github.com/urfave/cli/v3"
)

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "list pulled images",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "json",
				Usage: "print the store's records as JSON, with full digests",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return listImages(ctx, cmd)
		},
	}
}

func listImages(ctx context.Context, cmd *cli.Command) error {
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	images, err := s.ListImages()
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		return writeImageJSON(os.Stdout, images)
	}

	if len(images) == 0 {
		fmt.Println("No images found")
		return nil
	}

	return writeImageTable(os.Stdout, images, time.Now())
}
