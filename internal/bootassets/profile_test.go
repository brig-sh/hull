// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package bootassets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// profileEnv isolates a test from the host's asset and profile settings and
// pretends the host is arm64, the only architecture a profile variant has.
func profileEnv(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"HULL_BOOT_ASSETS", "BRIG_BOOT_ASSETS", PortableKernelEnv,
		PortableKernelRefEnv, VerifyModeEnv} {
		t.Setenv(k, "")
	}
	old := hostArch
	hostArch = "arm64"
	t.Cleanup(func() { hostArch = old })
	return t.TempDir()
}

// seedDefaultAssets writes a default kernel and initrd with their record, as
// a fetch would, so Ensure finds them and fetches nothing.
func seedDefaultAssets(t *testing.T, store string) string {
	t.Helper()
	dir := filepath.Join(store, "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]File{}
	for name, body := range map[string]string{KernelName(): "default kernel", InitrdName: "initrd"} {
		var rec File
		if err := writeFile(filepath.Join(dir, name), strings.NewReader(body), 0o644, &rec); err != nil {
			t.Fatal(err)
		}
		files[name] = rec
	}
	if err := writeProvenance(dir, Ref(), "sha256:feed", "", files); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeKernel(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizeProfile(t *testing.T) {
	for in, want := range map[string]string{"": "", "native": "", PortableArm64V1: PortableArm64V1} {
		got, err := NormalizeProfile(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeProfile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeProfile("portable-arm64-v2"); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("an unknown profile gave %v, want ErrUnknownProfile", err)
	}
}

// The variant sits inside the asset directory, so every way of moving that
// directory (the store, HULL_BOOT_ASSETS) moves the variant with it.
func TestProfileDirFollowsTheAssetDirectory(t *testing.T) {
	store := profileEnv(t)
	got, err := ProfileDir(store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(store, "assets", PortableArm64V1); got != want {
		t.Fatalf("ProfileDir = %q, want %q", got, want)
	}
	t.Setenv("HULL_BOOT_ASSETS", "/local/assets")
	if got, _ := ProfileDir(store, PortableArm64V1); got != "/local/assets/"+PortableArm64V1 {
		t.Fatalf("ProfileDir with HULL_BOOT_ASSETS = %q", got)
	}
	kernel, initrd, err := ProfilePaths(store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	if kernel != "/local/assets/"+PortableArm64V1+"/Image" || initrd != "/local/assets/"+InitrdName {
		t.Fatalf("ProfilePaths = %q, %q: want the variant kernel and the default initrd", kernel, initrd)
	}
}

func TestProfileVariantRefusals(t *testing.T) {
	store := profileEnv(t)
	if _, err := ProfileDir(store, "native"); err == nil {
		t.Fatal("the native profile has no variant directory")
	}
	hostArch = "amd64"
	if _, err := ProfileDir(store, PortableArm64V1); err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("an amd64 host gave %v, want a refusal naming the architecture", err)
	}
	hostArch = "arm64"
	t.Setenv(PortableKernelEnv, "/a/Image")
	t.Setenv(PortableKernelRefEnv, "ghcr.io/nofireai/hull-assets:portable")
	if _, _, err := ProfileSource(PortableArm64V1); err == nil {
		t.Fatal("both sources set must be refused")
	}
}

// A local kernel is copied, hashed on the way and recorded, so a later boot
// checks it like a fetched one; changing it afterwards is caught.
func TestImportProfileKernelRecordsTheFile(t *testing.T) {
	store := profileEnv(t)
	src := writeKernel(t, "hvi-time kernel")
	if err := ImportProfileKernel(store, PortableArm64V1, src); err != nil {
		t.Fatal(err)
	}
	rec, err := ProfileRecord(store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	sum, size, err := hashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ref != "file://"+src || rec.Digest != "sha256:"+sum || rec.VerifiedDigest != "" {
		t.Fatalf("record = %+v, want ref file://%s, digest sha256:%s and no verified digest", rec, src, sum)
	}
	if f := rec.Files["Image"]; f.SHA256 != sum || f.Size != size {
		t.Fatalf("record file = %+v, want %s/%d", f, sum, size)
	}
	kernel, _, _ := ProfilePaths(store, PortableArm64V1)
	if err := VerifyFiles(kernel); err != nil {
		t.Fatalf("the imported kernel does not verify: %v", err)
	}
	if err := os.WriteFile(kernel, []byte("something else"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFiles(kernel); err == nil {
		t.Fatal("a kernel changed after the import verified clean")
	}
}

func TestImportProfileKernelRefusals(t *testing.T) {
	store := profileEnv(t)
	if err := ImportProfileKernel(store, PortableArm64V1, writeKernel(t, "")); err == nil {
		t.Fatal("an empty kernel was imported")
	}
	if err := ImportProfileKernel(store, PortableArm64V1, t.TempDir()); err == nil {
		t.Fatal("a directory was imported as a kernel")
	}
	t.Setenv(VerifyModeEnv, "require")
	err := ImportProfileKernel(store, PortableArm64V1, writeKernel(t, "k"))
	if err == nil || !strings.Contains(err.Error(), "require") {
		t.Fatalf("HULL_VERIFY=require gave %v, want a refusal", err)
	}
	if ProfileKernelPresent(store, PortableArm64V1) {
		t.Fatal("a refused import left a kernel behind")
	}
}

// The first run of a profile installs its kernel from the configured file,
// takes the initrd from the default bundle, and a later run reuses the
// installed kernel rather than copying the file again.
func TestEnsureProfileInstallsFromTheConfiguredFile(t *testing.T) {
	store := profileEnv(t)
	defaultDir := seedDefaultAssets(t, store)
	src := writeKernel(t, "portable kernel")
	t.Setenv(PortableKernelEnv, src)
	kernel, initrd, err := EnsureProfile(context.Background(), store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	if kernel != filepath.Join(defaultDir, PortableArm64V1, "Image") || initrd != filepath.Join(defaultDir, InitrdName) {
		t.Fatalf("EnsureProfile = %q, %q", kernel, initrd)
	}
	if b, _ := os.ReadFile(kernel); string(b) != "portable kernel" {
		t.Fatalf("installed kernel holds %q", b)
	}
	if err := VerifyFiles(kernel, initrd); err != nil {
		t.Fatalf("the pair a profile boots does not verify: %v", err)
	}
	// The source changes; the installed kernel stays until asked.
	if err := os.WriteFile(src, []byte("rebuilt kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureProfile(context.Background(), store, PortableArm64V1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(kernel); string(b) != "portable kernel" {
		t.Fatalf("a second run replaced the installed kernel with %q", b)
	}
	if err := InstallProfile(context.Background(), store, PortableArm64V1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(kernel); string(b) != "rebuilt kernel" {
		t.Fatalf("InstallProfile left %q", b)
	}
}

func TestEnsureProfileWithoutASourceNamesTheRemedy(t *testing.T) {
	store := profileEnv(t)
	seedDefaultAssets(t, store)
	_, _, err := EnsureProfile(context.Background(), store, PortableArm64V1)
	if err == nil || !strings.Contains(err.Error(), PortableKernelEnv) {
		t.Fatalf("EnsureProfile with nothing configured gave %v, want the %s remedy", err, PortableKernelEnv)
	}
}

// A tampered variant kernel stops the run; it is not quietly reinstalled.
func TestEnsureProfileRefusesATamperedKernel(t *testing.T) {
	store := profileEnv(t)
	seedDefaultAssets(t, store)
	t.Setenv(PortableKernelEnv, writeKernel(t, "good"))
	kernel, _, err := EnsureProfile(context.Background(), store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = EnsureProfile(context.Background(), store, PortableArm64V1)
	if err == nil {
		t.Fatal("a tampered profile kernel was accepted")
	}
	// The remedy is the one that replaces the variant, and it names where
	// that command takes the kernel from. The plain `assets pull --force`
	// re-fetches the default bundle and leaves the variant as it is.
	msg := err.Error()
	if !strings.Contains(msg, "`hull assets pull --profile "+PortableArm64V1+" --force`") ||
		!strings.Contains(msg, PortableKernelEnv+"=") {
		t.Fatalf("the tampered kernel's refusal names the wrong remedy: %v", err)
	}
	t.Setenv(PortableKernelEnv, "")
	_, _, err = EnsureProfile(context.Background(), store, PortableArm64V1)
	if err == nil || !strings.Contains(err.Error(), "neither is set now") {
		t.Fatalf("with no source configured the remedy should say so: %v", err)
	}
}

// ReinstallCommand names the command that puts a directory's files back.
func TestReinstallCommand(t *testing.T) {
	if got := ReinstallCommand("/s/assets"); got != "`hull assets pull --force`" {
		t.Errorf("the bundle's directory: %s", got)
	}
	if got := ReinstallCommand("/s/assets/" + PortableArm64V1); !strings.Contains(got, "--profile "+PortableArm64V1) {
		t.Errorf("the profile's directory: %s", got)
	}
}

// VerifyProfileKernel goes by the bytes: the installed kernel and a copy of
// it at another path pass, another kernel does not, and with no kernel
// installed there is nothing to compare against.
func TestVerifyProfileKernel(t *testing.T) {
	store := profileEnv(t)
	defaultDir := seedDefaultAssets(t, store)
	copyOf := writeKernel(t, "portable kernel")
	err := VerifyProfileKernel(store, PortableArm64V1, copyOf, "the copy")
	if err == nil || !strings.Contains(err.Error(), "hull assets pull --profile") {
		t.Fatalf("with no profile kernel installed: %v", err)
	}
	if err := ImportProfileKernel(store, PortableArm64V1, copyOf); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(defaultDir, PortableArm64V1, ProfileKernelName)
	for _, path := range []string{installed, copyOf} {
		if err := VerifyProfileKernel(store, PortableArm64V1, path, path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	native := filepath.Join(defaultDir, KernelName())
	err = VerifyProfileKernel(store, PortableArm64V1, native, "the default kernel")
	if err == nil || !strings.Contains(err.Error(), "the default kernel is not the "+PortableArm64V1+" kernel") {
		t.Fatalf("the default bundle's kernel: %v", err)
	}
}

// fakeRegistry serves an OCI registry over plain http on loopback, and lets
// the fetch take a kernel from it as a mirror the operator allowed.
func fakeRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	t.Setenv("HULL_BOOT_ASSETS_INSECURE", "1")
	t.Setenv(ForeignRepoEnv, "1")
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushArtifact pushes an artifact whose layers are the given titled files.
func pushArtifact(t *testing.T, ref string, files map[string]string) {
	t.Helper()
	img := empty.Image
	for title, body := range files {
		var err error
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:       static.NewLayer([]byte(body), types.MediaType("application/octet-stream")),
			Annotations: map[string]string{titleAnnotation: title},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatal(err)
	}
}

// An artifact without an Image layer is refused before it writes anything:
// the installed kernel and its record are as they were, and the next run
// still boots it.
func TestFetchProfileWithoutAKernelChangesNothing(t *testing.T) {
	store := profileEnv(t)
	seedDefaultAssets(t, store)
	if err := ImportProfileKernel(store, PortableArm64V1, writeKernel(t, "portable kernel")); err != nil {
		t.Fatal(err)
	}
	dir, err := ProfileDir(store, PortableArm64V1)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, provenanceName))
	if err != nil {
		t.Fatal(err)
	}
	host := fakeRegistry(t)
	ref := host + "/kernels/portable:no-image"
	pushArtifact(t, ref, map[string]string{"vmlinux": "a kernel by another name"})
	err = FetchProfile(context.Background(), ref, store, PortableArm64V1)
	if err == nil || !strings.Contains(err.Error(), "no layer titled "+ProfileKernelName) {
		t.Fatalf("FetchProfile of an artifact with no Image layer: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, provenanceName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the refused fetch rewrote the record:\n%s\nwas\n%s", after, before)
	}
	if _, err := os.Stat(filepath.Join(dir, "vmlinux")); !os.IsNotExist(err) {
		t.Fatalf("the refused fetch wrote its layer: %v", err)
	}
	if _, _, err := EnsureProfile(context.Background(), store, PortableArm64V1); err != nil {
		t.Fatalf("the installed kernel no longer boots after the refused fetch: %v", err)
	}

	// The same registry with an Image layer installs it, recorded.
	ref = host + "/kernels/portable:good"
	pushArtifact(t, ref, map[string]string{ProfileKernelName: "fetched kernel"})
	if err := FetchProfile(context.Background(), ref, store, PortableArm64V1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ProfileKernelName)); string(b) != "fetched kernel" {
		t.Fatalf("the fetched kernel holds %q", b)
	}
	rec, err := ProfileRecord(store, PortableArm64V1)
	if err != nil || rec.Ref != ref {
		t.Fatalf("record after the fetch = %+v, %v", rec, err)
	}
}

// An artifact reference goes through the same reference check as the
// default bundle, before any bytes move.
func TestFetchProfileChecksTheReference(t *testing.T) {
	store := profileEnv(t)
	err := FetchProfile(context.Background(), "ghcr.io/someone/else:portable", store, PortableArm64V1)
	if err == nil || !strings.Contains(err.Error(), ForeignRepoEnv) {
		t.Fatalf("a foreign repository gave %v, want the %s refusal", err, ForeignRepoEnv)
	}
}
