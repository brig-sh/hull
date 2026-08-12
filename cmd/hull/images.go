// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"text/tabwriter"
	"time"

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

	// Create table output
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPOSITORY\tTAG\tDIGEST\tSIZE\tCREATED")

	for _, img := range images {
		// Parse repo and tag from reference
		repo := img.Ref
		tag := "latest"
		if repo != "" && repo[len(repo)-1] != ':' {
			// Try to extract tag
			for i := len(repo) - 1; i >= 0; i-- {
				if repo[i] == ':' {
					tag = repo[i+1:]
					repo = repo[:i]
					break
				}
			}
		}

		// Format size
		sizeStr := formatSize(img.Size)

		// Format digest
		digestStr := img.Digest
		if len(digestStr) > 19 {
			digestStr = digestStr[:19]
		}

		// Format age
		age := time.Since(img.PulledAt)
		var ageStr string
		if age < time.Minute {
			ageStr = "just now"
		} else if age < time.Hour {
			ageStr = fmt.Sprintf("%dm ago", int(age.Minutes()))
		} else if age < 24*time.Hour {
			ageStr = fmt.Sprintf("%dh ago", int(age.Hours()))
		} else {
			ageStr = fmt.Sprintf("%dd ago", int(age.Hours()/24))
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			repo,
			tag,
			digestStr,
			sizeStr,
			ageStr,
		)
	}

	return w.Flush()
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
