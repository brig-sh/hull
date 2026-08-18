//go:build darwin

// Copyright (c) 2023-2026, Nubificus LTD
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/bootassets"
)

func TestParseAnnotationEntriesLastValueWins(t *testing.T) {
	got, err := parseAnnotationEntries([]string{
		"com.urunc.unikernel.bootKernel=/old/Image",
		"com.urunc.unikernel.bootKernel=/new/Image",
		"com.urunc.unikernel.bootInitrd=/host/container-initrd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["com.urunc.unikernel.bootKernel"] != "/new/Image" {
		t.Fatalf("last annotation did not win: %v", got)
	}
	if _, err := parseAnnotationEntries([]string{"missing-value-separator"}); err == nil {
		t.Fatal("expected malformed annotation to fail")
	}
}

// The generic boot assets must not displace a kernel the image brought itself,
// and "unikernel" is the sentinel pkg/ociclient writes when the image brought
// nothing -- not a path.
func TestImageCarriesKernel(t *testing.T) {
	for name, tc := range map[string]struct {
		annotations map[string]string
		want        bool
	}{
		"no annotations at all":  {map[string]string{}, false},
		"ociclient defaults":     {map[string]string{"com.urunc.unikernel.binary": "unikernel", "com.urunc.unikernel.kernel": "unikernel"}, false},
		"empty values":           {map[string]string{"com.urunc.unikernel.binary": "", "com.urunc.unikernel.kernel": ""}, false},
		"bunny binary":           {map[string]string{"com.urunc.unikernel.binary": "/unikernel/app.elf"}, true},
		"kernel key only":        {map[string]string{"com.urunc.unikernel.kernel": "boot/vmlinuz"}, true},
		"binary sentinel+kernel": {map[string]string{"com.urunc.unikernel.binary": "unikernel", "com.urunc.unikernel.kernel": "boot/vmlinuz"}, true},
	} {
		if got := imageCarriesKernel(tc.annotations); got != tc.want {
			t.Errorf("%s: imageCarriesKernel = %v, want %v", name, got, tc.want)
		}
	}
}

func TestContainerHostsDoesNotModifyRootfs(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostsPath := filepath.Join(rootfs, "etc", "hosts")
	const original = "127.0.0.1 localhost\n"
	if err := os.WriteFile(hostsPath, []byte(original), 0o444); err != nil {
		t.Fatal(err)
	}

	got, err := containerHosts(rootfs, []string{"database:192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, original) || !strings.Contains(got, "192.0.2.10\tdatabase") {
		t.Fatalf("unexpected staged hosts file: %q", got)
	}
	after, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("shared OCI rootfs was modified: %q", after)
	}
}

func TestCloneRootfsAPFSReplacesOnlyInstanceSymlink(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "cached-rootfs")
	if err := os.MkdirAll(filepath.Join(source, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(source, "etc", "issue")
	if err := os.WriteFile(cacheFile, []byte("cached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheAlias := filepath.Join(source, "etc", "issue-alias")
	if err := os.Link(cacheFile, cacheAlias); err != nil {
		t.Fatal(err)
	}
	instanceRootfs := filepath.Join(base, "instance-rootfs")
	if err := os.Symlink(source, instanceRootfs); err != nil {
		t.Fatal(err)
	}

	if err := cloneRootfsAPFS(source, instanceRootfs); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(instanceRootfs); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("instance rootfs was not replaced by a directory: %v", info.Mode())
	}
	if err := os.WriteFile(filepath.Join(instanceRootfs, "etc", "issue"), []byte("instance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cloneFileInfo, err := os.Stat(filepath.Join(instanceRootfs, "etc", "issue"))
	if err != nil {
		t.Fatal(err)
	}
	cloneAliasInfo, err := os.Stat(filepath.Join(instanceRootfs, "etc", "issue-alias"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(cloneFileInfo, cloneAliasInfo) {
		t.Fatal("APFS clone did not preserve the source hard-link relationship")
	}
	if alias, err := os.ReadFile(filepath.Join(instanceRootfs, "etc", "issue-alias")); err != nil {
		t.Fatal(err)
	} else if string(alias) != "instance\n" {
		t.Fatalf("cloned hard-link alias did not see mutation: %q", alias)
	}
	got, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cached\n" {
		t.Fatalf("instance mutation reached cached rootfs: %q", got)
	}
}

// seedRecordedBootAssets writes a kernel and an initrd with the provenance
// record a fetch would have left beside them. It builds the record from the
// exported shape rather than reaching inside the package, so it exercises the
// same file a real fetch writes.
func seedRecordedBootAssets(t *testing.T) (dir, kernel, initrd string) {
	t.Helper()
	dir = t.TempDir()
	files := map[string]bootassets.File{}
	for name, content := range map[string]string{
		bootassets.KernelName(): "the kernel that was fetched",
		bootassets.InitrdName:   "the initrd that was fetched",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		sum := sha256.Sum256([]byte(content))
		files[name] = bootassets.File{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
	}
	record, err := json.Marshal(bootassets.Provenance{
		Ref:            "ghcr.io/nofireai/hull-assets:darwin-arm64",
		Digest:         "sha256:abc",
		VerifiedDigest: "sha256:abc",
		Files:          files,
	})
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), record, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return dir, filepath.Join(dir, bootassets.KernelName()), filepath.Join(dir, bootassets.InitrdName)
}

// TestVerifyContainerBootAssetsRefusesATamperedKernelFromAnnotations is defect
// 1, and it is the only test here that proves the fix reaches anybody.
//
// These arguments are the bootKernel/bootInitrd annotation values -- the form
// brig hands hull for six of its eight shipped profiles, having found the files
// with a stat and a non-zero-size check of its own. The content verification
// used to live behind the branch hull takes only when it resolves the assets
// itself, so on this path it never ran: annotations set, branch skipped, kernel
// straight to the VMM. A tampered kernel has to be refused here, given nothing
// but the paths, or the fix is decoration.
func TestVerifyContainerBootAssetsRefusesATamperedKernelFromAnnotations(t *testing.T) {
	_, kernel, initrd := seedRecordedBootAssets(t)

	// The control: the pair as fetched is booted, or every later assertion
	// would pass for the wrong reason.
	if err := verifyContainerBootAssets(kernel, initrd); err != nil {
		t.Fatalf("the assets as fetched should boot, got: %v", err)
	}

	if err := os.WriteFile(kernel, []byte("an attacker's kernel"), 0o644); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}
	err := verifyContainerBootAssets(kernel, initrd)
	if err == nil {
		t.Fatal("a tampered kernel supplied by annotation was accepted: the brig path is open")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("the refusal should be the content mismatch, got: %v", err)
	}
}

// TestVerifyContainerBootAssetsRefusesATamperedInitrdFromAnnotations covers the
// other file. The initrd carries the in-guest agent, so it is worth as much to
// an attacker as the kernel.
func TestVerifyContainerBootAssetsRefusesATamperedInitrdFromAnnotations(t *testing.T) {
	_, kernel, initrd := seedRecordedBootAssets(t)
	if err := os.WriteFile(initrd, []byte("an attacker's initrd"), 0o644); err != nil {
		t.Fatalf("swap initrd: %v", err)
	}
	if err := verifyContainerBootAssets(kernel, initrd); err == nil {
		t.Fatal("a tampered initrd supplied by annotation was accepted")
	}
}

// TestVerifyContainerBootAssetsRefusesARecordThatDropsTheKernel: editing the
// record is cheaper than forging a hash, and it has to be caught on the
// annotation path too.
func TestVerifyContainerBootAssetsRefusesARecordThatDropsTheKernel(t *testing.T) {
	dir, kernel, initrd := seedRecordedBootAssets(t)
	b, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var p bootassets.Provenance
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	delete(p.Files, bootassets.KernelName())
	edited, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), edited, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := os.WriteFile(kernel, []byte("any kernel at all"), 0o644); err != nil {
		t.Fatalf("swap kernel: %v", err)
	}
	if err := verifyContainerBootAssets(kernel, initrd); err == nil {
		t.Fatal("an unrecorded kernel booted because the record no longer mentioned it")
	}
}

// TestVerifyContainerBootAssetsAllowsAKernelNobodyFetched. Pointing
// HULL_BOOT_ASSETS at a local build is a supported way to work and there is
// nothing to compare that kernel against, so it is reported and booted. A check
// that refused it would be turned off within a day.
func TestVerifyContainerBootAssetsAllowsAKernelNobodyFetched(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, bootassets.KernelName())
	initrd := filepath.Join(dir, bootassets.InitrdName)
	for _, p := range []string{kernel, initrd} {
		if err := os.WriteFile(p, []byte("a local build"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := verifyContainerBootAssets(kernel, initrd); err != nil {
		t.Fatalf("a kernel with no record should be allowed, got: %v", err)
	}
}

// TestVerifyContainerBootAssetsIgnoresANonContainerBoot: no annotations, no
// paths, nothing to check -- and no error either, since an image that brought
// its own kernel does not come through here.
func TestVerifyContainerBootAssetsIgnoresANonContainerBoot(t *testing.T) {
	if err := verifyContainerBootAssets("", ""); err != nil {
		t.Fatalf("a run with no boot annotations should be left alone, got: %v", err)
	}
}
