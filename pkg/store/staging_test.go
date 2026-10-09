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

package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const stagingTestDigest = "sha256:c408baae42f5c74c0661fbc20a289fd23d4322988e52c88cd54108e5c4c74893"

// plantStaging writes a staging directory with one file under rootfs/, the
// way a pull in flight has one, and returns its path.
func plantStaging(t *testing.T, s *Store, name string) string {
	t.Helper()
	dir := filepath.Join(s.RootDir(), "images", name)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs", "layer"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// holdStaging takes the lock a pull holds on its staging directory, and
// releases it when the test ends.
func holdStaging(t *testing.T, dir string) *StagingLock {
	t.Helper()
	l, err := lockDir(dir, false)
	if err != nil {
		t.Fatalf("lockDir: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
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
	single := DirDiskUsage(dir)
	if single < int64(len(body)) {
		t.Fatalf("DirDiskUsage = %d for a %d-byte file", single, len(body))
	}
	if err := os.Link(first, filepath.Join(dir, "b")); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := DirDiskUsage(dir); got != single {
		t.Errorf("a hard link changed the total from %d to %d", single, got)
	}
}

// The bug in #114: the sweep a pull runs before unpacking removed every
// staging directory it found, including the one a concurrent pull was still
// unpacking into and the previous image another was holding aside to roll
// back to. A pull holds a lock on each; both must survive, and the leftovers
// of a pull that is gone -- whose lock died with it -- must not.
func TestSweepStagingLeavesALockedDirectoryAlone(t *testing.T) {
	s := newTestStore(t)
	live := []string{stagingTestDigest + ".tmp-1", stagingTestDigest + ".old-1"}
	gone := []string{stagingTestDigest + ".tmp-2", stagingTestDigest + ".old-2"}
	for _, name := range live {
		holdStaging(t, plantStaging(t, s, name))
	}
	for _, name := range gone {
		plantStaging(t, s, name)
	}
	for _, name := range live {
		if !s.StagingInFlight(name) {
			t.Errorf("%s is locked and not in flight", name)
		}
	}
	for _, name := range gone {
		if s.StagingInFlight(name) {
			t.Errorf("%s is unlocked and in flight", name)
		}
	}

	removed, err := s.SweepStaging()
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	sort.Strings(removed)
	sort.Strings(gone)
	if !reflect.DeepEqual(removed, gone) {
		t.Errorf("SweepStaging removed %v, want %v", removed, gone)
	}
	for _, name := range live {
		if _, err := os.Stat(filepath.Join(s.RootDir(), "images", name, "rootfs", "layer")); err != nil {
			t.Errorf("the sweep took a live pull's %s: %v", name, err)
		}
	}
	for _, name := range gone {
		if _, err := os.Stat(filepath.Join(s.RootDir(), "images", name)); !os.IsNotExist(err) {
			t.Errorf("%s of a dead pull survived the sweep: %v", name, err)
		}
	}
}

// A removal does not trust a listing: it takes the lock itself and refuses
// when a pull holds it, so a pull that started between the two is safe.
func TestRemoveStagingDirRefusesALockedDirectory(t *testing.T) {
	s := newTestStore(t)
	name := stagingTestDigest + ".tmp-1"
	l := holdStaging(t, plantStaging(t, s, name))
	if _, err := s.RemoveStagingDir(name); !errors.Is(err, ErrStagingInUse) {
		t.Fatalf("RemoveStagingDir on a locked directory = %v, want ErrStagingInUse", err)
	}
	_ = l.Close()
	if got, err := s.RemoveStagingDir(name); err != nil || !got {
		t.Fatalf("RemoveStagingDir once released = %v, %v; want true, nil", got, err)
	}
	if s.StagingInFlight(name) {
		t.Error("a removed directory reads as in flight")
	}
}

// CreateStaging hands back the directory locked, and a sweep cannot take it
// from under the pull. Releasing the lock is what makes it sweepable.
func TestCreateStagingIsHeldUntilClosed(t *testing.T) {
	s := newTestStore(t)
	name := stagingTestDigest + ".tmp-1"
	dir := filepath.Join(s.RootDir(), "images", name)
	l, err := CreateStaging(dir)
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	if removed, err := s.SweepStaging(); err != nil || len(removed) != 0 {
		t.Fatalf("SweepStaging took a directory a pull just created: %v, %v", removed, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if removed, err := s.SweepStaging(); err != nil || len(removed) != 1 {
		t.Fatalf("SweepStaging after release: %v, %v", removed, err)
	}
}

// The lock is on the inode and the name is what the pull uses, so they are
// checked against each other: a directory swept between the mkdir and the
// lock is made again, and the pull ends up holding the one it will write to.
func TestCreateStagingKeepsTheNameItLocked(t *testing.T) {
	s := newTestStore(t)
	name := stagingTestDigest + ".tmp-1"
	dir := filepath.Join(s.RootDir(), "images", name)
	l, err := CreateStaging(dir)
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	if !sameInode(l.f, dir) {
		t.Error("the lock held is not on the directory named")
	}
	// What a sweep looks like from the pull's side: the path no longer
	// names the inode held.
	if err := os.Rename(dir, dir+".swept"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if sameInode(l.f, dir) {
		t.Error("a missing path still matched the inode held")
	}
	_ = l.Close()
}

// Prune lists, then removes, and a pull's sweep can take the leftover in
// between. The outcome prune wanted is there either way, so a directory that
// is already gone is not a failed removal.
func TestRemoveStagingDirAcceptsAGoneDirectory(t *testing.T) {
	s := newTestStore(t)
	got, err := s.RemoveStagingDir(stagingTestDigest + ".tmp-1")
	if err != nil {
		t.Fatalf("RemoveStagingDir on a directory that is not there: %v", err)
	}
	if got {
		t.Error("RemoveStagingDir claims to have removed a directory that was not there")
	}
}

// Exercise both sweep windows: before the directory is opened, and after
// its inode is opened but before the creator checks the name it will use.
func TestCreateStagingRetriesAfterSweep(t *testing.T) {
	for _, when := range []string{"before open", "after open", "replaced inode"} {
		t.Run(when, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "staging")
			attempts := 0
			var discarded *StagingLock
			held, err := createStaging(dir, func(dir string, wait bool) (*StagingLock, error) {
				attempts++
				if attempts != 1 {
					return lockDir(dir, wait)
				}
				if when != "before open" {
					var err error
					discarded, err = lockDir(dir, wait)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = discarded.Close() })
				}
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if when == "replaced inode" {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if discarded != nil {
					return discarded, nil
				}
				return lockDir(dir, wait)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			if attempts != 2 || !sameInode(held.f, dir) {
				t.Fatalf("after %d attempts the lock must name the surviving directory", attempts)
			}
			if discarded != nil && discarded.f != nil {
				t.Error("retry leaked the discarded inode's lock")
			}
			if other, err := lockDir(dir, false); !errors.Is(err, ErrStagingInUse) {
				_ = other.Close()
				t.Fatalf("returned directory is not locked: %v", err)
			}
		})
	}
}

func TestCreateStagingStopsAfterRepeatedSweeps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staging")
	attempts := 0
	held, err := createStaging(dir, func(dir string, wait bool) (*StagingLock, error) {
		attempts++
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		return lockDir(dir, wait)
	})
	if held != nil || err == nil || attempts != 3 || !strings.Contains(err.Error(), "swept each time") {
		t.Fatalf("CreateStaging = %v, %v after %d attempts", held, err, attempts)
	}
}

func TestCreateStagingReturnsLockFailure(t *testing.T) {
	want := errors.New("lock failed")
	attempts := 0
	held, err := createStaging(filepath.Join(t.TempDir(), "staging"), func(string, bool) (*StagingLock, error) {
		attempts++
		return nil, want
	})
	if held != nil || !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("CreateStaging = %v, %v after %d attempts", held, err, attempts)
	}
}
