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
	"fmt"
	"time"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/containerd/platforms"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "list pulled images",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "json",
				Usage: "print the image records as JSON, with full digests",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return listImages(ctx, cmd)
		},
	}
}

// listImages prints the images in hull's containerd namespace, in the
// same table and JSON form as on macOS.
func listImages(ctx context.Context, cmd *cli.Command) error {
	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	imgs, err := c.ImageService().List(ctx)
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	// An image whose content cannot be read is still listed, under its name
	// and the digest DescribeImage returns with the error, rather than
	// dropped from the listing.
	records := make([]*store.ImageMetadata, 0, len(imgs))
	for _, img := range imgs {
		described, err := containerd.DescribeImage(ctx, c.ContentStore(), img, platforms.Default())
		if err != nil {
			logrus.Warnf("image %s: %v", img.Name, err)
		}
		records = append(records, imageRecord(described))
	}

	if cmd.Bool("json") {
		return writeImageJSON(cmd.Writer, records)
	}
	if len(records) == 0 {
		_, err = fmt.Fprintln(cmd.Writer, "No images found")
		return err
	}
	return writeImageTable(cmd.Writer, records, time.Now())
}

// imageRecord turns a containerd image into the record the macOS store keeps,
// so both listings print the same fields with the same meaning. PulledAt is
// when the name was last pointed at its image, the nearest containerd has to
// a pull time.
func imageRecord(img containerd.Image) *store.ImageMetadata {
	return &store.ImageMetadata{
		Ref:         img.Name,
		Digest:      img.Digest,
		IndexDigest: img.IndexDigest,
		Platform:    img.Platform,
		Size:        img.Size,
		Labels:      img.Labels,
		PulledAt:    img.UpdatedAt,
	}
}
