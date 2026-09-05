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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedVerifiedAssets writes a kernel and an initrd into a fresh store and
// records them, the way a successful Fetch would have. It returns the store
// directory and the asset directory beneath it.
func seedVerifiedAssets(t *testing.T) (storeDir, assetDir string) {
	t.Helper()
	storeDir = t.TempDir()
	// Dir() consults the environment first, and a developer running the suite
	// may well have HULL_BOOT_ASSETS pointing at a local build. Clear both so
	// the test describes the store it just made.
	t.Setenv("HULL_BOOT_ASSETS", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")

	var err error
	assetDir, err = Dir(storeDir)
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	files := map[string]File{}
	for name, content := range map[string]string{
		KernelName(): "a genuine kernel, as fetched",
		InitrdName:   "a genuine initrd, as fetched",
	} {
		var rec File
		if err := writeFile(filepath.Join(assetDir, name), strings.NewReader(content), bundleFileMode, &rec); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		files[name] = rec
	}
	if err := writeProvenance(assetDir, DefaultRepo+":test", "sha256:deadbeef", "sha256:deadbeef", files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	return storeDir, assetDir
}

// TestVerifyCachedAcceptsWhatWasFetched is the control: without it, a test that
// rejects everything would look like it was catching tampering.
func TestVerifyCachedAcceptsWhatWasFetched(t *testing.T) {
	storeDir, _ := seedVerifiedAssets(t)
	if err := VerifyCached(storeDir); err != nil {
		t.Fatalf("untouched assets should verify, got: %v", err)
	}
	if !Present(storeDir) {
		t.Fatal("seeded assets should be present")
	}
}

// TestVerifyCachedRefusesASwappedKernel is the finding this file exists for.
//
// The asset directory is user-writable and shared between hull and brig, so
// anything running as the user can drop a kernel into it. Before the provenance
// record, the reuse test was existence plus a non-zero size, which a swapped
// kernel passes trivially -- and the next sandbox booted it with the user's
// forwarded credentials, reporting nothing.
func TestVerifyCachedRefusesASwappedKernel(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)

	kernel := filepath.Join(assetDir, KernelName())
	if err := os.WriteFile(kernel, []byte("an attacker's kernel, same-ish size"), bundleFileMode); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}

	// The old test still passes, which is exactly why it was not enough.
	if !Present(storeDir) {
		t.Fatal("the swapped kernel is still present on disk; Present is not the check under test")
	}

	err := VerifyCached(storeDir)
	if err == nil {
		t.Fatal("a swapped kernel was accepted: the cache poisoning path is open")
	}
	if errors.Is(err, ErrNoProvenance) {
		t.Fatalf("a swapped kernel must not be reported as a missing record: %v", err)
	}
	for _, want := range []string{KernelName(), "does not match", "replaced it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q so the operator knows what to look at, got: %v", want, err)
		}
	}
}

// TestVerifyCachedRefusesASwappedInitrd covers the other half of the pair. The
// initrd carries the in-guest agent, so replacing it is as good as replacing
// the kernel for anything that runs inside the sandbox.
func TestVerifyCachedRefusesASwappedInitrd(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	if err := os.WriteFile(filepath.Join(assetDir, InitrdName), []byte("attacker initrd"), bundleFileMode); err != nil {
		t.Fatalf("swap initrd: %v", err)
	}
	if err := VerifyCached(storeDir); err == nil {
		t.Fatal("a swapped initrd was accepted")
	}
}

// TestVerifyCachedRefusesATruncatedAsset covers the case where the content is a
// prefix of the original, which a length check alone would also miss once the
// file is non-empty.
func TestVerifyCachedRefusesATruncatedAsset(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	if err := os.WriteFile(filepath.Join(assetDir, KernelName()), []byte("a"), bundleFileMode); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := VerifyCached(storeDir); err == nil {
		t.Fatal("a truncated kernel was accepted")
	}
}

// TestVerifyCachedReportsAMissingRecordDistinctly protects the distinction the
// caller depends on. A cache written by an older hull has no record and is a
// migration; a record that disagrees with the bytes is an attack. Collapsing
// them into one boolean would make the second look like the first, and Ensure
// would silently re-fetch over evidence.
func TestVerifyCachedReportsAMissingRecordDistinctly(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	if err := os.Remove(filepath.Join(assetDir, provenanceName)); err != nil {
		t.Fatalf("remove record: %v", err)
	}
	err := VerifyCached(storeDir)
	if !errors.Is(err, ErrNoProvenance) {
		t.Fatalf("a legacy cache should report ErrNoProvenance, got: %v", err)
	}
}

// TestVerifyCachedRefusesACorruptRecord: a record we cannot parse is not a
// migration. We cannot tell a truncated write from one rewritten by something
// that wanted the check skipped, so it does not get the benefit of the doubt.
func TestVerifyCachedRefusesACorruptRecord(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	if err := os.WriteFile(filepath.Join(assetDir, provenanceName), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}
	err := VerifyCached(storeDir)
	if err == nil {
		t.Fatal("a corrupt record was accepted")
	}
	if errors.Is(err, ErrNoProvenance) {
		t.Fatal("a corrupt record must not be treated as an absent one")
	}
}

// TestVerifyCachedRefusesARecordNamingAPathOutsideTheDirectory: the record is
// ours, but it still names files by string, and a name with a separator would
// read outside the asset directory.
func TestVerifyCachedRefusesARecordNamingAPathOutsideTheDirectory(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	files := map[string]File{"../../escape": {SHA256: "x", Size: 1}}
	if err := writeProvenance(assetDir, "ref", "sha256:x", "", files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	err := VerifyCached(storeDir)
	if err == nil || !strings.Contains(err.Error(), "not a file in") {
		t.Fatalf("a record naming a path outside the directory should be refused, got: %v", err)
	}
}

// TestCheckRefRefusesANonHTTPSRegistry. go-containerregistry picks the scheme
// itself and returns http for localhost, loopback and any RFC1918 address, so
// with HULL_BOOT_ASSETS_REF pointing at a LAN registry the kernel would arrive
// over cleartext with nothing authenticating it.
func TestCheckRefRefusesANonHTTPSRegistry(t *testing.T) {
	for _, ref := range []string{
		"192.168.1.50:5000/hull-assets:darwin-arm64",
		"10.0.0.9:5000/hull-assets:darwin-arm64",
		"localhost:5000/hull-assets:darwin-arm64",
		"127.0.0.1:5000/hull-assets:darwin-arm64",
	} {
		t.Setenv("HULL_BOOT_ASSETS_INSECURE", "")
		if _, err := checkRef(ref); err == nil {
			t.Errorf("%s: a kernel over cleartext should be refused", ref)
		} else if !strings.Contains(err.Error(), "https") {
			t.Errorf("%s: the refusal should say why, got: %v", ref, err)
		}
	}
}

// TestCheckRefAllowsANonHTTPSRegistryWhenTheOperatorSaysSo keeps the local
// development path open, but only when it is asked for explicitly. A registry
// on localhost is both cleartext and not the published repository, so it now
// takes both opt-ins -- which is the point: two separate things are being
// waived.
func TestCheckRefAllowsANonHTTPSRegistryWhenTheOperatorSaysSo(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS_INSECURE", "1")
	t.Setenv(ForeignRepoEnv, "1")
	if _, err := checkRef("localhost:5000/hull-assets:darwin-arm64"); err != nil {
		t.Fatalf("an explicit opt-in should be honoured, got: %v", err)
	}
}

// TestCheckRefIsSilentForThePublishedRepo. A version pin is an ordinary thing
// to do and must not be noisy.
func TestCheckRefIsSilentForThePublishedRepo(t *testing.T) {
	t.Setenv(ForeignRepoEnv, "")
	warn, err := checkRef(DefaultRepo + ":v0.1.0-darwin-arm64")
	if err != nil {
		t.Fatalf("the published repo should be accepted: %v", err)
	}
	if warn != "" {
		t.Errorf("a version pin on the published repo should be silent, got: %q", warn)
	}
}

// TestCheckRefRefusesAForeignRepositoryUntilItIsOptedInto is the finding.
//
// A bundle from a repository the project does not publish used to produce a
// log.Warnf and fetch anyway -- and at hull's default log level that single
// line is the entire defence for the kernel every sandbox on the host boots.
// A mirror or a staging registry is a decision somebody makes deliberately and
// once, so it is stated in the environment instead.
func TestCheckRefRefusesAForeignRepositoryUntilItIsOptedInto(t *testing.T) {
	const foreign = "ghcr.io/somebody-else/hull-assets:darwin-arm64"

	t.Setenv(ForeignRepoEnv, "")
	if _, err := checkRef(foreign); err == nil {
		t.Fatal("a kernel from another repository was fetched on a warning alone")
	} else if !strings.Contains(err.Error(), ForeignRepoEnv) {
		t.Errorf("the refusal should name the way to allow it, got: %v", err)
	}

	t.Setenv(ForeignRepoEnv, "1")
	warn, err := checkRef(foreign)
	if err != nil {
		t.Fatalf("an explicit opt-in should be honoured, got: %v", err)
	}
	if warn == "" {
		t.Error("an allowed third-party bundle should still say where the kernel is coming from")
	}

	// Naming the repository allows that one and nothing else, so an operator
	// who mirrors one registry does not thereby accept every registry.
	t.Setenv(ForeignRepoEnv, "ghcr.io/somebody-else/hull-assets")
	if _, err := checkRef(foreign); err != nil {
		t.Fatalf("naming the repository should allow it, got: %v", err)
	}
	if _, err := checkRef("ghcr.io/third-party/hull-assets:darwin-arm64"); err == nil {
		t.Error("allowing one repository allowed a different one as well")
	}
}

// TestSameRepoDoesNotStripAPrefixThatIsNotTheRegistry is the normalisation
// half of the same finding.
//
// sameRepo trimmed "index.docker.io/" wherever it appeared in the string, so
// index.docker.io/ghcr.io/nofireai/hull-assets -- a repository on Docker Hub,
// which anybody can create -- normalised to the published repository and was
// accepted with no warning at all. The check meant to catch a foreign registry
// was the thing that hid it.
func TestSameRepoDoesNotStripAPrefixThatIsNotTheRegistry(t *testing.T) {
	if sameRepo("index.docker.io/"+DefaultRepo, DefaultRepo) {
		t.Fatalf("index.docker.io/%s was treated as %s", DefaultRepo, DefaultRepo)
	}
	t.Setenv(ForeignRepoEnv, "")
	if _, err := checkRef("index.docker.io/" + DefaultRepo + ":darwin-arm64"); err == nil {
		t.Fatal("a Docker Hub repository spelled to look like ours was accepted")
	}

	// The case the trim was there for still works: a host written with and
	// without the registry prefix Docker Hub implies is the same host.
	if !sameRepo("docker.io/library/busybox", "index.docker.io/library/busybox") {
		t.Error("docker.io and index.docker.io should still be the same registry")
	}
	// A registry host is case-insensitive, so the spelling of the host must not
	// decide whether the bundle is ours. The repository path is not, and
	// go-containerregistry rejects it outright, which checkRef reports as an
	// unparseable reference rather than as a foreign repository.
	if !sameRepo("GHCR.IO/nofireai/hull-assets", DefaultRepo) {
		t.Error("the registry host should compare case-insensitively")
	}
}

// TestEnsureRefusesToHandBackASwappedKernel is the end-to-end version, and the
// one that matters: Ensure is what run.go asks for the kernel to boot.
//
// It reaches no network here -- the assets are on disk, which is the branch
// that used to return them unconditionally. Before the provenance record this
// call returned the attacker's kernel path and a nil error, and the sandbox
// booted it with the user's forwarded credentials.
func TestEnsureRefusesToHandBackASwappedKernel(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	if err := os.WriteFile(filepath.Join(assetDir, KernelName()), []byte("attacker kernel"), bundleFileMode); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}

	kernel, initrd, err := Ensure(t.Context(), storeDir)
	if err == nil {
		t.Fatalf("Ensure handed back a swapped kernel instead of refusing: %s, %s", kernel, initrd)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Ensure should refuse with the mismatch, got: %v", err)
	}
	if kernel != "" || initrd != "" {
		t.Errorf("a refusal must not also return usable paths, got %q and %q", kernel, initrd)
	}
}

// TestProvenanceRoundTrips checks the record survives a write and read, since
// everything above depends on it.
func TestProvenanceRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := map[string]File{"Image": {SHA256: "abc", Size: 3}}
	if err := writeProvenance(dir, "ref:tag", "sha256:1234", "sha256:1234", want); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	got, err := readProvenance(dir)
	if err != nil {
		t.Fatalf("readProvenance: %v", err)
	}
	if got.Ref != "ref:tag" || got.Digest != "sha256:1234" || got.Files["Image"].SHA256 != "abc" {
		t.Fatalf("record did not round-trip: %+v", got)
	}
}

// TestVerifyCachedRefusesARecordThatDoesNotCoverTheKernel is the bypass found
// in review, and it was cheaper than any of the ones above.
//
// The verification walked the entries the record happened to contain, so
// deleting the kernel's entry left a record that was still valid JSON, still
// described real files, and vouched for nothing that boots. An attacker with
// write access to the asset directory did not have to forge a sha256 -- they
// had to delete a line. The only guard was that the record was not entirely
// empty.
func TestVerifyCachedRefusesARecordThatDoesNotCoverTheKernel(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)

	p, err := readProvenance(assetDir)
	if err != nil {
		t.Fatalf("readProvenance: %v", err)
	}
	delete(p.Files, KernelName())
	if err := writeProvenance(assetDir, p.Ref, p.Digest, p.VerifiedDigest, p.Files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	// Any kernel at all, since nothing now claims to describe it.
	if err := os.WriteFile(filepath.Join(assetDir, KernelName()), []byte("an arbitrary kernel"), bundleFileMode); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}

	err = VerifyCached(storeDir)
	if err == nil {
		t.Fatal("an arbitrary kernel verified clean because the record did not mention it")
	}
	if errors.Is(err, ErrNoProvenance) {
		t.Fatalf("a record with the kernel dropped is not a missing record: %v", err)
	}
	for _, want := range []string{"does not cover", KernelName()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q, got: %v", want, err)
		}
	}
}

// TestVerifyCachedRefusesARecordThatDoesNotCoverTheInitrd covers the other
// half. The initrd carries the in-guest agent, so dropping its entry is worth
// as much to an attacker as dropping the kernel's.
func TestVerifyCachedRefusesARecordThatDoesNotCoverTheInitrd(t *testing.T) {
	storeDir, assetDir := seedVerifiedAssets(t)
	p, err := readProvenance(assetDir)
	if err != nil {
		t.Fatalf("readProvenance: %v", err)
	}
	delete(p.Files, InitrdName)
	if err := writeProvenance(assetDir, p.Ref, p.Digest, p.VerifiedDigest, p.Files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, InitrdName), []byte("an arbitrary initrd"), bundleFileMode); err != nil {
		t.Fatalf("swap initrd: %v", err)
	}
	if err := VerifyCached(storeDir); err == nil {
		t.Fatal("an arbitrary initrd verified clean because the record did not mention it")
	}
}

// TestVerifyFilesRefusesATamperedKernelGivenByPath is defect 1 at this layer.
//
// This is the call shape the boot path actually uses now: two host paths, with
// nothing said about who resolved them. brig resolves them itself -- a stat
// and a non-zero size -- and passes them to hull in the bootKernel/bootInitrd
// annotations, so any check that hangs off hull's own resolver never sees
// them. Keyed on the file, the same tampering is caught either way.
func TestVerifyFilesRefusesATamperedKernelGivenByPath(t *testing.T) {
	_, assetDir := seedVerifiedAssets(t)
	kernel := filepath.Join(assetDir, KernelName())
	initrd := filepath.Join(assetDir, InitrdName)

	if err := VerifyFiles(kernel, initrd); err != nil {
		t.Fatalf("the untouched pair should verify by path, got: %v", err)
	}
	if err := os.WriteFile(kernel, []byte("an attacker's kernel"), bundleFileMode); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}
	err := VerifyFiles(kernel, initrd)
	if err == nil {
		t.Fatal("a tampered kernel passed when the path was supplied directly")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("the refusal should be the content mismatch, got: %v", err)
	}
}

// TestVerifyFilesReportsNoProvenanceForAKernelNobodyFetched keeps the
// bring-your-own-kernel path open. HULL_BOOT_ASSETS pointing at a local build
// is a supported way to work, and there is nothing to compare it against, so
// the caller is told rather than stopped.
func TestVerifyFilesReportsNoProvenanceForAKernelNobodyFetched(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, KernelName())
	writeTestFile(t, kernel, "a kernel somebody built this morning")
	if err := VerifyFiles(kernel); !errors.Is(err, ErrNoProvenance) {
		t.Fatalf("a kernel with no record should report ErrNoProvenance, got: %v", err)
	}
}

// TestVerifyFilesRefusesAFileTheRecordBesideItOmits. A directory that has a
// record is a directory something fetched into, so a file booted out of it and
// absent from the record is the dropped-entry bypass wearing a different hat.
func TestVerifyFilesRefusesAFileTheRecordBesideItOmits(t *testing.T) {
	_, assetDir := seedVerifiedAssets(t)
	planted := filepath.Join(assetDir, "vmlinuz-somebody-elses")
	writeTestFile(t, planted, "planted kernel")
	if err := VerifyFiles(planted); err == nil {
		t.Fatal("a file the record does not mention was booted out of a fetched directory")
	}
}

// TestProvenanceRecordsTheVerifiedDigest: a cached boot has to be able to say
// which digest a signature was checked against, not merely which digest was
// downloaded. The two are different claims and the record keeps them apart.
func TestProvenanceRecordsTheVerifiedDigest(t *testing.T) {
	dir := t.TempDir()
	files := map[string]File{"Image": {SHA256: "abc", Size: 3}}
	if err := writeProvenance(dir, "ref:tag", "sha256:1234", "sha256:1234", files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	got, err := readProvenance(dir)
	if err != nil {
		t.Fatalf("readProvenance: %v", err)
	}
	if got.VerifiedDigest != "sha256:1234" {
		t.Fatalf("VerifiedDigest = %q, want the digest cosign checked", got.VerifiedDigest)
	}

	// An unverified fetch must not leave anything that reads as a verified one.
	if err := writeProvenance(dir, "ref:tag", "sha256:1234", "", files); err != nil {
		t.Fatalf("writeProvenance: %v", err)
	}
	got, err = readProvenance(dir)
	if err != nil {
		t.Fatalf("readProvenance: %v", err)
	}
	if got.VerifiedDigest != "" {
		t.Fatalf("VerifiedDigest = %q, want it empty when nothing was verified", got.VerifiedDigest)
	}
}
