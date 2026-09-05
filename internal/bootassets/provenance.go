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
	"sort"
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
// What this record IS
//
// It is a statement of what a fetch put on disk: the reference, the manifest
// digest, the digest cosign verified (see verify.go), and the sha256 of every
// file written. Re-hashing before a boot catches the file that changed after
// the fetch -- a half-finished download, a stray `cp` into the shared asset
// directory, a second runtime writing a different bundle over this one -- and,
// because the record carries the verified digest, ties the bytes being booted
// back to a signature that was checked at fetch time.
//
// What this record is NOT
//
// It is not tamper-proofing, and an earlier version of this comment implied
// that it was. The record lives in the same user-writable directory as the
// files it describes and is not itself signed, so anything that can rewrite
// the kernel can rewrite provenance.json in the same breath and produce a
// cache that verifies perfectly. Nothing here changes that, and pretending
// otherwise is worse than not checking at all.
//
// What it buys is that the attack has to be complete. A partial write, a
// swapped file, or a record edited to simply drop the kernel's entry -- which
// is cheaper than forging a hash and was the bypass found in review -- are all
// refused, and the verified digest gives a later boot something to point at.
// The real anchor is the signature checked at fetch time; this is what carries
// that check forward to the boots that reuse the download.

// provenanceName is the record written beside the assets it describes.
const provenanceName = "provenance.json"

// Provenance is what a fetch writes down about what it fetched.
//
// Digest is the bundle's manifest digest, which is what a signature would be
// made over, so recording it now means a later verification step has the
// subject it needs without re-resolving a tag that may have moved.
// VerifiedDigest is the digest a signature was actually checked against at
// fetch time, and is empty when nothing was checked -- no cosign on the host,
// an unsigned bundle, verification turned off. Keeping it separate from Digest
// is the point: "this is what we downloaded" and "this is what we proved" are
// different claims, and a cached boot can say which one it is standing on.
type Provenance struct {
	Ref            string          `json:"ref"`
	Digest         string          `json:"digest"`
	VerifiedDigest string          `json:"verifiedDigest,omitempty"`
	Files          map[string]File `json:"files"`
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
func writeProvenance(dir, ref, digest, verifiedDigest string, files map[string]File) error {
	p := Provenance{Ref: ref, Digest: digest, VerifiedDigest: verifiedDigest, Files: files}
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

// Record returns the provenance record for a store's assets, or
// ErrNoProvenance when there is not one.
//
// It exists so that `hull assets show` can say whether the kernel on this host
// was signature-checked when it arrived. Without somewhere to read it, the
// difference between a verified bundle and one taken on trust is a field in a
// JSON file, and an operator asking "is this kernel the published one?" has no
// way to find out short of opening it.
func Record(storeDir string) (Provenance, error) {
	dir, err := Dir(storeDir)
	if err != nil {
		return Provenance{}, err
	}
	return readProvenance(dir)
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

// VerifyCached re-hashes the assets in a store's own asset directory and
// compares them with the record written when they were fetched.
//
// It returns ErrNoProvenance when there is no record to compare against, which
// the caller is expected to treat as "fetch again to establish one" rather than
// as tampering. Any other error means the bytes on disk are not the bytes that
// were fetched, and the caller must not boot them.
func VerifyCached(storeDir string) error {
	kernel, initrd, err := Paths(storeDir)
	if err != nil {
		return err
	}
	return VerifyFiles(kernel, initrd)
}

// VerifyStagedCopy checks a copy against the record that describes the
// original.
//
// VerifyFiles answers "are the bytes at this path the recorded ones", which is
// the wrong question when the path is one somebody else can change between the
// answer and the use. hull now copies a host boot asset into the instance
// directory and boots the copy; this hashes the COPY and compares it against
// the entry recorded for the original, so what was verified and what is
// executed are the same bytes.
func VerifyStagedCopy(original, staged string) error {
	dir, base := filepath.Dir(original), filepath.Base(original)
	rec, err := readProvenance(dir)
	if err != nil {
		return err
	}
	want, ok := rec.Files[base]
	if !ok {
		return fmt.Errorf("the provenance record beside %s does not describe %s, so there "+
			"is nothing to check the staged copy against", dir, base)
	}
	got, size, err := hashFile(staged)
	if err != nil {
		return err
	}
	if got != want.SHA256 || size != want.Size {
		return fmt.Errorf("refusing to boot %s: it hashes %s (%d bytes), but the record for "+
			"%s says %s (%d bytes). The file changed after it was fetched",
			original, got, size, base, want.SHA256, want.Size)
	}
	return nil
}

// VerifyFiles verifies the files that are about to be booted, whoever resolved
// them, against the provenance record sitting beside them.
//
// The subject is the file, not the code path that produced its name, and that
// is the whole design. The previous check hung off Ensure, which hull calls
// only when it resolves the assets itself -- and the caller that matters does
// not do that. brig locates the kernel with its own stat-and-non-zero-size
// check and hands hull the paths in the bootKernel/bootInitrd annotations, so
// for six of its eight shipped profiles the verification added to Ensure never
// ran at all: a fix that was live in the tree and unreachable in production.
//
// Keying on the file removes the way that happened. A caller cannot arrive at
// a boot without a kernel path, and a kernel path is all this needs -- so any
// new resolver, in hull or in another runtime driving it, is covered the day
// it is written rather than the day somebody remembers to add a call.
//
// A file with no record beside it reports ErrNoProvenance. That case is real
// and legitimate -- HULL_BOOT_ASSETS pointing at somebody's local kernel build
// -- so it is the caller's decision, not a refusal here. Everything else is
// fatal: the caller must not boot it.
func VerifyFiles(paths ...string) error {
	if len(paths) == 0 {
		return errors.New("no boot assets given to verify")
	}
	// Group by directory: the record describes a directory, and the kernel and
	// the initrd normally share one, so this reads and parses it once.
	byDir := make(map[string][]string, 1)
	dirs := make([]string, 0, 1)
	for _, p := range paths {
		if p == "" {
			return errors.New("cannot verify an empty boot asset path")
		}
		dir, base := filepath.Dir(p), filepath.Base(p)
		if _, seen := byDir[dir]; !seen {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], base)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		p, err := readProvenance(dir)
		if err != nil {
			return err
		}
		if err := verifyAgainstRecord(dir, p, byDir[dir]); err != nil {
			return err
		}
	}
	return nil
}

// verifyAgainstRecord checks a record against the directory it describes, and
// then checks that it describes the files we came here to boot.
//
// Both halves are needed. Hashing only the entries the record happens to
// contain is what let a review delete the kernel's entry and watch an
// arbitrary kernel verify clean -- cheaper than forging a hash, and it left
// nothing in the log. Requiring coverage means the record has to account for
// the files that boot, so dropping an entry is as loud as changing one.
func verifyAgainstRecord(dir string, p Provenance, required []string) error {
	// Sorted so that a record with several bad entries always reports the same
	// one, rather than whichever the map iteration reached first.
	names := make([]string, 0, len(p.Files))
	for name := range p.Files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		// The record is ours, but it still names files by string, and a name
		// with a separator in it would read outside the asset directory.
		if name != filepath.Base(name) || name == "." || name == ".." {
			return fmt.Errorf("boot asset provenance names %q, which is not a file in %s", name, dir)
		}
		want := p.Files[name]
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

	for _, name := range required {
		if _, ok := p.Files[name]; !ok {
			return fmt.Errorf("the boot asset provenance in %s does not cover %s, so nothing "+
				"vouches for the file about to be booted -- a record that omits an entry is "+
				"not a record that passed. Re-fetch with `hull assets pull --force` after "+
				"working out what wrote to %s", dir, name, dir)
		}
	}
	return nil
}

// writeFileBytes is writeFile for a value already in memory.
func writeFileBytes(path string, b []byte, perm os.FileMode) error {
	return writeFile(path, strings.NewReader(string(b)), perm, nil)
}

// ForeignRepoEnv opts in to fetching a kernel from a repository other than the
// published one. Set it to 1, or to the repository being allowed.
const ForeignRepoEnv = "HULL_BOOT_ASSETS_ALLOW_FOREIGN"

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
// somewhere else entirely is refused unless it is opted into, and it used to be
// a log line instead. A warning was the wrong instrument: hull's default log
// level puts it one line above debug output nobody reads, and the thing being
// warned about is the kernel every sandbox on this host is about to run. A
// legitimate mirror or staging registry is a decision somebody makes once, so
// making them state it costs an environment variable and removes the case where
// HULL_BOOT_ASSETS_REF was set by something other than the person at the
// keyboard.
//
// The signature check in verify.go does not cover this on its own: an
// unrecognised repository is precisely the case where there is no signature of
// ours to check.
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
	repo := parsed.Context().Name()
	if sameRepo(repo, DefaultRepo) {
		return "", nil
	}
	if allow := strings.TrimSpace(os.Getenv(ForeignRepoEnv)); allow == "1" || (allow != "" && sameRepo(allow, repo)) {
		return fmt.Sprintf("boot assets are being fetched from %s rather than %s, because %s "+
			"allows it; the kernel and initrd your sandboxes boot will come from there, and no "+
			"signature of ours covers them", repo, DefaultRepo, ForeignRepoEnv), nil
	}
	return "", fmt.Errorf("refusing to fetch a kernel from %s: hull publishes its boot assets at "+
		"%s, and a bundle from anywhere else carries no signature we can check. Set %s=1 (or to "+
		"%s) if this is a mirror or staging registry you control and you meant it",
		repo, DefaultRepo, ForeignRepoEnv, repo)
}

// sameRepo compares two repository names, allowing for the registry host being
// spelled in any of the ways that mean the same host.
//
// This parses rather than trimming a prefix off the string. The trim it
// replaces stripped "index.docker.io/" wherever it appeared, so
// index.docker.io/ghcr.io/nofireai/hull-assets normalised to the published
// repository and passed the check -- a completely different registry,
// suppressed by the very code meant to catch it. go-containerregistry does the
// normalisation properly, including docker.io to index.docker.io, so a
// reference that genuinely is the published repo still matches.
func sameRepo(a, b string) bool {
	ra, err := name.NewRepository(a)
	if err != nil {
		return false
	}
	rb, err := name.NewRepository(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ra.RegistryStr(), rb.RegistryStr()) &&
		strings.EqualFold(ra.RepositoryStr(), rb.RepositoryStr())
}
