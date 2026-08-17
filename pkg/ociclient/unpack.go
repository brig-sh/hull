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

package ociclient

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/archive"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("subsystem", "ociclient")

// UnpackLayers extracts all OCI image layers to the rootfs directory. p may be
// nil, in which case nothing is reported.
func UnpackLayers(layers []v1.Layer, rootfsDir string, p *Progress) error {
	// Create rootfs directory
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return fmt.Errorf("failed to create rootfs directory: %w", err)
	}

	// Apply each layer in order
	for i, layer := range layers {
		p.startLayer(i + 1)
		if err := unpackLayerWithRetry(layer, rootfsDir, i, p); err != nil {
			return fmt.Errorf("failed to unpack layer %d: %w", i, err)
		}
	}

	return nil
}

// layerAttempts is how many times a single layer is re-fetched before the pull
// gives up.
const layerAttempts = 4

// unpackLayerWithRetry re-fetches a layer whose stream broke part-way through.
//
// Layers arrive as one long-lived HTTP stream, and the big ones are exposed:
// the desktop image has a single 552MB layer, and losing its stream at any
// point discarded the entire ~0.7GB pull with no way to resume. Re-applying a
// layer over its own partial extraction is safe -- tar overwrites what it
// already wrote -- so a retry costs bandwidth, not correctness.
//
// Only transport failures are retried. A permission problem fails the same way
// every time, and retrying it just hides the real error. Truncation is the
// ambiguous case: a cut-short download and a malformed archive both surface as
// io.ErrUnexpectedEOF and cannot be told apart here, so those are retried and
// then reported -- costing one wasted fetch on genuinely bad content.
func unpackLayerWithRetry(layer v1.Layer, rootfsDir string, index int, p *Progress) error {
	var err error
	for attempt := 1; attempt <= layerAttempts; attempt++ {
		err = unpackLayer(layer, rootfsDir, index, p)
		if err == nil {
			return nil
		}
		if !isTransientTransportErr(err) || attempt == layerAttempts {
			return err
		}
		backoff := time.Duration(attempt) * time.Second
		log.Warnf("layer %d: %v; retrying in %s (attempt %d/%d)",
			index, err, backoff, attempt+1, layerAttempts)
		p.retryLayer()
		time.Sleep(backoff)
	}
	return err
}

// isTransientTransportErr reports whether an error is a stream or connection
// failure worth retrying rather than a problem with the content itself.
func isTransientTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	for _, sub := range []string{
		"PROTOCOL_ERROR",
		"stream error",
		"unexpected EOF",
		"connection reset",
		"broken pipe",
		"INTERNAL_ERROR",
		"http2:",
		"TLS handshake timeout",
		"i/o timeout",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// unpackLayer extracts a single OCI layer to the rootfs directory.
//
// containerd's archive.Apply runs first. It chowns as it extracts, so it fails
// with "operation not permitted" on root-owned entries when we are not root,
// which on macOS is the normal case -- but it does apply everything up to that
// point, and dropping it in favour of the manual extractor alone broke
// checkpoint/restore in e2e. Whatever it contributes is load-bearing, so it
// stays until that is understood.
func unpackLayer(layer v1.Layer, rootfsDir string, index int, p *Progress) error {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("failed to get layer content: %w", err)
	}
	defer func() { _ = rc.Close() }()
	// Counting here covers both the network fetch and the extraction: the
	// reader is lazy, so bytes only move as we consume them.
	var src io.Reader = &progressReader{r: rc, p: p}

	log.Debugf("Unpacking layer %d to %s", index, rootfsDir)

	ctx := context.Background()
	_, err = archive.Apply(ctx, rootfsDir, src)

	// Permission errors are expected as a non-root user; finish with the
	// manual extractor, which skips chown.
	permissionErr := err != nil && strings.Contains(err.Error(), "operation not permitted")
	if err != nil && !permissionErr {
		return fmt.Errorf("failed to apply layer: %w", err)
	}

	// The manual pass is not only a fallback. It is the one that holds the tar
	// header, so it is the only place the image's ownership can be recorded --
	// see recordGuestAttr -- and on macOS nothing else can express it. It
	// therefore runs even when archive.Apply got all the way through, rather
	// than only when it tripped over a chown it was never going to be allowed
	// to make.
	if !permissionErr && !recordsOwnership {
		log.Debugf("Layer %d unpacked successfully", index)
		return nil
	}

	rc2, rerr := layer.Uncompressed()
	if rerr != nil {
		return fmt.Errorf("failed to reopen layer: %w", rerr)
	}
	defer func() { _ = rc2.Close() }()

	// Only count the bytes once: on the permission path this is the extraction
	// the caller is watching, but when it follows a successful apply the layer
	// has already been reported in full and counting it twice would take the
	// progress past 100%.
	var second io.Reader = rc2
	if permissionErr {
		second = &progressReader{r: rc2, p: p}
	}
	if err := extractTarIgnoreChown(second, rootfsDir); err != nil {
		return fmt.Errorf("failed to extract layer with fallback: %w", err)
	}

	log.Debugf("Layer %d unpacked successfully (manual pass)", index)
	return nil
}

// resolveImageUser resolves an OCI image config User string ("name",
// "uid", "name:group", "uid:gid", and mixtures) against the extracted
// rootfs's /etc/passwd and /etc/group. It returns the numeric ids and the
// user's home directory ("" when unknown).
func resolveImageUser(rootfsDir, spec string) (uint32, uint32, string, error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")

	var uid, gid uint32
	home := ""

	if n, err := strconv.ParseUint(userPart, 10, 32); err == nil {
		uid = uint32(n)
		gid = uint32(n)
		// Best effort: pick up gid/home when the uid has a passwd entry.
		if fields := passwdLookup(rootfsDir, 2, userPart); fields != nil {
			if g, gerr := strconv.ParseUint(fields[3], 10, 32); gerr == nil {
				gid = uint32(g)
			}
			home = fields[5]
		}
	} else {
		fields := passwdLookup(rootfsDir, 0, userPart)
		if fields == nil {
			return 0, 0, "", fmt.Errorf("user %q not found in image /etc/passwd", userPart)
		}
		u, uerr := strconv.ParseUint(fields[2], 10, 32)
		g, gerr := strconv.ParseUint(fields[3], 10, 32)
		if uerr != nil || gerr != nil {
			return 0, 0, "", fmt.Errorf("malformed passwd entry for %q", userPart)
		}
		uid, gid = uint32(u), uint32(g)
		home = fields[5]
	}

	if hasGroup {
		if n, err := strconv.ParseUint(groupPart, 10, 32); err == nil {
			gid = uint32(n)
		} else {
			fields := fileLookup(filepath.Join(rootfsDir, "etc", "group"), 0, groupPart, 4)
			if fields == nil {
				return 0, 0, "", fmt.Errorf("group %q not found in image /etc/group", groupPart)
			}
			g, gerr := strconv.ParseUint(fields[2], 10, 32)
			if gerr != nil {
				return 0, 0, "", fmt.Errorf("malformed group entry for %q", groupPart)
			}
			gid = uint32(g)
		}
	}

	return uid, gid, home, nil
}

// passwdLookup returns the fields of the /etc/passwd line whose field
// `keyIdx` equals `key`, or nil.
func passwdLookup(rootfsDir string, keyIdx int, key string) []string {
	return fileLookup(filepath.Join(rootfsDir, "etc", "passwd"), keyIdx, key, 7)
}

// fileLookup scans a colon-separated database for a line whose field
// `keyIdx` equals `key` and has at least minFields fields.
func fileLookup(path string, keyIdx int, key string, minFields int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= minFields && fields[keyIdx] == key {
			return fields
		}
	}
	return nil
}

// removeForReplace deletes a path so it can be rewritten, coping with the
// read-only modes that appear inside container layers. Deleting a file depends
// on the parent directory being writable, not the file, so a read-only parent
// is relaxed first; the layer's own permissions are reapplied afterwards by
// the extraction, so widening here does not leak into the result.
//
// Both the removal and the relaxing go through root, so a layer cannot point
// either of them at a host file by planting a symlink first.
func removeForReplace(root *os.Root, name string) error {
	err := root.Remove(name)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	parent := filepath.Dir(name)
	fi, statErr := root.Stat(parent)
	if statErr != nil {
		return err
	}
	if chErr := root.Chmod(parent, fi.Mode().Perm()|0o300); chErr != nil {
		return err
	}
	if rmErr := root.Remove(name); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}

// archiveName turns a tar entry name into a path relative to the rootfs root,
// rejecting the names that are trying to leave it.
//
// This is the cheap first gate, not the containment check. An entry naming an
// absolute path or ".." is malformed by any reading of the OCI spec and costs
// nothing to refuse before the kernel is involved -- but a name alone can
// never establish containment, because the dangerous case is a name that looks
// perfectly ordinary until you resolve it through a symlink the same archive
// planted a few entries earlier. What actually holds the extraction inside the
// rootfs is the os.Root in extractTarIgnoreChown.
func archiveName(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty entry name")
	}
	if path.IsAbs(name) {
		return "", errors.New("absolute path")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path leaves the rootfs")
	}
	return filepath.FromSlash(clean), nil
}

// Bounds on what one layer is allowed to expand into, so that a decompression
// bomb fails the pull instead of filling the disk (gosec G110: the io.Copy
// below has no other limit on it, and neither the tar size field nor the
// compressed layer size is anything but a claim by the image).
//
// The numbers are deliberately far above anything a real image reaches. The
// largest single layer this repo has had to unpack is the desktop image's
// 552MB one (see unpackLayerWithRetry); an ubuntu-based agent rootfs is a few
// GB across a few hundred thousand files. 16GiB and 4M entries leave better
// than an order of magnitude of headroom on both, while still bounding what a
// hostile layer can do to the host before anyone notices.
const (
	maxLayerBytes   int64 = 16 << 30
	maxLayerEntries       = 4_000_000
)

// The extractor reads the caps through these rather than the constants
// directly, only so that a test can shrink them to a size it can actually
// reach. Nothing at run time assigns to them.
var (
	layerByteCap  = maxLayerBytes
	layerEntryCap = maxLayerEntries
)

// extractTarIgnoreChown extracts a tar archive, ignoring chown errors (for macOS compatibility)
//
// Every path it touches is resolved through an os.Root opened on rootfsDir
// rather than joined onto rootfsDir by hand. Joining and then checking the
// result lexically proves nothing: filepath.Join folds ".." away before the
// check sees it, and no check on a name can see the symlinks. Two ordinary
// looking entries -- "escape" as a symlink to /somewhere, then "escape/pwned"
// as a regular file -- are enough to make os.Create, os.MkdirAll and
// os.RemoveAll write, truncate and delete host files as whoever ran hull,
// with the pull still reporting success. os.Root re-resolves each component
// with the root as its ceiling and refuses to cross it, which holds whether
// the link came from this layer or from the archive.Apply pass that ran
// before it.
//
// A refused entry fails the whole pull. Skipping it and carrying on would
// leave a rootfs that is missing exactly the files an attacker chose, and say
// nothing about it.
func extractTarIgnoreChown(r io.Reader, rootfsDir string) error {
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return fmt.Errorf("failed to open rootfs %s: %w", rootfsDir, err)
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(r)
	var (
		totalBytes int64
		entries    int
	)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		entries++
		if entries > layerEntryCap {
			return fmt.Errorf("layer has more than %d entries: refusing to extract %s",
				layerEntryCap, header.Name)
		}

		// The entry's path relative to the rootfs root. Every os.Root call
		// below takes this; nothing below joins a path onto rootfsDir in
		// order to operate on it.
		name, err := archiveName(header.Name)
		if err != nil {
			return fmt.Errorf("refusing tar entry %q: %w", header.Name, err)
		}
		if name == "." {
			// The archive's own root entry ("./"), which is the rootfs we
			// already have.
			continue
		}

		// What the entry is at, on the host. Only recordGuestAttr and
		// chmodPOSIX use it, because setting an xattr on a symlink has no
		// path-free form on macOS. It is safe to re-resolve because it is
		// only ever used after the matching os.Root call on the same name has
		// already succeeded, and that call resolving without escaping is the
		// kernel saying this name lands inside the rootfs.
		target := filepath.Join(rootfsDir, name)

		// OCI whiteouts mark deletions from lower layers and must be
		// applied, not materialized: a ".wh.<name>" entry deletes
		// <name>, and a ".wh..wh..opq" entry empties the directory it
		// appears in (the layer then re-populates it).
		if base := filepath.Base(name); strings.HasPrefix(base, ".wh.") {
			dir := filepath.Dir(name)
			if base == ".wh..wh..opq" {
				names, rerr := rootDirNames(root, dir)
				if rerr != nil {
					return fmt.Errorf("failed to apply opaque whiteout in %s: %w", dir, rerr)
				}
				for _, n := range names {
					if err := root.RemoveAll(filepath.Join(dir, n)); err != nil {
						return fmt.Errorf("failed to apply opaque whiteout in %s: %w", dir, err)
					}
				}
			} else {
				victim := filepath.Join(dir, strings.TrimPrefix(base, ".wh."))
				if err := root.RemoveAll(victim); err != nil {
					return fmt.Errorf("failed to apply whiteout for %s: %w", victim, err)
				}
			}
			continue
		}

		// Create parent directories. root.MkdirAll is happy for a component to
		// already be a symlink to a directory inside the rootfs -- images rely
		// on that, /lib -> /usr/lib being the obvious one -- and refuses only
		// when following it would leave, which is where a layer that planted
		// its own "escape" link stops. The entry is named in the error either
		// way: what comes back from the kernel there is a bare "file exists",
		// which says nothing about which entry asked or why it was refused.
		if parent := filepath.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0755); err != nil {
				return fmt.Errorf("failed to create parent directory %s for %s: %w",
					parent, header.Name, err)
			}
		}

		// What the entry is in the image, which the host filesystem cannot
		// hold: an unprivileged unpack cannot chown to root, so every path
		// below lands owned by whoever ran hull. Recorded per entry, right
		// where the tar header is still in hand.
		keepAttr := func(symlink bool) {
			if err := recordGuestAttr(target, uint32(header.Mode),
				uint32(header.Uid), uint32(header.Gid), symlink); err != nil {
				log.Debugf("could not record image ownership for %s: %v", target, err)
			}
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Create directory
			if err := root.MkdirAll(name, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", name, err)
			}
			keepAttr(false)

		case tar.TypeReg:
			// Drop any existing file first, as the symlink and hardlink cases
			// below already do. A later layer routinely replaces a file an
			// earlier one wrote read-only (/usr/lib/udev/hwdb.bin is one), and
			// macOS enforces the mode bits against the owner too, so opening
			// it for write fails with EACCES and loses the whole pull.
			if err := removeForReplace(root, name); err != nil {
				return fmt.Errorf("failed to replace file %s: %w", name, err)
			}
			file, err := root.Create(name)
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", name, err)
			}

			// Bounded rather than a straight io.Copy: see layerByteCap. The
			// reader is given one byte more than the budget so that going
			// over it is visible here rather than as a silent truncation.
			remaining := layerByteCap - totalBytes
			written, err := io.Copy(file, io.LimitReader(tr, remaining+1))
			totalBytes += written
			if err == nil && written > remaining {
				err = fmt.Errorf("layer expands past the %d byte limit", layerByteCap)
			}
			if err != nil {
				_ = file.Close()
				return fmt.Errorf("failed to write file %s: %w", name, err)
			}

			_ = file.Close()
			// Before the chmod, not after: setxattr needs write permission on
			// the file, and an image is full of files that do not have it --
			// /etc/sudoers is 0440, and sudo refuses to run when it cannot see
			// it as root-owned. Recording first means the mode the image asked
			// for, setuid bit included, is already stored by the time the file
			// stops being writable.
			keepAttr(false)

			// Set file permissions
			if err := chmodPOSIX(target, header.Mode); err != nil {
				log.Warnf("failed to chmod %s: %v", target, err)
			}

		case tar.TypeSymlink:
			// Create symlink.
			//
			// Linkname is deliberately not checked, and root.Symlink does not
			// check it either: images are full of absolute links -- /lib ->
			// /usr/lib, every /etc/alternatives entry -- and refusing those
			// would refuse most real images. Creating a link is not following
			// one. The containment is on the other side: any later entry that
			// tries to walk through this link is resolved by root, and stops
			// at the rootfs boundary whatever the link says.
			if err := root.RemoveAll(name); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove existing symlink %s: %w", name, err)
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return fmt.Errorf("failed to create symlink %s -> %s: %w", name, header.Linkname, err)
			}
			keepAttr(true)

		case tar.TypeLink:
			// Hard link to a file extracted earlier in this rootfs.
			// Fall back to copying the content when linking fails
			// (silently skipping would lose the file entirely).
			//
			// Linkname gets the same treatment as the entry's own name, and
			// root.Link resolves it inside the rootfs. Unlike a symlink, a
			// hard link is dereferenced the moment it is made: "/etc/shadow"
			// here would hand the guest the host's copy, with no symlink left
			// behind to notice.
			source, err := archiveName(header.Linkname)
			if err != nil {
				return fmt.Errorf("refusing hard link %s -> %q: %w", name, header.Linkname, err)
			}
			if err := root.RemoveAll(name); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove existing file %s: %w", name, err)
			}
			if err := root.Link(source, name); err != nil {
				data, rerr := root.ReadFile(source)
				if rerr != nil {
					if os.IsNotExist(rerr) {
						// A name the archive references but never carried.
						// That file is already lost; warning is all there is
						// to do about it.
						log.Warnf("failed to hard link %s -> %s: %v", name, source, err)
						continue
					}
					// Anything else -- a source root refused to resolve, most
					// of all -- is a refusal, and refusals fail the pull.
					return fmt.Errorf("failed to hard link %s -> %s: %w", name, source, rerr)
				}
				// Written permissively so the record below can be made, then
				// restricted -- same ordering, and for the same reason, as
				// the regular-file case above.
				if werr := root.WriteFile(name, data, 0o644); werr != nil {
					return fmt.Errorf("failed to copy hard link target %s: %w", name, werr)
				}
				keepAttr(false)
				if cerr := chmodPOSIX(target, header.Mode); cerr != nil {
					log.Warnf("failed to chmod %s: %v", target, cerr)
				}
				continue
			}
			// A hard link shares its inode with the entry it points at, and an
			// xattr belongs to the inode rather than to the name, so the
			// source's record already covers this name. Re-stating it would
			// only fail on the modes that made the ordering above necessary.

		default:
			// Other types (devices, fifos, etc.) are not supported
			log.Debugf("Skipping unsupported tar entry type %v: %s", header.Typeflag, header.Name)
		}
	}

	return nil
}

// rootDirNames lists a directory's entries through root, which os.ReadDir
// cannot do: it takes a path, and an opaque whiteout naming a directory the
// layer symlinked out of the rootfs would have it enumerate -- and then the
// caller delete -- whatever is on the host at the other end.
//
// A directory that is not there yet is not an error. A whiteout is free to
// name something no lower layer created, and there is then nothing to remove.
func rootDirNames(root *os.Root, dir string) ([]string, error) {
	d, err := root.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.Readdirnames(-1)
}

// chmodPOSIX applies a tar header's mode, setuid bit and all.
//
// os.Chmod does not: it takes an os.FileMode, which carries setuid, setgid and
// sticky as high bits (os.ModeSetuid is 1<<23) rather than at their POSIX
// values. Converting a header's raw 04755 with os.FileMode(...) therefore
// yields a mode whose 04000 sits in a bit position Go ignores, and the file
// quietly comes out 0755.
//
// Nothing noticed while hvi was the only backend that mattered -- it reads the
// mode from the record beside the file, not from the file. vz reports the host
// mode straight through, so there the missing bit is the difference between
// sudo working and "must be owned by uid 0 and have the setuid bit set".
func chmodPOSIX(target string, mode int64) error {
	// Permission bits plus setuid/setgid/sticky, which is everything a tar
	// header carries and all that chmod accepts.
	return syscall.Chmod(target, uint32(mode&0o7777))
}
