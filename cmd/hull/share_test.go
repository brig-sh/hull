//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// openDirFd opens a directory and hands back the raw descriptor, the way a
// caller passes one to hull. The descriptor is owned by whatever it is given
// to, so nothing here closes it.
func openDirFd(t *testing.T, dir string) int {
	t.Helper()
	fd, err := syscall.Open(dir, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return fd
}

func closeShareFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func identity(t *testing.T, path string) (int32, uint64) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Dev, st.Ino
}

func TestParseSharesFromPaths(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()

	shares, files, err := parseShares([]string{first + ":/mnt/a", second + ":/mnt/b:ro"}, nil, "vz")
	defer closeShareFiles(files)
	if err != nil {
		t.Fatalf("parseShares: %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("got %d shares, want 2", len(shares))
	}
	if shares[0].host != first || shares[0].guest != "/mnt/a" || shares[0].tag != "share0" || shares[0].readOnly {
		t.Errorf("first share = %+v", shares[0])
	}
	if shares[1].host != second || shares[1].guest != "/mnt/b" || shares[1].tag != "share1" || !shares[1].readOnly {
		t.Errorf("second share = %+v", shares[1])
	}
	if len(files) != 0 {
		t.Errorf("a path share must not hold a descriptor, got %d", len(files))
	}
}

// The reason the flag exists: a descriptor names the directory the caller
// opened, and hull turns it into a name for that identity rather than
// resolving one of its own.
func TestParseSharesFromFdNamesTheIdentity(t *testing.T) {
	dir := t.TempDir()
	wantDev, wantIno := identity(t, dir)

	shares, files, err := parseShares(nil, []string{fdSpec(openDirFd(t, dir), "/root")}, "vz")
	defer closeShareFiles(files)
	if err != nil {
		t.Fatalf("parseShares: %v", err)
	}
	if len(shares) != 1 || len(files) != 1 {
		t.Fatalf("got %d shares and %d descriptors, want 1 and 1", len(shares), len(files))
	}
	if shares[0].guest != "/root" || shares[0].tag != "share0" {
		t.Errorf("share = %+v", shares[0])
	}
	gotDev, gotIno := identity(t, shares[0].host)
	if gotDev != wantDev || gotIno != wantIno {
		t.Errorf("%s names (%d,%d), want the opened directory (%d,%d)",
			shares[0].host, gotDev, gotIno, wantDev, wantIno)
	}
}

// The window this closes. Between the caller handing the directory over and
// the VM starting, anything that can write to a parent component can put
// another directory at that name. What the guest receives must not change.
func TestParseSharesFromFdSurvivesAParentSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	real := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "marker"), []byte("original"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fd := openDirFd(t, real)

	// The name is taken over: the directory the caller opened moves aside and
	// an impostor takes its place. A share resolved by name lands on the
	// impostor from here on.
	if err := os.Rename(real, filepath.Join(parent, "moved")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "marker"), []byte("impostor"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	shares, files, err := parseShares(nil, []string{fdSpec(fd, "/root")}, "vz")
	defer closeShareFiles(files)
	if err != nil {
		t.Fatalf("parseShares: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(shares[0].host, "marker"))
	if err != nil {
		t.Fatalf("read the marker through the share: %v", err)
	}
	if string(marker) != "original" {
		t.Errorf("the share resolves to %q, want the directory the caller opened", marker)
	}
}

// Both flags feed one list of exports, and every export needs its own tag: two
// shares under one tag would mount the same directory twice.
func TestParseSharesTagsAcrossBothFlags(t *testing.T) {
	byPath, byFd := t.TempDir(), t.TempDir()

	shares, files, err := parseShares([]string{byPath + ":/mnt/a"}, []string{fdSpec(openDirFd(t, byFd), "/mnt/b")}, "vz")
	defer closeShareFiles(files)
	if err != nil {
		t.Fatalf("parseShares: %v", err)
	}
	if len(shares) != 2 || shares[0].tag != "share0" || shares[1].tag != "share1" {
		t.Fatalf("tags = %q, %q", shares[0].tag, shares[1].tag)
	}
}

func TestParseSharesFromFdRejections(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fileFd, err := syscall.Open(file, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}

	for _, tc := range []struct {
		name, spec, want string
	}{
		{"no guest path", "7", "expected FD:/guest/path"},
		{"not a number", "seven:/root", "descriptor number"},
		{"a standard stream", "1:/root", "descriptor number"},
		{"negative", "-1:/root", "descriptor number"},
		{"relative guest path", "7:root", "must be absolute"},
		{"a file, not a directory", fdSpec(fileFd, "/root"), "not a directory"},
		{"unknown mode", "7:/root:rx", "want ro or rw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shares, files, err := parseShares(nil, []string{tc.spec}, "vz")
			closeShareFiles(files)
			if err == nil {
				t.Fatalf("%q was accepted as %+v", tc.spec, shares)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A read-only share cannot be honored on the 9p path, and mounting it
// read-write instead is the one outcome worth refusing. The rule is the flag's
// whichever way the directory was named.
func TestParseSharesFromFdRefusesReadOnlyOnQemu(t *testing.T) {
	dir := t.TempDir()

	_, files, err := parseShares(nil, []string{fdSpec(openDirFd(t, dir), "/root") + ":ro"}, "qemu")
	closeShareFiles(files)
	if err == nil {
		t.Fatal("a read-only share must be refused on the qemu backend")
	}
	if !strings.Contains(err.Error(), "read-only shares are not supported") {
		t.Errorf("error = %v", err)
	}
}

func fdSpec(fd int, guest string) string {
	return strconv.Itoa(fd) + ":" + guest
}

// The same directory can reasonably be wanted at two guest paths. hull works on
// copies of the caller's descriptor, so neither share closes the other's, and
// the caller's own descriptor is never closed at all.
func TestParseSharesFromFdAcceptsTheSameDescriptorTwice(t *testing.T) {
	dir := t.TempDir()
	fd := openDirFd(t, dir)
	defer func() { _ = syscall.Close(fd) }()

	shares, files, err := parseShares(nil, []string{fdSpec(fd, "/mnt/a"), fdSpec(fd, "/mnt/b")}, "vz")
	if err != nil {
		t.Fatalf("parseShares: %v", err)
	}
	if len(shares) != 2 || len(files) != 2 {
		t.Fatalf("got %d shares and %d descriptors, want 2 and 2", len(shares), len(files))
	}
	if shares[0].host != shares[1].host {
		t.Errorf("the same directory produced %q and %q", shares[0].host, shares[1].host)
	}
	if files[0].Fd() == files[1].Fd() {
		t.Error("both shares hold one descriptor number, so closing either closes the other")
	}
	closeShareFiles(files)

	// The caller's descriptor is still the caller's.
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Errorf("hull closed the caller's descriptor: %v", err)
	}
}

// What a caller that forgot to clear FD_CLOEXEC passes: a number naming
// nothing. The message has to say so, rather than report a failure to read a
// directory the caller believes it handed over.
func TestParseSharesFromFdRejectsADescriptorNotHeld(t *testing.T) {
	spare := openDirFd(t, t.TempDir())
	if err := syscall.Close(spare); err != nil {
		t.Fatalf("close: %v", err)
	}

	shares, files, err := parseShares(nil, []string{fdSpec(spare, "/root")}, "vz")
	closeShareFiles(files)
	if err == nil {
		t.Fatalf("a descriptor the process does not hold was accepted as %+v", shares)
	}
	if !strings.Contains(err.Error(), "is not a descriptor this process holds") {
		t.Errorf("error = %v, want it to name the descriptor as the problem", err)
	}
	if !strings.Contains(err.Error(), "FD_CLOEXEC") {
		t.Errorf("error = %v, want it to name the likely cause", err)
	}
}

// An error names the flag the caller passed, not the other one.
func TestParseSharesErrorsNameTheirOwnFlag(t *testing.T) {
	dir := t.TempDir()

	_, files, err := parseShares(nil, []string{fdSpec(openDirFd(t, dir), "/root") + ":rx"}, "vz")
	closeShareFiles(files)
	if err == nil || !strings.Contains(err.Error(), "shared-dir-fd mode") {
		t.Errorf("error = %v, want it to name shared-dir-fd", err)
	}

	_, files, err = parseShares([]string{dir + ":/root:rx"}, nil, "vz")
	closeShareFiles(files)
	if err == nil || !strings.Contains(err.Error(), "shared-dir mode") {
		t.Errorf("error = %v, want it to name shared-dir", err)
	}
}

// A read-only share on the 9p path is refused whichever flag named it, and the
// refusal names that flag.
func TestParseSharesFromFdReadOnlyRefusalNamesTheFlag(t *testing.T) {
	dir := t.TempDir()

	_, files, err := parseShares(nil, []string{fdSpec(openDirFd(t, dir), "/root") + ":ro"}, "qemu")
	closeShareFiles(files)
	if err == nil {
		t.Fatal("a read-only share must be refused on the qemu backend")
	}
	if !strings.Contains(err.Error(), "shared-dir-fd") {
		t.Errorf("error = %v, want it to name shared-dir-fd", err)
	}
}
