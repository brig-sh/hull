//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// --- containment: the block builder's own walks ------------------------------
//
// Everything below plants what a malicious image would plant and checks the two
// things that matter: hull stays inside the rootfs, and the victim file outside
// it is byte-for-byte and mode-for-mode what it was. Every victim lives under
// the test's own t.TempDir(); nothing here names a real path.
//
// The sink that made this necessary is the mode restore. Before the os.Root
// rewrite it walked the tree with filepath.WalkDir and handed the callback's
// path to syscall.Chmod, which resolves the path itself and follows every
// symlink in it. That expression, run against the tree these tests build,
// turns a 0600 private key outside the rootfs into a setuid 04755 file:
//
//	syscall.Chmod(filepath.Join(rootfs, "sudo"), 0o4755)  =>  victim urwxr-xr-x
//
// The only thing that kept the whole function from doing it was a d.Type() test
// on a directory entry read out of the image's own tree earlier in the walk --
// a check-then-act, one edit away from being wrong, and exactly the shape of
// defect the rest of cmd/hull was audited for.

// recordLinkAttr records a guest attribute on a symlink itself rather than on
// whatever it points at. This is what the unpack does for symlink entries
// (recordGuestAttr passes XATTR_NOFOLLOW), and it matters here: without it the
// test would set the attribute on the victim, and the test would be planting
// the very change it is trying to detect.
func recordLinkAttr(t *testing.T, path string, mode, uid, gid uint32) {
	t.Helper()
	var v [12]byte
	binary.LittleEndian.PutUint32(v[0:4], mode)
	binary.LittleEndian.PutUint32(v[4:8], uid)
	binary.LittleEndian.PutUint32(v[8:12], gid)
	if err := unix.Setxattr(path, "com.nubificus.hvi.linux-attr", v[:], unix.XATTR_NOFOLLOW); err != nil {
		t.Skipf("cannot record guest attributes here: %v", err)
	}
}

// An image can ship any name in its rootfs as an absolute symlink onto the
// host -- the extractor preserves those on purpose, because real images need
// them. The mode restore must not follow one, whether it walks into it or is
// handed the name directly.
func TestRestoreClonedModesStaysInsideTheRootfs(t *testing.T) {
	base, rootfs := newRootfs(t)
	key := plantVictim(t, filepath.Join(base, "home", ".ssh"), "id_ed25519",
		"-----BEGIN OPENSSH PRIVATE KEY-----\n")

	// "sudo" as an absolute symlink to the caller's private key, with the
	// image declaring it setuid root. Following this is a chmod 4755 of a
	// host key: readable by everyone, and executable as its owner.
	planted := filepath.Join(rootfs, "sudo")
	if err := os.Symlink(key.path, planted); err != nil {
		t.Fatal(err)
	}
	recordLinkAttr(t, planted, 0o4755, 0, 0)

	if err := restoreClonedModes(rootfs); err != nil {
		t.Errorf("a planted entry should be skipped, not fail the boot: %v", err)
	}
	key.mustBeUntouched(t)

	// And the sink itself, given the planted name directly rather than reached
	// through the walk: this is what a future edit that drops the symlink test
	// above would end up doing, so it has to refuse on its own account.
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	mustRefuse(t, rootfsRefusal("restore the mode of", "sudo",
		root.Chmod("sudo", guestFileMode(0o4755))), "sudo")
	key.mustBeUntouched(t)
}

// A relative symlink escapes just as well as an absolute one, and reads more
// innocently in an image listing. "../../../.." is fixed-length and needs to
// know nothing about how deep the bundle happens to live.
func TestRestoreClonedModesRefusesRelativeEscape(t *testing.T) {
	base, rootfs := newRootfs(t)
	conf := plantVictim(t, filepath.Join(base, "home"), "zshrc", "export PATH=/usr/bin\n")

	if err := os.Symlink("../../home", filepath.Join(rootfs, "esc")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	mustRefuse(t, rootfsRefusal("restore the mode of", "esc/zshrc",
		root.Chmod("esc/zshrc", guestFileMode(0o4777))), "esc/zshrc")
	conf.mustBeUntouched(t)

	if err := restoreClonedModes(rootfs); err != nil {
		t.Errorf("a planted entry should be skipped, not fail the boot: %v", err)
	}
	conf.mustBeUntouched(t)
}

// The ownership pass reads an xattr off every path in the tree and emits a
// debugfs command naming it. A planted directory symlink must not turn that
// into a walk of the caller's home, and no host path may reach the script:
// debugfs resolves the names it is given inside the ext4 image, so a host path
// in there is a hull bug rather than an escape, but it is the first half of one.
func TestOwnershipScriptStaysInsideTheRootfs(t *testing.T) {
	base, rootfs := newRootfs(t)
	secrets := filepath.Join(base, "home", "secrets")
	token := plantVictim(t, secrets, "api-token", "sk-live-do-not-read\n")
	recordAttr(t, token.path, 0o100600, 501, 20)

	// "etc" as an absolute symlink to a directory of the caller's, which is the
	// cheapest way for an image to aim a tree walk somewhere it should not go.
	if err := os.Symlink(secrets, filepath.Join(rootfs, "etc")); err != nil {
		t.Fatal(err)
	}
	// One real file, so the test can tell "walked nothing" from "walked only
	// what it should".
	real := filepath.Join(rootfs, "init")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordAttr(t, real, 0o100755, 0, 0)

	var script bytes.Buffer
	skipped, err := appendOwnershipScript(&script, rootfs)
	if err != nil {
		t.Fatalf("appendOwnershipScript: %v", err)
	}
	if skipped != 0 {
		t.Errorf("nothing here is inexpressible, but %d path(s) were skipped", skipped)
	}
	got := script.String()
	if !strings.Contains(got, `sif "/init" uid 0`) {
		t.Errorf("the real file lost its recorded ownership:\n%s", got)
	}
	if strings.Contains(got, "api-token") || strings.Contains(got, base) {
		t.Errorf("HOST PATH REACHED THE DEBUGFS SCRIPT -- the walk followed the "+
			"planted \"etc\" out of the rootfs:\n%s", got)
	}
	token.mustBeUntouched(t)
}

// --- positive control --------------------------------------------------------

// Real images are full of contained relative symlinks ("usr/bin/awk ->
// ../../etc/alternatives/awk"), and the containment must not cost them
// anything: a rootfs built out of ordinary image content still has to produce a
// correct image, or the fix has traded a security bug for a broken runtime.
func TestBlockRootfsBuildsABenignImage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the mode restore has nothing to put back")
	}
	for _, bin := range []string{mke2fsBin, debugfsBin} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s is not installed: %v", bin, err)
		}
	}
	base, rootfs := newRootfs(t)

	for _, dir := range []string{"usr/bin", "etc/alternatives", "etc"} {
		if err := os.MkdirAll(filepath.Join(rootfs, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	awk := filepath.Join(rootfs, "etc/alternatives/awk")
	if err := os.WriteFile(awk, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordAttr(t, awk, 0o100755, 0, 0)
	// The contained relative symlink the whole containment argument is built
	// around: it must survive, both as an entry and as a recorded ownership.
	link := filepath.Join(rootfs, "usr/bin/awk")
	if err := os.Symlink("../../etc/alternatives/awk", link); err != nil {
		t.Fatal(err)
	}
	recordLinkAttr(t, link, 0o120777, 0, 0)
	sudo := filepath.Join(rootfs, "usr/bin/sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordAttr(t, sudo, 0o4755, 0, 0)
	if err := syscall.Chmod(sudo, 0o4755); err != nil {
		t.Fatal(err)
	}

	// The mode restore leaves a tree that already has its bits alone, and
	// still reaches every file through the contained relative link's siblings.
	if err := restoreClonedModes(rootfs); err != nil {
		t.Fatalf("restoreClonedModes on a benign rootfs: %v", err)
	}
	if st, err := os.Lstat(sudo); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSetuid == 0 {
		t.Errorf("sudo came out %v: the guest would refuse to run it", st.Mode())
	}
	if st, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the contained relative symlink was replaced by a %v", st.Mode())
	}

	disk := filepath.Join(base, "rootfs.ext4")
	injects := []blockInject{
		{guestPath: "/urunit.conf", content: []byte("URUNIT_ENV=PATH=/usr/bin\n"), mode: 0o600},
		{guestPath: "/etc/hosts", content: []byte("127.0.0.1 localhost\n"), mode: 0o644},
	}
	if err := buildBlockRootfs(disk, rootfs, injects, 16); err != nil {
		t.Fatalf("buildBlockRootfs on a benign rootfs: %v", err)
	}

	// Read the finished image back rather than trusting the exit codes: debugfs
	// exits 0 whatever happened, which is the whole reason buildBlockRootfs
	// reads its output instead.
	out, err := exec.Command(debugfsBin, "-R", "stat /usr/bin/sudo", disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "Mode:  04755") {
		t.Errorf("setuid did not survive into the image:\n%s", out)
	}
	if !strings.Contains(string(out), "User:     0") {
		t.Errorf("recorded ownership did not survive into the image:\n%s", out)
	}
	out, err = exec.Command(debugfsBin, "-R", "stat /usr/bin/awk", disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "Fast link dest: \"../../etc/alternatives/awk\"") {
		t.Errorf("the contained relative symlink did not survive into the image:\n%s", out)
	}
	out, err = exec.Command(debugfsBin, "-R", "cat /urunit.conf", disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs cat: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "URUNIT_ENV=PATH=/usr/bin") {
		t.Errorf("the injected urunit.conf is not in the image:\n%s", out)
	}
}

// guestFileMode translates the image's raw Linux mode word into the fs.FileMode
// os.Root.Chmod takes. The three bits that matter live in different places in
// the two representations, and getting it wrong would drop exactly the bits the
// mode restore exists to put back -- silently, because the chmod would succeed.
func TestGuestFileModeCarriesTheSpecialBits(t *testing.T) {
	for _, tc := range []struct {
		raw  uint32
		want os.FileMode
	}{
		{0o755, 0o755},
		{0o4755, 0o755 | os.ModeSetuid},
		{0o2755, 0o755 | os.ModeSetgid},
		{0o1777, 0o777 | os.ModeSticky},
		{0o7000, os.ModeSetuid | os.ModeSetgid | os.ModeSticky},
	} {
		if got := guestFileMode(tc.raw); got != tc.want {
			t.Errorf("guestFileMode(%#o) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
