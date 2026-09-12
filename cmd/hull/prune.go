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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// pullStagingGrace is how long a staging directory whose pid is still alive is
// left alone.
//
// A `<digest>.tmp-<pid>` belonging to a pull in flight must not be deleted:
// doing so fails that pull, and this command is expected to be safe to run at
// any time. But a pid is weak evidence -- the kernel recycles pid numbers, so
// an unrelated process can inherit the number of the hull that left the
// directory behind, and the entry would then be immortal.
//
// So the pid only protects a directory that was also touched recently. No pull
// takes an hour, and a directory older than that belongs to a hull that is
// long gone whatever is answering to its pid now.
const pullStagingGrace = time.Hour

func pruneCommand() *cli.Command {
	return &cli.Command{
		Name:  "prune",
		Usage: "remove images and pull leftovers nothing needs, reclaiming store space",
		Description: "Without --all this removes what cannot be used: staging directories " +
			"left by an interrupted pull, image directories whose metadata or rootfs is " +
			"missing, and digests superseded by a later pull of the same reference and " +
			"platform. With --all it also removes every image no instance refers to.\n\n" +
			"An image any instance refers to is never removed, running or stopped. Use " +
			"`hull rmi --force` for that case.\n\n" +
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
	targets, err := pruneTargets(s, all, time.Now())
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(w, "Nothing to prune")
		return nil
	}

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
			rmErr = s.RemoveStagingDir(t.name)
		} else {
			rmErr = s.DeleteImage(t.name)
		}
		if rmErr != nil {
			failures = append(failures, rmErr)
			continue
		}
		_, _ = fmt.Fprintf(w, "Removed %s (%s, %s)\n", t.describe(), t.reason, formatSize(t.bytes))
		reclaimed += t.bytes
	}

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
	return errors.Join(failures...)
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
func pruneTargets(s *store.Store, all bool, now time.Time) ([]pruneTarget, error) {
	staging, err := s.StagingDirs()
	if err != nil {
		return nil, err
	}
	var targets []pruneTarget
	for _, name := range staging {
		dir := filepath.Join(s.RootDir(), "images", name)
		bytes, touched := dirUsage(dir)
		if pullInFlight(dir, now, touched) {
			log.Debugf("leaving %s alone: a pull may still be using it", name)
			continue
		}
		reason := "leftover from an interrupted pull"
		if displaced(dir) {
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
			bytes:   bytes,
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
			bytes:  dirDiskUsage(s.ImageDir(name)),
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

// pullInFlight reports whether a staging directory may still belong to a
// running pull. See pullStagingGrace for why both halves are needed.
//
// The two staging kinds date differently. A `.tmp-<pid>` is written into for
// as long as the pull runs, so its age is the newest mtime anywhere in the
// tree: the directory's own mtime only moves when an entry is created directly
// in it, four times over a whole pull, and an image big enough to spend an
// hour unpacking would look untouched.
//
// A `.old-<pid>` is never written into. It arrives by rename at commit time
// and carries the displaced image's mtimes, which say when that image was
// unpacked and nothing about the pull holding it. Reading them let prune
// delete the rollback copy of a live pull whose previous image happened to be
// a month old. Its age is the directory's own ctime, which the rename sets.
func pullInFlight(dir string, now, touched time.Time) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	if displaced(dir) {
		touched = statusChangeTime(fi)
	}
	if now.Sub(touched) > pullStagingGrace {
		return false
	}
	return processAlive(stagingPID(dir))
}

// displaced reports whether a staging name is the `.old-<pid>` kind.
func displaced(dir string) bool {
	return strings.Contains(filepath.Base(dir), ".old-")
}

// statusChangeTime is the inode's ctime, which a rename updates and a copy of
// the file times does not. Zero when the platform does not report one, which
// reads as ancient and prunes.
func statusChangeTime(fi os.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}

// stagingPID reads the pid out of a `<digest>.tmp-<pid>` name, or 0 when the
// name does not carry one.
func stagingPID(dir string) int {
	name := filepath.Base(dir)
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0
	}
	pid, err := strconv.Atoi(name[i+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// processAlive reports whether a pid names a live process. EPERM counts: the
// process exists, it just belongs to somebody else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// dirDiskUsage sums what a directory tree actually occupies, so the figure
// printed is the space the host gets back rather than the size of the layers
// the image was pulled from.
//
// Blocks, not sizes: an unpacked rootfs is mostly small files, each rounded up
// to a block, and a sparse file occupies less than it claims. Hard links are
// counted once -- unpack creates them for repeated layer entries, and counting
// each name would overstate the total.
//
// APFS clones are counted in full, since each clone reports the blocks it
// shares. Nothing hull writes under images/ is a clone of another entry there,
// so the figure is exact for a store hull built and an upper bound for one
// somebody has copied inside with `cp -c`.
//
// Returns what it managed to measure. A walk that fails part-way gives a low
// figure in a report; refusing to delete over it would be worse.
func dirDiskUsage(dir string) int64 {
	total, _ := dirUsage(dir)
	return total
}

// dirUsage is dirDiskUsage plus the newest mtime it saw, which is how the
// staging sweep tells a pull in flight from one that died. One walk answers
// both questions, and the staging path needs both.
func dirUsage(dir string) (int64, time.Time) {
	var total int64
	var newest time.Time
	seen := make(map[uint64]bool)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if mod := info.ModTime(); mod.After(newest) {
			newest = mod
		}
		if st.Nlink > 1 {
			if seen[st.Ino] {
				return nil
			}
			seen[st.Ino] = true
		}
		total += st.Blocks * 512
		return nil
	})
	return total, newest
}
