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
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
)

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "list pulled images",
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

	if len(images) == 0 {
		fmt.Println("No images found")
		return nil
	}

	return writeImageTable(os.Stdout, images, time.Now())
}

// writeImageTable prints one row per stored image.
//
// The repository and tag come from the store's own split of the reference the
// image was pulled under. Scanning the reference backwards for a colon, as
// this used to, landed on the one inside `@sha256:` for an image pulled by
// digest, and printed a repository ending in `@sha256` with the hex as its
// tag. Such an image has no tag, and says so, the way docker does.
func writeImageTable(out io.Writer, images []*store.ImageMetadata, now time.Time) error {
	w := tabwriter.NewWriter(out, 4, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPOSITORY\tTAG\tDIGEST\tSIZE\tCREATED")

	for _, img := range images {
		ref := store.ParseReference(img.Ref)
		tag := ref.Tag
		switch {
		case ref.Digest != "":
			tag = "<none>"
		case tag == "":
			tag = "latest"
		}

		digestStr := img.Digest
		if len(digestStr) > 19 {
			digestStr = digestStr[:19]
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			ref.Repository,
			tag,
			digestStr,
			formatSize(img.Size),
			formatAge(now.Sub(img.PulledAt)),
		)
	}

	return w.Flush()
}

func formatAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func formatSize(bytes int64) string {
	units := []string{"B", "KB", "MB", "GB"}
	size := float64(bytes)
	for _, unit := range units {
		if size < 1024.0 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024.0
	}
	return fmt.Sprintf("%.1f TB", size)
}
