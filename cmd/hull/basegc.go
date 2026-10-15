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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// Bases are removed once nothing on this host needs them
// (docs/migration.md, "Bases"). A base is needed by:
//
//   - an instance whose state.json names it;
//   - a move whose record names it, on either side;
//   - an image whose base it is (bases/images), while the image is in the
//     store, so the image's next instance starts from it;
//   - a destination's prepare that received it (bases/received), so the
//     move it was sent ahead of finds it.
//
// `hull rm` removes the instance's base when nothing else needs it. `hull
// prune` removes every base nothing needs, and the entries of images no
// longer in the store; with --bases it also drops the bases kept for
// images and received ahead of a move. A base younger than baseGrace is
// left alone: a run may be copying it into a disk it has not recorded yet.

// baseGrace is how old a base must be before a sweep may remove it.
const baseGrace = 10 * time.Minute

// receivedBasesDir records the bases a destination's prepare received.
func receivedBasesDir(s *store.Store) string { return filepath.Join(s.BasesDir(), "received") }

// markBaseReceived records that a prepare received the base sum.
func markBaseReceived(s *store.Store, sum string) error {
	if err := os.MkdirAll(receivedBasesDir(s), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(receivedBasesDir(s), sum), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
}

// baseUses lists, by base digest, what on this host needs it. Image
// entries count only while their image is in the store, and only with
// images; received bases only with received.
func baseUses(s *store.Store, images, received bool) (map[string][]string, error) {
	uses := map[string][]string{}
	add := func(sum, who string) {
		if sum != "" {
			uses[sum] = append(uses[sum], who)
		}
	}
	instances, err := s.ListInstances()
	if err != nil {
		return nil, err
	}
	for _, st := range instances {
		if st.Base != nil {
			add(st.Base.SHA256, "instance "+st.ID)
		}
		if rec, err := readMigrationRecord(s.InstanceMigrationRecord(st.ID)); err == nil {
			add(rec.BaseSHA256, fmt.Sprintf("move %s of %s", rec.TxID, st.ID))
		}
	}
	// A move's record outlives a removed instance only in the ledger, and
	// a reservation can exist before the instance does: both are in the
	// instance directories, which a removed instance no longer has.
	if names, err := os.ReadDir(filepath.Join(s.RootDir(), "instances")); err == nil {
		for _, n := range names {
			if rec, err := readMigrationRecord(s.InstanceMigrationRecord(n.Name())); err == nil {
				add(rec.BaseSHA256, fmt.Sprintf("move %s of %s", rec.TxID, n.Name()))
			}
		}
	}
	if images {
		entries, err := imageBaseEntries(s)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if s.ImageComplete(e.Image) {
				add(e.Digest, "image "+e.Image)
			}
		}
	}
	if received {
		names, err := os.ReadDir(receivedBasesDir(s))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, n := range names {
			if checkDiskDigest(n.Name()) == nil {
				add(n.Name(), "received ahead of a move")
			}
		}
	}
	return uses, nil
}

// fileBlocks is the space a file takes on disk, holes left out.
func fileBlocks(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

// removeBase removes the base sum and what records it as received.
func removeBase(s *store.Store, sum string) error {
	if err := checkDiskDigest(sum); err != nil {
		return err
	}
	if err := os.Remove(s.BasePath(sum)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(receivedBasesDir(s), sum))
	_ = os.Remove(spareBasePath(s, sum))
	_ = os.Remove(spareLockPath(s, sum))
	_ = os.Remove(s.BasePath(sum) + ".checked")
	return syncDir(s.BasesDir())
}

// releaseBase removes the base sum, which an instance just removed started
// from, when nothing else on this host needs it.
func releaseBase(s *store.Store, sum string) {
	if sum == "" || !baseHeld(s, sum) {
		return
	}
	uses, err := baseUses(s, true, true)
	if err != nil {
		log.Warnf("Kept base %s: listing what uses it failed: %v", digestHead(sum), err)
		return
	}
	if len(uses[sum]) > 0 {
		return
	}
	if err := removeBase(s, sum); err != nil {
		log.Warnf("Could not remove base %s, which nothing uses: %v", digestHead(sum), err)
	}
}

// baseSweep is one base, entry or leftover a prune removes.
type baseSweep struct {
	path   string
	what   string
	reason string
	bytes  int64
	base   string
}

// baseSweepTargets is what a prune of the bases removes: bases nothing
// needs, the entries of images gone from the store (and, with all, every
// image's entry and received mark whose base is otherwise unused),
// half-written bases of runs and prepares that died, and bases set aside
// as corrupt.
func baseSweepTargets(s *store.Store, all bool, now time.Time) ([]baseSweep, error) {
	names, err := os.ReadDir(s.BasesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uses, err := baseUses(s, !all, !all)
	if err != nil {
		return nil, err
	}
	var out []baseSweep
	for _, n := range names {
		name := n.Name()
		info, err := n.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(s.BasesDir(), name)
		old := now.Sub(info.ModTime()) > baseGrace
		switch sum, isBase := strings.CutSuffix(name, ".ext4"); {
		case isBase && checkDiskDigest(sum) == nil:
			if len(uses[sum]) > 0 || !old {
				continue
			}
			reason := "nothing uses it"
			if all {
				reason = "no instance or move uses it"
			}
			out = append(out, baseSweep{path: path, what: "base " + digestHead(sum), reason: reason,
				bytes: fileBlocks(info), base: sum})
		case strings.Contains(name, ".ext4.bad-"):
			out = append(out, baseSweep{path: path, what: name, reason: "set aside as corrupt",
				bytes: fileBlocks(info)})
		case (strings.HasPrefix(name, ".new-") || strings.HasPrefix(name, ".in-")) &&
			now.Sub(info.ModTime()) > time.Hour:
			out = append(out, baseSweep{path: path, what: name, reason: "left by a run or a prepare that died",
				bytes: fileBlocks(info)})
		}
	}
	entries, err := imageBaseEntries(s)
	if err != nil {
		return nil, err
	}
	for key, e := range entries {
		gone := !s.ImageComplete(e.Image)
		if !gone && (!all || len(uses[e.Digest]) > 0) {
			continue
		}
		reason := "its image is gone from the store"
		if !gone {
			reason = "--bases"
		}
		out = append(out, baseSweep{path: filepath.Join(imageBasesDir(s), key+".json"),
			what: "the base entry of " + e.Image, reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// pruneBasesIn removes what baseSweepTargets lists, or with dryRun says it
// would, and returns the space it freed.
func pruneBasesIn(w io.Writer, s *store.Store, all, dryRun bool) (int64, error) {
	targets, err := baseSweepTargets(s, all, time.Now())
	if err != nil {
		return 0, err
	}
	var freed int64
	var failures []error
	for _, t := range targets {
		if dryRun {
			_, _ = fmt.Fprintf(w, "Would remove %s (%s, %s)\n", t.what, t.reason, formatSize(t.bytes))
			freed += t.bytes
			continue
		}
		var err error
		if t.base != "" {
			err = removeBase(s, t.base)
		} else {
			err = os.Remove(t.path)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "Removed %s (%s, %s)\n", t.what, t.reason, formatSize(t.bytes))
		freed += t.bytes
	}
	return freed, errors.Join(failures...)
}
