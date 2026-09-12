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
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

func rmiCommand() *cli.Command {
	return &cli.Command{
		Name:      "rmi",
		Usage:     "remove one or more images from the store",
		ArgsUsage: "<image> [<image>...]",
		Description: "An image is named by the reference it was pulled under, by its store " +
			"digest, or by the first characters of that digest as `hull images` prints " +
			"them.\n\n" +
			"A reference can name several stored images: pulling a republished tag adds a " +
			"digest without retiring the old one, and each platform of a tag is its own " +
			"entry. All of them are removed. Use --platform to remove one platform's " +
			"entries only.\n\n" +
			"An image a running instance boots from is never removed. One a stopped " +
			"instance refers to is removed only with --force. On the vz generic-container " +
			"path that instance symlinks its bundle into the cache, so it loses its root " +
			"filesystem; the other rootfs modes hold their own copy.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "force",
				Aliases: []string{"f"},
				Usage:   "remove an image a stopped instance still refers to",
			},
			&cli.StringFlag{
				Name:  "platform",
				Usage: "only remove entries pulled for this platform, e.g. linux/amd64",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			return removeImages(cmd)
		},
	}
}

func removeImages(cmd *cli.Command) error {
	refs := cmd.Args().Slice()
	if len(refs) == 0 {
		return errors.New("image reference required")
	}

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	return removeImagesIn(os.Stdout, s, refs, cmd.String("platform"), cmd.Bool("force"))
}

// removeImagesIn is removeImages without the CLI plumbing, so the resolution
// and refusal paths can be exercised by a test.
//
// Every reference is attempted, and the failures are reported together at the
// end. `hull rmi a b` with an unknown `a` should still remove `b`: the
// alternative is an operator discovering the order their arguments happened to
// be in.
func removeImagesIn(w io.Writer, s *store.Store, refs []string, platform string, force bool) error {
	holders, err := instancesByImage(s)
	if err != nil {
		return err
	}

	var failures []error
	for _, ref := range refs {
		matches, byPrefix, err := s.FindImages(ref)
		if err != nil {
			return err
		}
		found := len(matches)
		if platform != "" {
			matches = keepPlatform(matches, platform)
		}
		if len(matches) == 0 {
			// Distinguish the two ways a reference comes back empty. "No such
			// image" for a tag the store does have, under another platform,
			// sends the reader looking for a typo that is not there.
			if found > 0 {
				failures = append(failures, fmt.Errorf(
					"%s is in the store, but not for platform %s", ref, platform))
			} else {
				failures = append(failures, fmt.Errorf("no such image: %s", ref))
			}
			continue
		}
		// A tag that names several images names all of them; a digest prefix
		// that does names one and cannot say which. Removing the set there
		// would delete images the user did not ask about, so it is refused
		// with the digests to choose from.
		if byPrefix && len(matches) > 1 {
			failures = append(failures, fmt.Errorf("%s is ambiguous: it matches %s",
				ref, strings.Join(matchedDigests(matches), ", ")))
			continue
		}
		for _, img := range matches {
			if err := removeOneImage(w, s, img, holders[img.Digest], force); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func removeOneImage(w io.Writer, s *store.Store, img *store.ImageMetadata, held []imageHolder, force bool) error {
	if running := runningHolders(held); len(running) > 0 {
		return fmt.Errorf("image %s is in use by running %s %s; stop %s first",
			img.Digest, plural(len(running), "instance", "instances"),
			strings.Join(running, ", "), plural(len(running), "it", "them"))
	}
	if len(held) > 0 && !force {
		ids := holderIDs(held)
		return fmt.Errorf("image %s is referenced by %s %s; remove %s with `hull rm`, or pass "+
			"--force to remove the image anyway. A vz generic-container instance symlinks its "+
			"bundle into the cache and loses its root filesystem",
			img.Digest, plural(len(ids), "instance", "instances"), strings.Join(ids, ", "),
			plural(len(ids), "it", "them"))
	}

	if err := s.DeleteImage(img.Digest); err != nil {
		return err
	}
	// The same line prune prints: the two commands remove the same thing.
	// Docker says "Deleted:" here; nothing reads hull's output for that yet.
	_, _ = fmt.Fprintf(w, "Removed %s %s\n", img.Digest, img.Ref)
	return nil
}

// keepPlatform narrows a match set to one platform.
//
// A record from before the platform was recorded was necessarily a default
// pull, which is how the cache lookup in run.go reads it too. Reading it any
// other way would make `rmi --platform` and `run --platform` disagree about
// which entry is which.
func keepPlatform(images []*store.ImageMetadata, platform string) []*store.ImageMetadata {
	var kept []*store.ImageMetadata
	for _, img := range images {
		entry := img.Platform
		if entry == "" {
			entry = ociclient.DefaultPlatform
		}
		if entry == platform {
			kept = append(kept, img)
		}
	}
	return kept
}

// imageHolder is an instance that refers to an image, and whether its VMM is
// alive right now.
type imageHolder struct {
	id      string
	running bool
}

// instancesByImage maps each image digest to the instances that refer to it.
//
// Every instance holds a reference, not just a running one. On the vz generic
// container path the bundle is a bare symlink into the image cache, so
// removing the image takes the root filesystem of an instance the operator
// still has on their `hull ps` listing.
func instancesByImage(s *store.Store) (map[string][]imageHolder, error) {
	instances, err := s.ListInstances()
	if err != nil {
		return nil, err
	}
	held := make(map[string][]imageHolder)
	for _, inst := range instances {
		if inst.ImageDigest == "" {
			continue
		}
		held[inst.ImageDigest] = append(held[inst.ImageDigest],
			imageHolder{id: inst.ID, running: instanceMayHaveVMM(inst)})
	}
	for digest := range held {
		sort.Slice(held[digest], func(i, j int) bool { return held[digest][i].id < held[digest][j].id })
	}
	return held, nil
}

// instanceMayHaveVMM reports whether a record can still have a VMM behind it.
//
// A "starting" record with no pid counts as live. run writes that record with
// PID 0 before spawning the VMM, and the pid write afterwards is warn-only, so
// a run killed in that window leaves a live VMM under a pid-0 record. There is
// nothing to check there, and `rm` is the command that resolves it, through
// reapOrphanVMM. Reading it as stopped let `rmi --force` delete the rootfs of
// a running guest and `store compact --force` unmount the volume under it.
//
// Every record that carries a pid goes to processIsAVMM with the record,
// "starting" included. Counting a "starting" record live on the strength of
// its status alone threw away the one check that defeats pid reuse: a run that
// died after the pid write leaves that status behind, and after a reboot the
// number can belong to somebody else's VMM. Such a record then refused every
// removal for good, since `ps` leaves a "starting" record alone and the only
// way out was `rm --force`, which signals that pid (#70).
//
// The record matters as much as the pid. The nil form of processIsAVMM answers
// "is any VMM at this pid", which is true for an unrelated VMM that inherited
// a recycled number; the record adds the started-at comparison `stop` makes.
func instanceMayHaveVMM(inst *store.InstanceState) bool {
	if inst.Status == store.StatusStarting && inst.PID == 0 {
		return true
	}
	return (inst.Status == "running" || inst.Status == store.StatusStarting) &&
		inst.PID > 0 && processIsAVMM(inst.PID, inst)
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
	return digests
}

func holderIDs(held []imageHolder) []string {
	ids := make([]string, 0, len(held))
	for _, h := range held {
		ids = append(ids, h.id)
	}
	return ids
}
