//go:build darwin

// Copyright (c) 2023-2026, Nubificus LTD
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
