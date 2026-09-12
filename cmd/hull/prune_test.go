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
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// stagingDir plants a pull leftover and returns its path.
func stagingDir(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	dir := filepath.Join(s.RootDir(), "images", name)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

func targetNames(targets []pruneTarget) []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.name)
	}
	sort.Strings(names)
	return names
}

// A usable image nothing supersedes stays, with or without an instance.
func TestPruneKeepsTheUsableImage(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would remove %v", targetNames(targets))
	}
}

// The republished-tag case the issue is about: the older digest can never be
// chosen by a run again, so it is dead weight the moment the newer one lands.
func TestPruneRemovesASupersededDigest(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, PulledAt: time.Now().Add(-24 * time.Hour),
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{Ref: cacheTestRef, Digest: otherTestDigest}, true)

	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, false, false, ""); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	if !strings.Contains(out.String(), "superseded") {
		t.Errorf("output does not say why: %q", out.String())
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 1 || images[0].Digest != otherTestDigest {
		t.Fatalf("prune kept the wrong entry: %v", images)
	}
}

// Two platforms of one tag are two images, not a supersession. Pruning one
// would send the next --platform run back to the registry.
func TestPruneKeepsBothPlatformsOfATag(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, Platform: "linux/arm64",
		PulledAt: time.Now().Add(-24 * time.Hour),
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: otherTestDigest, Platform: "linux/amd64",
	}, true)

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would remove a platform variant: %v", targetNames(targets))
	}
}

// An image with no rootfs is what an interrupted pull leaves. It satisfies no
// lookup and occupies the disk, so it goes without --all.
func TestPruneRemovesAnUnusableImage(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, false)

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].name != cacheTestDigest {
		t.Fatalf("pruneTargets = %v", targetNames(targets))
	}
	if !strings.Contains(targets[0].reason, "unusable") {
		t.Errorf("reason = %q", targets[0].reason)
	}
}

// An image.json that cannot be parsed makes the directory invisible to every
// command. Nothing but this can free it, so it must not be skipped.
func TestPruneRemovesAnUnreadableRecord(t *testing.T) {
	s := newCacheTestStore(t)
	dir := s.ImageDir(cacheTestDigest)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, false, false, ""); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the unreadable entry survived: %v", err)
	}
}

// A directory under images/ that is not named by a digest cannot be handed to
// a recursive delete on the strength of a guess about what it is.
func TestPruneLeavesAnEntryThatIsNotADigest(t *testing.T) {
	s := newCacheTestStore(t)
	stray := filepath.Join(s.RootDir(), "images", "not-a-digest")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would remove %v", targetNames(targets))
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the stray directory was removed: %v", err)
	}
}

// --all is the difference between "unusable" and "unused": a perfectly good
// image nothing refers to only goes when asked for explicitly.
func TestPruneAllRemovesAnUnusedImage(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	targets, err := pruneTargets(s, true, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].name != cacheTestDigest {
		t.Fatalf("pruneTargets = %v", targetNames(targets))
	}
}

// prune never takes an image away from an instance, not even a stopped one and
// not even with --all. `hull rmi --force` is the deliberate way to do that.
func TestPruneNeverRemovesAHeldImage(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, false) // unusable, so it would go on every other ground
	seedInstance(t, s, "held", cacheTestDigest, "stopped", 0)

	targets, err := pruneTargets(s, true, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would remove an image an instance refers to: %v", targetNames(targets))
	}
}

func TestPruneSweepsStagingLeftovers(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	for _, suffix := range []string{".tmp-", ".old-"} {
		dir := stagingDir(t, s, cacheTestDigest+suffix+"999999")
		// Age the whole tree, not just the directory: a `.tmp-` is dated by
		// the newest mtime under it, so ageing the top alone changes nothing.
		old := time.Now().Add(-2 * pullStagingGrace)
		for _, p := range []string{filepath.Join(dir, "rootfs"), dir} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}

	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, false, false, ""); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	staging, err := s.StagingDirs()
	if err != nil {
		t.Fatalf("StagingDirs: %v", err)
	}
	if len(staging) != 0 {
		t.Fatalf("staging leftovers survived: %v", staging)
	}
	if _, err := os.Stat(s.ImageDir(cacheTestDigest)); err != nil {
		t.Errorf("sweeping the leftovers took the image with them: %v", err)
	}
}

// A staging directory belonging to a pull in flight must survive: prune has to
// be safe to run at any moment, and deleting it fails that pull.
func TestPruneLeavesAPullInFlightAlone(t *testing.T) {
	s := newCacheTestStore(t)
	dir := stagingDir(t, s, cacheTestDigest+".tmp-"+strconv.Itoa(os.Getpid()))

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would remove a pull in flight: %v", targetNames(targets))
	}

	// The freshness that protects it is the newest mtime in the tree, not the
	// staging directory's own. A pull writes into rootfs/, and an image big
	// enough can spend an hour there without touching the directory above.
	old := time.Now().Add(-2 * pullStagingGrace)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	targets, err = pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("a pull still writing into rootfs/ was pruned: %v", targetNames(targets))
	}

	// Age the whole tree, and the pid alone must not protect it: pid numbers
	// are recycled, and no pull takes an hour.
	if err := os.Chtimes(filepath.Join(dir, "rootfs"), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	targets, err = pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("an hour-old staging directory was still protected: %v", targetNames(targets))
	}
}

func TestPruneDryRunRemovesNothing(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, false)

	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, false, true, "compact hint"); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	if !strings.Contains(out.String(), "Would remove") {
		t.Errorf("dry run does not say what it would do: %q", out.String())
	}
	if strings.Contains(out.String(), "compact hint") {
		t.Error("a dry run pointed at compaction, though it freed nothing to compact around")
	}
	if _, err := os.Stat(s.ImageDir(cacheTestDigest)); err != nil {
		t.Errorf("the dry run removed the image: %v", err)
	}
}

func TestPruneEmptyStore(t *testing.T) {
	s := newCacheTestStore(t)
	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, true, false, "compact hint"); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing to prune") {
		t.Errorf("output = %q", out.String())
	}
	if strings.Contains(out.String(), "compact hint") {
		t.Error("a prune that freed nothing should not advertise compaction")
	}
}

// The figure printed is the space the host gets back, so it counts blocks and
// counts a hard-linked file once. Unpack creates those for repeated entries.
func TestDirDiskUsageCountsAHardLinkOnce(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("x"), 64*1024)
	first := filepath.Join(dir, "a")
	if err := os.WriteFile(first, body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	single := dirDiskUsage(dir)
	if single < int64(len(body)) {
		t.Fatalf("dirDiskUsage = %d for a %d-byte file", single, len(body))
	}
	if err := os.Link(first, filepath.Join(dir, "b")); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := dirDiskUsage(dir); got != single {
		t.Errorf("a hard link changed the total from %d to %d", single, got)
	}
}

func TestStagingPID(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{cacheTestDigest + ".tmp-4242", 4242},
		{cacheTestDigest + ".old-1", 1},
		{cacheTestDigest + ".tmp-", 0},
		{cacheTestDigest + ".tmp-abc", 0},
		{cacheTestDigest, 0},
	} {
		if got := stagingPID(filepath.Join("/store/images", tc.name)); got != tc.want {
			t.Errorf("stagingPID(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The blocking case from review. `run` picks the newest COMPLETE entry, so an
// incomplete newer one must not supersede the complete older one: prune used
// to remove the incomplete entry as unusable and the complete one as
// superseded by it, leaving the tag with nothing bootable.
func TestPruneKeepsTheLastCompleteEntryOfATag(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, PulledAt: time.Now().Add(-24 * time.Hour),
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: otherTestDigest, PulledAt: time.Now(),
	}, false) // newer, and unusable

	var out bytes.Buffer
	if err := pruneStoreIn(&out, s, false, false, ""); err != nil {
		t.Fatalf("pruneStoreIn: %v", err)
	}
	if _, err := os.Stat(s.ImageDir(otherTestDigest)); !os.IsNotExist(err) {
		t.Errorf("the unusable newer entry survived: %v", err)
	}
	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); !ok {
		t.Fatalf("the tag has no bootable entry left; prune removed it. Output:\n%s", out.String())
	}
}

// A `.old-<pid>` carries the displaced image's mtimes, which say when that
// image was unpacked. Reading them as the entry's age let prune delete the
// rollback copy of a pull that is still running.
func TestPruneKeepsAFreshlyDisplacedImage(t *testing.T) {
	s := newCacheTestStore(t)
	dir := stagingDir(t, s, cacheTestDigest+".old-"+strconv.Itoa(os.Getpid()))
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{filepath.Join(dir, "rootfs"), dir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("prune would delete the rollback copy of a live pull: %v", targetNames(targets))
	}
}

// The two staging kinds hold different things, and the line says which.
func TestPruneNamesWhatEachStagingKindIs(t *testing.T) {
	s := newCacheTestStore(t)
	stagingDir(t, s, cacheTestDigest+".tmp-"+strconv.Itoa(stalePID))
	stagingDir(t, s, cacheTestDigest+".old-"+strconv.Itoa(stalePID))

	targets, err := pruneTargets(s, false, time.Now())
	if err != nil {
		t.Fatalf("pruneTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("pruneTargets = %v, want both staging entries", targetNames(targets))
	}
	reasons := map[string]string{}
	for _, target := range targets {
		reasons[target.name] = target.reason
	}
	if r := reasons[cacheTestDigest+".tmp-"+strconv.Itoa(stalePID)]; !strings.Contains(r, "interrupted pull") ||
		strings.Contains(r, "previous image") {
		t.Errorf(".tmp- reason = %q", r)
	}
	if r := reasons[cacheTestDigest+".old-"+strconv.Itoa(stalePID)]; !strings.Contains(r, "previous image") {
		t.Errorf(".old- reason = %q", r)
	}
}
