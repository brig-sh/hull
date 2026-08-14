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
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		log.Debugf("archive.Apply hit a permission error, completing with the manual extractor")

		rc2, rerr := layer.Uncompressed()
		if rerr != nil {
			return fmt.Errorf("failed to reopen layer: %w", rerr)
		}
		defer func() { _ = rc2.Close() }()

		if err := extractTarIgnoreChown(&progressReader{r: rc2, p: p}, rootfsDir); err != nil {
			return fmt.Errorf("failed to extract layer with fallback: %w", err)
		}

		log.Debugf("Layer %d unpacked successfully (fallback method)", index)
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to apply layer: %w", err)
	}

	log.Debugf("Layer %d unpacked successfully", index)
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

		switch header.Typeflag {
		case tar.TypeDir:
			// Create directory
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", target, err)
			}

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

			// Set file permissions
			if err := os.Chmod(target, os.FileMode(header.Mode)); err != nil {
				log.Warnf("failed to chmod %s: %v", target, err)
			}
			_ = file.Close()

		case tar.TypeSymlink:
			// Create symlink
			if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove existing symlink %s: %w", target, err)
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("failed to create symlink %s -> %s: %w", target, header.Linkname, err)
			}

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
				if werr := os.WriteFile(target, data, os.FileMode(header.Mode)); werr != nil {
					return fmt.Errorf("failed to copy hard link target %s: %w", target, werr)
				}
			}

		default:
			// Other types (devices, fifos, etc.) are not supported
			log.Debugf("Skipping unsupported tar entry type %v: %s", header.Typeflag, header.Name)
		}
	}

	return nil
}
