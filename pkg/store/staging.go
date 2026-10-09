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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Pull staging directories, and the locks that say whose they are.
//
// A pull unpacks into `<digest>.tmp-<pid>` and displaces the previous image
// to `<digest>.old-<pid>-<unique>` while it commits. Both are swept by the
// next pull into the store and by `hull prune`, and neither may remove one a
// pull is still using: that fails the pull, or publishes an image missing the
// layers that were deleted under it (#114).
//
// So the pull holds a lock on each directory it is using, a flock on the
// directory's own descriptor, for as long as it is using it. The kernel
// releases the lock when the process exits, however it exits, and the lock
// follows the inode through a rename, so the displaced image stays covered
// from the rename aside to the delete. A sweeper tries the lock without
// waiting: busy means a pull is using the directory, acquired means nothing
// is, and the sweeper deletes while holding it, so two sweepers cannot race
// each other and a pull cannot adopt the directory half-way through.
//
// The pid in the name only keeps two pulls' directories apart. Prune used to
// read liveness from it, together with how recently the tree was written, and
// both were guesses: pids are recycled, and a layer's tar restores the mtimes
// of what it unpacks, so a slow pull looked abandoned an hour in.

// ErrStagingInUse says a pull holds the staging directory.
var ErrStagingInUse = errors.New("a pull is using the staging directory")

// StagingLock is an exclusive lock on a directory, held through its open
// descriptor. Close releases it; so does the death of the process.
type StagingLock struct {
	f *os.File
}

// lockDir takes the lock on dir, waiting for it or not. Busy without waiting
// is ErrStagingInUse.
func lockDir(dir string, wait bool) (*StagingLock, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrStagingInUse, filepath.Base(dir))
		}
		return nil, fmt.Errorf("failed to lock %s: %w", dir, err)
	}
	return &StagingLock{f: f}, nil
}

// Close releases the lock. Closing twice is harmless.
func (l *StagingLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

// CreateStaging creates a staging directory and returns it locked.
//
// The lock is on an inode and the name is what the pull goes on to use, so
// the two have to be checked against each other: a sweeper can take and
// delete the directory between the mkdir and the lock, and a pull that kept
// writing into a path the sweeper had just removed would be the bug again.
// The lock waits for a sweeper or a same-name pull (in this process or
// another PID namespace), which can take as long as that pull runs. Then
// the path is checked to still name the inode locked, and the directory
// is made again if not.
func CreateStaging(dir string) (*StagingLock, error) {
	return createStaging(dir, lockDir)
}

// The lock function is passed in so tests can reproduce a sweep between
// creation and lock acquisition without timing-dependent goroutines.
func createStaging(dir string, lock func(string, bool) (*StagingLock, error)) (*StagingLock, error) {
	for range 3 {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create the staging directory: %w", err)
		}
		l, err := lock(dir, true)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if sameInode(l.f, dir) {
			return l, nil
		}
		_ = l.Close()
	}
	return nil, fmt.Errorf("failed to keep the staging directory %s: it was swept each time it was made", dir)
}

// sameInode reports whether path still names the directory f holds open.
func sameInode(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, named)
}

// StagingInFlight reports whether a pull is using a staging directory in
// images/: whether its lock is held. A name nothing is listed under is not
// in flight.
//
// This answers for a listing. A removal does not trust it, because the pull
// could start between the question and the delete; RemoveStagingDir takes
// the lock itself and holds it while it deletes.
func (s *Store) StagingInFlight(name string) bool {
	l, err := lockDir(filepath.Join(s.rootDir, "images", name), false)
	if err != nil {
		return errors.Is(err, ErrStagingInUse)
	}
	_ = l.Close()
	return false
}

// Displaced reports whether a staging name is the `.old-*` kind.
func Displaced(name string) bool {
	return strings.Contains(filepath.Base(name), ".old-")
}

// SweepStaging removes the staging directories in images/ no pull is using,
// and reports which it removed.
//
// The pull path calls this before unpacking, so a store does not accumulate
// the leftovers of interrupted pulls until somebody runs `hull prune`. It
// used to delete every staging directory it found, which took the
// `<digest>.tmp-<pid>` a concurrent pull was still unpacking into: that pull
// then either failed on the missing tree or, when the deletion landed between
// two layers, finished unpacking the remaining ones into a fresh directory and
// published an image with the earlier layers' entries missing. Every later
// pull and run found that image complete and reused it.
//
// The `.old-*` kind has the same shape. A commit renames the previous
// image aside, and renames it back if publishing the new one fails; a sweep
// in between left that rollback nothing to restore.
//
// Nothing in a sweep is unrecoverable: a staging directory nothing holds
// belongs to a pull that will not finish, and the image it was for can be
// pulled again.
func (s *Store) SweepStaging() ([]string, error) {
	names, err := s.StagingDirs()
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, name := range names {
		got, err := s.RemoveStagingDir(name)
		switch {
		case err == nil && got:
			removed = append(removed, name)
		case err == nil:
			// Another sweeper got there first.
		case errors.Is(err, ErrStagingInUse):
			// A pull is using it.
		default:
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// DirDiskUsage sums what a directory tree actually occupies, so the figure
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
func DirDiskUsage(dir string) int64 {
	var total int64
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
		if st.Nlink > 1 {
			if seen[st.Ino] {
				return nil
			}
			seen[st.Ino] = true
		}
		total += st.Blocks * 512
		return nil
	})
	return total
}
