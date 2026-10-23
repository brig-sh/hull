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
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

func rmiCommand() *cli.Command {
	return &cli.Command{
		Name:        "rmi",
		Usage:       rmiUsage,
		ArgsUsage:   "<image> [<image>...]",
		Description: rmiDescription,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "force",
				Aliases: []string{"f"},
				Usage:   rmiForceUsage,
			},
			&cli.StringFlag{
				Name:  "platform",
				Usage: rmiPlatformUsage,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return removeImages(ctx, cmd)
		},
	}
}

// pickRemovals narrows the images ref matched to the ones rmi removes, or
// returns the error rmi reports for ref. keep narrows a match set to the
// --platform the caller was given, and is called only when one was.
func pickRemovals(ref, platform string, matches []*store.ImageMetadata, byPrefix bool,
	keep func([]*store.ImageMetadata) []*store.ImageMetadata) ([]*store.ImageMetadata, error) {
	found := len(matches)
	if platform != "" {
		matches = keep(matches)
	}
	if len(matches) == 0 {
		// Distinguish the two ways a reference comes back empty. "No such
		// image" for a tag the store does have, under another platform,
		// sends the reader looking for a typo that is not there.
		if found > 0 {
			return nil, fmt.Errorf(rmiNotForPlatform, ref, platform)
		}
		return nil, fmt.Errorf("no such image: %s", ref)
	}
	// A tag that names several images names all of them; a digest prefix
	// that does names one and cannot say which. Removing the set there
	// would delete images the user did not ask about, so it is refused
	// with the digests to choose from.
	digests := matchedDigests(matches)
	if byPrefix && len(digests) > 1 {
		return nil, fmt.Errorf("%s is ambiguous: it matches %s", ref, strings.Join(digests, ", "))
	}
	return matches, nil
}

// imageHolder is an instance that refers to an image, and whether it runs
// right now: on macOS whether its VMM is alive, on Linux whether its
// containerd task has not stopped.
type imageHolder struct {
	id      string
	running bool
}

func runningHolders(held []imageHolder) []string {
	var ids []string
	for _, h := range held {
		if h.running {
			ids = append(ids, h.id)
		}
	}
	return ids
}

// plural picks the wording for a count. These messages name a list of
// instances that is usually one long, and "instance held ... remove them" is
// the kind of seam that makes an error read as machine output.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func matchedDigests(images []*store.ImageMetadata) []string {
	digests := make([]string, 0, len(images))
	for _, img := range images {
		digests = append(digests, img.Digest)
	}
	sort.Strings(digests)
	// On Linux several names can point at one image, and a digest prefix
	// that matches them all still names that one image, not several.
	return slices.Compact(digests)
}

func holderIDs(held []imageHolder) []string {
	ids := make([]string, 0, len(held))
	for _, h := range held {
		ids = append(ids, h.id)
	}
	return ids
}
