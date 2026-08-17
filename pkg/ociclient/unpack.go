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
func removeForReplace(target string) error {
	err := os.Remove(target)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	parent := filepath.Dir(target)
	fi, statErr := os.Stat(parent)
	if statErr != nil {
		return err
	}
	if chErr := os.Chmod(parent, fi.Mode().Perm()|0o300); chErr != nil {
		return err
	}
	if rmErr := os.Remove(target); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}

// extractTarIgnoreChown extracts a tar archive, ignoring chown errors (for macOS compatibility)
func extractTarIgnoreChown(r io.Reader, rootfsDir string) error {
	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Construct the full path
		target := filepath.Join(rootfsDir, header.Name)

		// Ensure the target path is within rootfsDir (security check)
		absTarget, err := filepath.Abs(target)
		absRoot, err2 := filepath.Abs(rootfsDir)
		rel, err3 := "", error(nil)
		if err == nil && err2 == nil {
			rel, err3 = filepath.Rel(absRoot, absTarget)
		}
		if err != nil || err2 != nil || err3 != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			log.Warnf("Skipping file outside rootfs: %s", header.Name)
			continue
		}

		// OCI whiteouts mark deletions from lower layers and must be
		// applied, not materialized: a ".wh.<name>" entry deletes
		// <name>, and a ".wh..wh..opq" entry empties the directory it
		// appears in (the layer then re-populates it).
		if base := filepath.Base(header.Name); strings.HasPrefix(base, ".wh.") {
			dir := filepath.Dir(target)
			if base == ".wh..wh..opq" {
				entries, rerr := os.ReadDir(dir)
				if rerr != nil && !os.IsNotExist(rerr) {
					return fmt.Errorf("failed to apply opaque whiteout in %s: %w", dir, rerr)
				}
				for _, e := range entries {
					if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
						return fmt.Errorf("failed to apply opaque whiteout in %s: %w", dir, err)
					}
				}
			} else {
				victim := filepath.Join(dir, strings.TrimPrefix(base, ".wh."))
				if err := os.RemoveAll(victim); err != nil {
					return fmt.Errorf("failed to apply whiteout for %s: %w", victim, err)
				}
			}
			continue
		}

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
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
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", target, err)
			}
			keepAttr(false)

		case tar.TypeReg:
			// Drop any existing file first, as the symlink and hardlink cases
			// below already do. A later layer routinely replaces a file an
			// earlier one wrote read-only (/usr/lib/udev/hwdb.bin is one), and
			// macOS enforces the mode bits against the owner too, so opening
			// it for write fails with EACCES and loses the whole pull.
			if err := removeForReplace(target); err != nil {
				return fmt.Errorf("failed to replace file %s: %w", target, err)
			}
			file, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", target, err)
			}

			if _, err := io.Copy(file, tr); err != nil {
				_ = file.Close()
				return fmt.Errorf("failed to write file %s: %w", target, err)
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
			// Create symlink
			if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove existing symlink %s: %w", target, err)
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("failed to create symlink %s -> %s: %w", target, header.Linkname, err)
			}
			keepAttr(true)

		case tar.TypeLink:
			// Hard link to a file extracted earlier in this rootfs.
			// Fall back to copying the content when linking fails
			// (silently skipping would lose the file entirely).
			source := filepath.Join(rootfsDir, header.Linkname)
			_ = os.RemoveAll(target)
			if err := os.Link(source, target); err != nil {
				data, rerr := os.ReadFile(source)
				if rerr != nil {
					log.Warnf("failed to hard link %s -> %s: %v", target, source, err)
					continue
				}
				// Written permissively so the record below can be made, then
				// restricted -- same ordering, and for the same reason, as
				// the regular-file case above.
				if werr := os.WriteFile(target, data, 0o644); werr != nil {
					return fmt.Errorf("failed to copy hard link target %s: %w", target, werr)
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
func chmodPOSIX(path string, mode int64) error {
	// Permission bits plus setuid/setgid/sticky, which is everything a tar
	// header carries and all that chmod accepts.
	return syscall.Chmod(path, uint32(mode&0o7777))
}
