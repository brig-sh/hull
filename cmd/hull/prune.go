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
	"path/filepath"
	"sort"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

func pruneCommand() *cli.Command {
	return &cli.Command{
		Name:  "prune",
		Usage: "remove images and pull leftovers nothing needs, reclaiming store space",
		Description: "Without --all this removes what cannot be used: staging directories " +
			"left by an interrupted pull, image directories whose metadata or rootfs is " +
			"missing, and digests superseded by a later pull of the same reference and " +
			"platform. With --all it also removes every image no instance refers to.\n\n" +
			"An image any instance refers to is never removed, running or stopped. Stop " +
			"running instances first, then use `hull rmi --force` for that case.\n\n" +
			"Nothing removed here is unrecoverable: every image can be pulled again.\n\n" +
			"Deleting inside the store does not shrink its backing sparse image. Run " +
			"`hull store compact` afterwards to return the space to the host.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "also remove every image no instance refers to, not just unusable ones",
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "print what would be removed and remove nothing",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			return pruneStore(cmd)
		},
	}
}

func pruneStore(cmd *cli.Command) error {
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	storeDir := cmd.String("store-dir")
	hint := ""
	if _, statErr := os.Stat(storeImagePath(storeDir, defaultStoreDir())); statErr == nil {
		hint = "Run `hull store compact` to return the reclaimed space to the host."
	}
	return pruneStoreIn(os.Stdout, s, cmd.Bool("all"), cmd.Bool("dry-run"), hint)
}

// pruneTarget is one directory prune has decided to remove.
type pruneTarget struct {
	// name is the directory name under images/, which is the digest for an
	// image and a staging name for pull scaffolding.
	name string
	// staging says which of the two it is, and so which store call removes it.
	staging bool
	// ref is what the image was pulled under, empty when no record was read.
	ref string
	// reason is printed, so it has to read as an explanation on its own.
	reason string
	bytes  int64
}

func pruneStoreIn(w io.Writer, s *store.Store, all, dryRun bool, compactHint string) error {
	targets, err := pruneTargets(s, all)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(w, "Nothing to prune")
		return nil
	}

	reclaimed, err := removePruneTargets(w, s, targets, dryRun)

	verb := "Reclaimed"
	if dryRun {
		verb = "Would reclaim"
	}
	_, _ = fmt.Fprintf(w, "%s %s\n", verb, formatSize(reclaimed))
	// Not after a dry run: nothing has been freed yet, so pointing at
	// compaction there would be advice to compact around what is still in the
	// store.
	if compactHint != "" && reclaimed > 0 && !dryRun {
		_, _ = fmt.Fprintln(w, compactHint)
	}
	return err
}

// removePruneTargets removes (or, on a dry run, reports) each target, and
// returns the bytes reclaimed along with every removal that failed.
func removePruneTargets(w io.Writer, s *store.Store, targets []pruneTarget, dryRun bool) (int64, error) {
	var reclaimed int64
	var failures []error
	for _, t := range targets {
		if dryRun {
			_, _ = fmt.Fprintf(w, "Would remove %s (%s, %s)\n", t.describe(), t.reason, formatSize(t.bytes))
			reclaimed += t.bytes
			continue
		}
		var rmErr error
		if t.staging {
			var got bool
			got, rmErr = s.RemoveStagingDir(t.name)
			if rmErr == nil && !got {
				// A pull's sweep took it between the listing and now. The
				// space is free, but not by this run, so it is not counted.
				_, _ = fmt.Fprintf(w, "Already gone: %s (%s)\n", t.describe(), t.reason)
				continue
			}
		} else {
			rmErr = s.DeleteImage(t.name)
		}
		if errors.Is(rmErr, store.ErrStagingInUse) {
			// A pull started using it between the listing and now. Not a
			// failure: it is exactly what the listing would have skipped.
			_, _ = fmt.Fprintf(w, "Left %s alone (a pull is using it)\n", t.describe())
			continue
		}
		if rmErr != nil {
			failures = append(failures, rmErr)
			continue
		}
		_, _ = fmt.Fprintf(w, "Removed %s (%s, %s)\n", t.describe(), t.reason, formatSize(t.bytes))
		reclaimed += t.bytes
	}
	return reclaimed, errors.Join(failures...)
}

func (t pruneTarget) describe() string {
	if t.ref == "" {
		return t.name
	}
	return fmt.Sprintf("%s %s", t.name, t.ref)
}

// pruneTargets decides what goes.
//
// The listing starts from the directory entries rather than from the records,
// because an entry whose image.json cannot be read is invisible to every
// command and still occupies the disk. Nothing else can free it.
//
// The decision is a snapshot. A `hull run` starting between it and the delete
// can claim an image this call has already written off: run writes the bundle
// symlink into the cache in GenerateBundle, and the first record carrying
// ImageDigest is saved later, inside launchVMM, so the image has no holder for
// that span. Such a run fails on a missing rootfs; nothing re-pulls, because
// resolveImageDigest has already returned. The store's lock does not close the
// window from here, since it is taken per operation and holding it across a
// whole prune would block the instance creation the check is looking for.
// Saving the record before GenerateBundle would close it, and that is a change
// to the run path.
func pruneTargets(s *store.Store, all bool) ([]pruneTarget, error) {
	staging, err := s.StagingDirs()
	if err != nil {
		return nil, err
	}
	var targets []pruneTarget
	for _, name := range staging {
		if s.StagingInFlight(name) {
			log.Debugf("leaving %s alone: a pull is using it", name)
			continue
		}
		reason := "leftover from an interrupted pull"
		if store.Displaced(name) {
			// Not the same thing as a half-written `.tmp-`: this is the
			// previous image, renamed aside by a pull that then died before
			// deleting it. It is a whole image, and the line should say so
			// rather than read as scaffolding.
			reason = "previous image left behind by an interrupted pull"
		}
		targets = append(targets, pruneTarget{
			name:    name,
			staging: true,
			reason:  reason,
			bytes:   store.DirDiskUsage(filepath.Join(s.RootDir(), "images", name)),
		})
	}

	names, err := s.ImageDirNames()
	if err != nil {
		return nil, err
	}
	images, err := s.ListImages()
	if err != nil {
		return nil, err
	}
	held, err := instancesByImage(s)
	if err != nil {
		return nil, err
	}
	byDigest := make(map[string]*store.ImageMetadata, len(images))
	for _, img := range images {
		byDigest[img.Digest] = img
	}
	superseded := supersededDigests(images, s.ImageComplete)

	for _, name := range names {
		if len(held[name]) > 0 {
			continue
		}
		img, recorded := byDigest[name]
		reason := ""
		switch {
		case !recorded:
			// Either the metadata is unreadable, or the directory name is not
			// the digest it claims to key. Both are junk; only the first can
			// be deleted safely, because DeleteImage will not recurse through
			// a name that is not a digest.
			if err := store.ValidateImageDigest(name); err != nil {
				log.Warnf("images/%s has no readable metadata and is not named by a digest, "+
					"so prune is leaving it alone: %v", name, err)
				continue
			}
			reason = "no readable metadata"
		case !s.ImageComplete(name):
			reason = "unusable: no rootfs, or unpacked by an older hull"
		case superseded[name] != "":
			// The digest that replaced it is named short, the way the DIGEST
			// column of `hull images` names one: this is context for a line
			// that already carries a full digest, not something to copy.
			reason = fmt.Sprintf("superseded by %s", shortDigest(superseded[name]))
		case all:
			reason = "not used by any instance"
		default:
			continue
		}
		t := pruneTarget{
			name:   name,
			reason: reason,
			bytes:  store.DirDiskUsage(s.ImageDir(name)),
		}
		if recorded {
			t.ref = img.Ref
		}
		targets = append(targets, t)
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets, nil
}

// supersededDigests reports, for each image a later pull of the same reference
// and platform replaced, the digest that replaced it.
//
// The store is keyed by manifest digest, so a republished tag adds an entry
// instead of rewriting one, and the older entry stays for as long as the store
// does. `hull run` resolves such a group to the most recently pulled complete
// member, which is the one kept here: prune removes what run would never
// choose.
//
// Only a complete image supersedes. cachedDigest skips an incomplete entry, so
// an incomplete newer one does not hide an older usable one, and prune has to
// read the group the same way. Reading it by PulledAt alone removed the last
// bootable entry of a tag: the incomplete entry went as unusable, the complete
// one went as superseded by it, and the tag was left with nothing.
//
// The group is the reference as written, not the repository. Two references to
// one repository -- a tag and a digest pin -- are different intentions, and an
// image pinned by digest is named by something that cannot be republished, so
// it never supersedes and is never superseded.
func supersededDigests(images []*store.ImageMetadata, complete func(digest string) bool) map[string]string {
	newest := make(map[string]*store.ImageMetadata)
	key := func(img *store.ImageMetadata) string {
		platform := img.Platform
		if platform == "" {
			platform = ociclient.DefaultPlatform
		}
		return img.Ref + "\x00" + platform
	}
	for _, img := range images {
		if !complete(img.Digest) {
			continue
		}
		k := key(img)
		best, seen := newest[k]
		if !seen || newer(img, best) {
			newest[k] = img
		}
	}
	superseded := make(map[string]string)
	for _, img := range images {
		keeper, ok := newest[key(img)]
		if ok && keeper.Digest != img.Digest {
			superseded[img.Digest] = keeper.Digest
		}
	}
	return superseded
}

// newer breaks a timestamp tie on the digest, so two records pulled within the
// same clock tick do not prune each other depending on readdir order.
func newer(a, b *store.ImageMetadata) bool {
	if a.PulledAt.Equal(b.PulledAt) {
		return a.Digest > b.Digest
	}
	return a.PulledAt.After(b.PulledAt)
}
