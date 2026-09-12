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

// writeImageJSON prints the stored records as they are, one per image, so a
// reference that resolved to two platforms shows both.
//
// The table cuts the manifest digest short and never shows the index digest,
// and the index digest is the one a caller comparing against a registry
// needs: a multi-arch tag resolves to it, while the store is keyed by the
// per-platform manifest digest, a different object. Here every digest is
// whole. indexDigest and platform are left out of a record that has none
// rather than printed empty, so "single-arch" and "pulled before it was
// recorded" read differently from a record that says so.
//
// Labels come out of the image config, so this goes through printJSON, which
// keeps a C1 control in one from reaching the terminal.
func writeImageJSON(out io.Writer, images []*store.ImageMetadata) error {
	if images == nil {
		// A machine reading the listing gets a list either way.
		images = []*store.ImageMetadata{}
	}
	return printJSON(out, images)
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

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			ref.Repository,
			tag,
			shortDigest(img.Digest),
			formatSize(img.Size),
			formatAge(now.Sub(img.PulledAt)),
		)
	}

	return w.Flush()
}

// shortDigest is how a digest is named where the whole thing would be noise:
// the algorithm and the first twelve hex characters.
//
// It is also what `hull rmi` accepts as a prefix, and that is not a
// coincidence -- this listing is where a user gets the digest they then paste
// into a command.
func shortDigest(digest string) string {
	const shortLen = len("sha256:") + 12
	if len(digest) <= shortLen {
		return digest
	}
	return digest[:shortLen]
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
