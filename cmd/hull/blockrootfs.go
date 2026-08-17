// Copyright (c) 2023-2026, Nubificus LTD
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
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/brig-sh/hull/pkg/ociclient"
)

const (
	mke2fsBin  = "/opt/homebrew/opt/e2fsprogs/sbin/mke2fs"
	debugfsBin = "/opt/homebrew/opt/e2fsprogs/sbin/debugfs"
)

// blockInject is a per-instance file placed into the image after it is built.
//
// These used to be written into a staged copy of the rootfs before imaging,
// which is what the copy existed for. Writing them into the finished image
// instead means the image rootfs in the store is never touched and never
// duplicated.
type blockInject struct {
	guestPath string // absolute path inside the image
	content   []byte
	mode      uint32
}

// buildBlockRootfs writes an ext4 image of rootfsDir, then puts back what the
// host filesystem could not carry.
//
// rootfsDir is read and never written, so it can be the shared image rootfs
// straight out of the store. That is the point: the previous version cloned
// the whole tree per instance purely so it had somewhere to drop three files,
// and `cp` on macOS clears setuid and setgid on every file it copies unless it
// runs as root -- so the clone was also, silently, what disarmed sudo.
//
// mke2fs writes the modes faithfully, including setuid, but stamps every inode
// with the *host* uid and gid: an unprivileged unpack cannot own anything as
// root, so the whole image came out owned by the user who ran hull. The
// ownership the image actually declares was recorded per file at unpack time
// (see ociclient.GuestAttr) and is restored here, straight into the finished
// filesystem, where ext4's inodes can hold what APFS could not.
func buildBlockRootfs(diskPath, rootfsDir string, injects []blockInject, sizeMB int) error {
	mkCmd := exec.Command(mke2fsBin, "-t", "ext4", "-d", rootfsDir,
		"-L", "rootfs", "-m", "0", "-q", diskPath, fmt.Sprintf("%dM", sizeMB))
	if out, err := mkCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create ext4 image: %s: %w", string(out), err)
	}

	// The injected files need to exist on the host for debugfs to copy them in.
	// Three small files beside the image, not a second copy of the rootfs.
	stage, err := os.MkdirTemp(filepath.Dir(diskPath), "block-inject-")
	if err != nil {
		return fmt.Errorf("failed to stage block image files: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	var script bytes.Buffer
	for i, inj := range injects {
		hostPath := filepath.Join(stage, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(hostPath, inj.content, 0600); err != nil {
			return fmt.Errorf("failed to stage %s: %w", inj.guestPath, err)
		}
		// rm first: write onto an existing name leaves the old inode behind.
		// A missing file makes debugfs complain and carry on, which is the
		// correct outcome for a path the image did not have.
		fmt.Fprintf(&script, "rm %s\n", debugfsPath(inj.guestPath))
		fmt.Fprintf(&script, "write %s %s\n", debugfsPath(hostPath), debugfsPath(inj.guestPath))
		// S_IFREG included deliberately: debugfs writes the whole i_mode field,
		// so a permission-only value would leave the inode with no file type
		// at all. See appendOwnershipScript.
		fmt.Fprintf(&script, "sif %s mode 0%o\n", debugfsPath(inj.guestPath), 0o100000|inj.mode)
		fmt.Fprintf(&script, "sif %s uid 0\n", debugfsPath(inj.guestPath))
		fmt.Fprintf(&script, "sif %s gid 0\n", debugfsPath(inj.guestPath))
	}

	skipped, err := appendOwnershipScript(&script, rootfsDir)
	if err != nil {
		return err
	}
	if skipped > 0 {
		// Not fatal, but not silent either: those files keep the host's
		// ownership inside the guest, and a caller staring at one of them
		// deserves to know why.
		log.Warnf("%d path(s) could not be given their recorded ownership in the "+
			"block image because the name is not expressible as a debugfs argument", skipped)
	}
	if script.Len() == 0 {
		return nil
	}

	scriptPath := filepath.Join(stage, "fixups.debugfs")
	if err := os.WriteFile(scriptPath, script.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to write block image fixups: %w", err)
	}
	dbCmd := exec.Command(debugfsBin, "-w", "-f", scriptPath, diskPath)
	out, err := dbCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to apply block image fixups: %s: %w", string(out), err)
	}
	// debugfs exits 0 whatever happened. It reports a command it could not run
	// on stdout and carries on to the next one, so a filesystem it failed to
	// open at all -- every subsequent command answering "Filesystem not open"
	// -- is indistinguishable from success by exit code, and the first sign of
	// it is a guest that will not boot. Read the output instead.
	if bad := debugfsFailure(out); bad != "" {
		return fmt.Errorf("failed to apply block image fixups: %s", bad)
	}
	return repairBlockImage(diskPath)
}

// repairBlockImage settles the filesystem after debugfs has edited it.
//
// debugfs writes inodes and directory entries without maintaining the
// summaries that describe them, so unlinking and creating files leaves the
// group descriptors disagreeing with reality. Linux notices on mount, and
// because the root filesystem defaults to errors=remount-ro it mounts the
// image READ-ONLY -- at which point the init cannot copy urunit into place and
// the kernel panics on an init that will not execute. The image is otherwise
// perfectly good, which is what makes this worth a comment: the symptom
// appears at boot, three layers away from the cause.
//
// e2fsck recomputes the summaries. It costs a few tens of milliseconds here,
// against an image that has never been mounted.
func repairBlockImage(diskPath string) error {
	// -f forces a full check even though the filesystem looks clean, which it
	// does: nothing has mounted it. -y answers the questions, all of which are
	// "recount this".
	cmd := exec.Command("/opt/homebrew/opt/e2fsprogs/sbin/e2fsck", "-f", "-y", diskPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// e2fsck reports what it did through the exit code: 1 means it found
	// problems and fixed them, which is precisely why it was run. 2 adds
	// "reboot the system", meaningless for an image nothing has mounted.
	// Anything from 4 up is a filesystem it could not correct.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() <= 2 {
		return nil
	}
	return fmt.Errorf("failed to check the block image: %s: %w", string(out), err)
}

// appendOwnershipScript emits the ownership every path in the image declares.
//
// Only paths that carry a record are touched. Anything else keeps what mke2fs
// gave it, which is the right answer for a file the image did not bring.
func appendOwnershipScript(w *bytes.Buffer, rootfsDir string) (skipped int, err error) {
	buf := bufio.NewWriter(w)
	walkErr := filepath.WalkDir(rootfsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A tree we cannot walk fully is worth reporting, but one
			// unreadable entry is not worth failing a boot over.
			log.Debugf("block image ownership: skipping %s: %v", path, err)
			return nil
		}
		symlink := d.Type()&fs.ModeSymlink != 0
		mode, uid, gid, ok := ociclient.GuestAttr(path, symlink)
		if !ok {
			return nil
		}
		rel, relErr := filepath.Rel(rootfsDir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		guestPath := "/" + filepath.ToSlash(rel)
		if !debugfsExpressible(guestPath) {
			skipped++
			return nil
		}
		q := debugfsPath(guestPath)
		fmt.Fprintf(buf, "sif %s uid %d\n", q, uid)
		fmt.Fprintf(buf, "sif %s gid %d\n", q, gid)
		// Ownership only. The mode is already right -- the unpack put the
		// image's mode on the host file, setuid included, and mke2fs copies it
		// faithfully.
		//
		// Setting it here would also be actively destructive: debugfs writes
		// the whole i_mode field, file-type bits and all, so a permission-only
		// value silently clears S_IFDIR and turns every directory it touches
		// into a regular file. That corrupts the filesystem so thoroughly that
		// debugfs cannot reopen it, every later command fails with
		// "Filesystem not open" -- and debugfs still exits 0, so nothing
		// reports it until the guest kernel refuses the mount.
		_ = mode
		return nil
	})
	if flushErr := buf.Flush(); flushErr != nil {
		return skipped, flushErr
	}
	if walkErr != nil {
		return skipped, fmt.Errorf("failed to read image ownership: %w", walkErr)
	}
	return skipped, nil
}

// debugfsPath quotes a path for debugfs, whose parser splits on whitespace.
func debugfsPath(p string) string { return `"` + p + `"` }

// debugfsExpressible reports whether a path survives being passed to debugfs.
//
// Quoting covers spaces and tabs. A double quote or a newline in the name has
// no escape that debugfs's parser honours, so those are left alone rather than
// guessed at -- a mangled command would set the wrong inode, which is worse
// than setting none.
func debugfsExpressible(p string) bool {
	return !strings.ContainsAny(p, "\"\n\r")
}

// debugfsFailure returns the first line of output that means a command did not
// take effect, or "" when everything applied.
//
// "File not found" is deliberately not one of them: the injects unlink a path
// before writing it, and a path the image did not already have is the ordinary
// case rather than a problem.
func debugfsFailure(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.Contains(line, "Filesystem not open"),
			strings.Contains(line, "checksum does not match"),
			strings.Contains(line, "Bad magic number"),
			strings.Contains(line, "Corrupt "):
			return line
		}
	}
	return ""
}

// restoreClonedModes puts back the setuid, setgid and sticky bits that macOS
// `cp` drops.
//
// cp clears S_ISUID and S_ISGID on everything it copies unless it runs as
// root -- a sensible default for a general-purpose tool, and wrong for a copy
// of a container image, where those bits are part of what the image is. The
// result is a rootfs whose sudo, su, passwd and mount are all disarmed, and
// the only symptom is the guest refusing to run them.
//
// It is a sibling of restoreClonedHardlinks: both put back something cp did
// not carry across, from the record the unpack left beside each file. Files
// with nothing recorded, and files whose recorded mode has no special bits,
// are left alone -- which is nearly all of them, so this walks a large tree
// and writes to a handful of paths.
func restoreClonedModes(clone string) error {
	return filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A symlink has no mode of its own worth restoring, and chmod would
		// follow it out of the tree.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		mode, _, _, ok := ociclient.GuestAttr(path, false)
		if !ok || mode&0o7000 == 0 {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		// Only the files that actually lost something.
		if uint32(info.Mode().Perm())|(mode&0o7000) == mode&0o7777 &&
			info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return nil
		}
		if chErr := syscall.Chmod(path, mode&0o7777); chErr != nil {
			return fmt.Errorf("restore mode on %s: %w", path, chErr)
		}
		return nil
	})
}
