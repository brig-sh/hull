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

package bootassets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// This file exists because of what the asset directory is: the kernel and the
// initrd that every sandbox boots on, sitting in a directory the user can
// write, reused across runs, and -- by design, so that a host which has run
// either runtime has already seeded the other -- shared between hull and brig.
//
// Before this, a cached pair was accepted on three tests: the file exists, it
// is not a directory, and its size is greater than zero. Nothing bound those
// bytes to the bundle they were fetched from, so anything able to write the
// directory could replace the kernel and the next boot would take it without
// a word. The image the guest runs is signature-checked; the kernel it runs on
// was not checked at all. That is the wrong way round -- a kernel is strictly
// more privileged than the filesystem above it.
//
// The record below closes the reuse half of that: what we fetched is written
// down, and what we reuse is hashed and compared against it. It does not, on
// its own, establish that the bundle was genuine when it arrived; signing the
// bundle at publish time is the other half, and is tracked separately.

// provenanceName is the record written beside the assets it describes.
const provenanceName = "provenance.json"

// Provenance is what a fetch writes down about what it fetched.
//
// Digest is the bundle's manifest digest, which is what a signature would be
// made over, so recording it now means a later verification step has the
// subject it needs without re-resolving a tag that may have moved.
type Provenance struct {
	Ref    string          `json:"ref"`
	Digest string          `json:"digest"`
	Files  map[string]File `json:"files"`
}

// File is one fetched file, by content.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ErrNoProvenance means the assets are there but predate this record, so we
// have nothing to compare them against.
//
// It is deliberately distinguishable from a mismatch. An absent record is a
// cache written by an older hull and is a migration, not an attack; a record
// that disagrees with the bytes beside it is the case worth stopping for. A
// single boolean would have collapsed the two, and "could not check" is not
// the same as "failed" -- the same distinction the image path draws.
var ErrNoProvenance = errors.New("no provenance record for the cached boot assets")

// writeProvenance records what a fetch produced.
//
// It is written last, after every file has landed, so that a fetch interrupted
// half way leaves no record rather than a record describing files that are not
// all there yet. A missing record fails safe: the next Ensure re-fetches.
func writeProvenance(dir, ref, digest string, files map[string]File) error {
	p := Provenance{Ref: ref, Digest: digest, Files: files}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode boot asset provenance: %w", err)
	}
	b = append(b, '\n')
	return writeFileBytes(filepath.Join(dir, provenanceName), b, 0o644)
}

// readProvenance loads the record, or reports that there is not one.
func readProvenance(dir string) (Provenance, error) {
	var p Provenance
	b, err := os.ReadFile(filepath.Join(dir, provenanceName))
	if errors.Is(err, os.ErrNotExist) {
		return p, ErrNoProvenance
	}
	if err != nil {
		return p, fmt.Errorf("read boot asset provenance: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		// A corrupt record is not a migration. We cannot tell whether it was
		// truncated by a full disk or rewritten by something that wanted the
		// check skipped, so it does not get the benefit of the doubt.
		return p, fmt.Errorf("boot asset provenance in %s is unreadable, so the cached "+
			"kernel cannot be trusted: %w", dir, err)
	}
	if len(p.Files) == 0 {
		return p, fmt.Errorf("boot asset provenance in %s records no files", dir)
	}
	return p, nil
}

// hashFile returns the sha256 and size of a file on disk.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyCached re-hashes the assets on disk and compares them with the record
// written when they were fetched.
//
// It returns ErrNoProvenance when there is no record to compare against, which
// the caller is expected to treat as "fetch again to establish one" rather than
// as tampering. Any other error means the bytes on disk are not the bytes that
// were fetched, and the caller must not boot them.
func VerifyCached(storeDir string) error {
	dir, err := Dir(storeDir)
	if err != nil {
		return err
	}
	p, err := readProvenance(dir)
	if err != nil {
		return err
	}
	for name, want := range p.Files {
		// The record is ours, but it still names files by string, and a name
		// with a separator in it would read outside the asset directory.
		if name != filepath.Base(name) || name == "." || name == ".." {
			return fmt.Errorf("boot asset provenance names %q, which is not a file in %s", name, dir)
		}
		path := filepath.Join(dir, name)
		got, size, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("verify cached boot asset %s: %w", path, err)
		}
		if got != want.SHA256 || size != want.Size {
			return fmt.Errorf("cached boot asset %s does not match what was fetched from %s "+
				"(recorded sha256:%s, %d bytes; found sha256:%s, %d bytes). Something has "+
				"replaced it since. Re-fetch with `hull assets pull --force` after working out "+
				"what wrote to %s",
				path, p.Ref, want.SHA256, want.Size, got, size, dir)
		}
	}
	return nil
}

// writeFileBytes is writeFile for a value already in memory.
func writeFileBytes(path string, b []byte, perm os.FileMode) error {
	return writeFile(path, strings.NewReader(string(b)), perm, nil)
}

// checkRef decides whether a bundle reference is one we are willing to fetch a
// kernel from.
//
// Two things are being guarded. The first is transport: go-containerregistry
// picks the scheme itself, and returns http -- not https -- for localhost, for
// loopback, and for any RFC1918 address. Since HULL_BOOT_ASSETS_REF can point
// anywhere, the default configuration is one environment variable away from
// fetching a kernel over cleartext from a host on the local network. That is
// refused unless the caller says plainly that it meant to.
//
// The second is provenance by repository. A ref that is the published bundle
// under a different tag is an ordinary version pin and passes silently. A ref
// somewhere else entirely may be a legitimate mirror or staging registry, so it
// is not refused -- but it is not silent either, because nothing downstream
// will tell the user that the kernel their agent is about to run came from a
// registry the project does not publish.
func checkRef(ref string) (warning string, err error) {
	parsed, perr := name.ParseReference(ref)
	if perr != nil {
		return "", fmt.Errorf("boot asset reference %q is not a valid image reference: %w", ref, perr)
	}
	reg := parsed.Context().Registry
	if reg.Scheme() != "https" && os.Getenv("HULL_BOOT_ASSETS_INSECURE") != "1" {
		return "", fmt.Errorf("refusing to fetch a kernel from %s over %s: the registry resolves "+
			"to a scheme that is not https, so the bundle would arrive unauthenticated and "+
			"unencrypted. Set HULL_BOOT_ASSETS_INSECURE=1 if this is a local registry you "+
			"control and you meant it", ref, reg.Scheme())
	}
	if repo := parsed.Context().Name(); !sameRepo(repo, DefaultRepo) {
		return fmt.Sprintf("boot assets are being fetched from %s rather than %s; "+
			"the kernel and initrd your sandboxes boot will come from there", repo, DefaultRepo), nil
	}
	return "", nil
}

// sameRepo compares two repository names, allowing for the registry host being
// spelled with or without an explicit index.docker.io style prefix.
func sameRepo(a, b string) bool {
	norm := func(s string) string { return strings.TrimPrefix(strings.ToLower(s), "index.docker.io/") }
	return norm(a) == norm(b)
}
