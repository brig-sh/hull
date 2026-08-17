//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// recordAttr writes the record the unpack leaves beside each file: mode, uid
// and gid, little-endian, under the name hvi and the block builder both read.
func recordAttr(t *testing.T, path string, mode, uid, gid uint32) {
	t.Helper()
	var v [12]byte
	binary.LittleEndian.PutUint32(v[0:4], mode)
	binary.LittleEndian.PutUint32(v[4:8], uid)
	binary.LittleEndian.PutUint32(v[8:12], gid)
	if err := unix.Setxattr(path, "com.nubificus.hvi.linux-attr", v[:], 0); err != nil {
		t.Skipf("cannot record guest attributes here: %v", err)
	}
}

// macOS cp clears setuid and setgid on everything it copies unless it runs as
// root, so a cloned rootfs arrives with sudo, su and passwd all disarmed. The
// guest reads the mode off that clone on vz, and refuses to run them.
func TestRestoreClonedModesPutsBackWhatCpDropped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, cp keeps the bits and there is nothing to restore")
	}
	src := t.TempDir()
	sudo := filepath.Join(src, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The unpack records what the image said, then applies it to the file.
	recordAttr(t, sudo, 0o4755, 0, 0)
	if err := syscall.Chmod(sudo, 0o4755); err != nil {
		t.Fatal(err)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("/bin/cp", "-c", "-a", src, clone).CombinedOutput()
	if err != nil {
		t.Fatalf("cp: %s: %v", out, err)
	}

	cloned := filepath.Join(clone, "sudo")
	st, err := os.Stat(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSetuid != 0 {
		t.Skip("this cp preserved setuid, so the bug under test does not reproduce here")
	}

	if err := restoreClonedModes(clone); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSetuid == 0 {
		t.Errorf("clone is %v: the guest would see a sudo it refuses to run", st.Mode())
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("permission bits are %o, want 755", st.Mode().Perm())
	}
}

// A file the image did not bring, or one with no special bits, must not be
// touched -- this walks a whole rootfs and should write to almost none of it.
func TestRestoreClonedModesLeavesOrdinaryFilesAlone(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreClonedModes(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("mode changed from %v to %v", before.Mode(), after.Mode())
	}
}
