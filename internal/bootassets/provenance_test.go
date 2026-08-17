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
	if err := writeProvenance(assetDir, DefaultRepo+":test", "sha256:deadbeef", files); err != nil {
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
	if err := writeProvenance(assetDir, "ref", "sha256:x", files); err != nil {
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
// development path open, but only when it is asked for explicitly.
func TestCheckRefAllowsANonHTTPSRegistryWhenTheOperatorSaysSo(t *testing.T) {
	t.Setenv("HULL_BOOT_ASSETS_INSECURE", "1")
	if _, err := checkRef("localhost:5000/hull-assets:darwin-arm64"); err != nil {
		t.Fatalf("an explicit opt-in should be honoured, got: %v", err)
	}
}

// TestCheckRefIsSilentForThePublishedRepoAndWarnsForAnother. A version pin is
// an ordinary thing to do and must not be noisy; a different repository may be
// a legitimate mirror, so it is not refused -- but nothing else downstream
// would tell the user where their kernel came from.
func TestCheckRefIsSilentForThePublishedRepoAndWarnsForAnother(t *testing.T) {
	warn, err := checkRef(DefaultRepo + ":v0.1.0-darwin-arm64")
	if err != nil {
		t.Fatalf("the published repo should be accepted: %v", err)
	}
	if warn != "" {
		t.Errorf("a version pin on the published repo should be silent, got: %q", warn)
	}

	warn, err = checkRef("ghcr.io/somebody-else/hull-assets:darwin-arm64")
	if err != nil {
		t.Fatalf("a third-party mirror should not be refused outright: %v", err)
	}
	if warn == "" {
		t.Error("a third-party bundle should say where the kernel is coming from")
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
	if err := writeProvenance(dir, "ref:tag", "sha256:1234", want); err != nil {
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
