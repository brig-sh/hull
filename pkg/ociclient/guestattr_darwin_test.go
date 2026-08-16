//go:build darwin

package ociclient

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// readGuestAttr reads back what hvi's virtio-fs would read: mode, uid, gid,
// little-endian, from the xattr named in guestattr_darwin.go.
func readGuestAttr(t *testing.T, path string, symlink bool) (mode, uid, gid uint32) {
	t.Helper()
	buf := make([]byte, 12)
	flags := 0
	if symlink {
		flags = unix.XATTR_NOFOLLOW
	}
	n, err := unix.Getxattr(path, guestAttrXattr, buf)
	if err != nil {
		// Getxattr on darwin follows symlinks; for a link the value was
		// written with NOFOLLOW, so read it the same way.
		if symlink {
			t.Fatalf("no image ownership recorded for the symlink %s: %v (flags %d)", path, err, flags)
		}
		t.Fatalf("no image ownership recorded for %s: %v", path, err)
	}
	if n != 12 {
		t.Fatalf("%s: recorded %d bytes, hvi reads exactly 12", path, n)
	}
	return binary.LittleEndian.Uint32(buf[0:4]),
		binary.LittleEndian.Uint32(buf[4:8]),
		binary.LittleEndian.Uint32(buf[8:12])
}

func attrTarball(t *testing.T, entries []*tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if body, ok := bodies[h.Name]; ok {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body, ok := bodies[h.Name]; ok {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The bug this pins down: an unprivileged unpack on macOS cannot chown to
// root, so the guest saw /usr/bin/sudo owned by the host user (501) and
// refused to run -- "must be owned by uid 0 and have the setuid bit set".
// The ownership has to survive somewhere, and this is where.
func TestUnpackRecordsImageOwnership(t *testing.T) {
	dir := t.TempDir()
	data := attrTarball(t, []*tar.Header{
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 0, Gid: 0},
		// setuid root, which is the whole point.
		{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0},
		// A file the image says belongs to the workload user.
		{Name: "home/claude/.bashrc", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1000, Gid: 1000},
		{Name: "usr/bin/editor", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin/vi", Mode: 0o777, Uid: 0, Gid: 0},
	}, map[string]string{
		"usr/bin/sudo":        "#!/bin/sh\n",
		"home/claude/.bashrc": "export PS1=\n",
	})

	if err := extractTarIgnoreChown(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}

	// The host cannot express any of this, which is exactly why it is
	// recorded rather than applied.
	if st, err := os.Stat(filepath.Join(dir, "usr/bin/sudo")); err != nil {
		t.Fatal(err)
	} else if st.Sys().(*syscall.Stat_t).Uid == 0 && os.Geteuid() != 0 {
		t.Fatal("the test is not proving anything: the file really is root-owned")
	}

	mode, uid, gid := readGuestAttr(t, filepath.Join(dir, "usr/bin/sudo"), false)
	if uid != 0 || gid != 0 {
		t.Errorf("sudo recorded as %d:%d, want 0:0 -- the guest would refuse to run it", uid, gid)
	}
	// hvi keeps the low 12 bits, so the setuid bit has to be in them.
	if mode&0o7777 != 0o4755 {
		t.Errorf("sudo mode recorded as %o, want 4755 (the setuid bit is what sudo needs)", mode&0o7777)
	}

	if _, uid, gid := readGuestAttr(t, filepath.Join(dir, "home/claude/.bashrc"), false); uid != 1000 || gid != 1000 {
		t.Errorf(".bashrc recorded as %d:%d, want 1000:1000", uid, gid)
	}

	if _, uid, _ := readGuestAttr(t, filepath.Join(dir, "usr/bin/"), false); uid != 0 {
		t.Errorf("directory recorded as uid %d, want 0", uid)
	}
}

// A symlink's own ownership is recorded on the link, not on whatever it points
// at. Following it would stamp the link's attributes onto another file of the
// image -- and for a dangling link, would fail outright.
func TestUnpackRecordsSymlinkWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	data := attrTarball(t, []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 0, Gid: 0},
		// Deliberately dangling: nothing in this layer provides the target.
		{Name: "etc/mtab", Typeflag: tar.TypeSymlink, Linkname: "/proc/self/mounts", Mode: 0o777, Uid: 0, Gid: 0},
	}, nil)

	if err := extractTarIgnoreChown(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "etc/mtab")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symlink was not created: %v", err)
	}

	buf := make([]byte, 12)
	if _, err := unix.Lgetxattr(link, guestAttrXattr, buf); err != nil {
		t.Fatalf("a dangling symlink got no recorded ownership: %v", err)
	}
	if uid := binary.LittleEndian.Uint32(buf[4:8]); uid != 0 {
		t.Errorf("symlink recorded as uid %d, want 0", uid)
	}
}

// /etc/sudoers is 0440, and setxattr needs write permission on the file. The
// first version of this recorded the attributes after chmod, so every
// read-only file in the image silently got no record -- and sudo, which
// checks sudoers' owner, still refused to run.
func TestUnpackRecordsReadOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	data := attrTarball(t, []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 0, Gid: 0},
		{Name: "etc/sudoers", Typeflag: tar.TypeReg, Mode: 0o440, Uid: 0, Gid: 0},
		// Read-only for its owner and nothing else, as /etc/shadow really is.
		// A mode with no owner-read at all is not covered: the record would be
		// written but hvi could not read it back, and no real image has one --
		// this rootfs has zero such files and three that are merely unwritable.
		{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o400, Uid: 0, Gid: 42},
	}, map[string]string{
		"etc/sudoers": "root ALL=(ALL:ALL) ALL\n",
		"etc/shadow":  "root:!:20000:\n",
	})

	if err := extractTarIgnoreChown(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}

	mode, uid, gid := readGuestAttr(t, filepath.Join(dir, "etc/sudoers"), false)
	if uid != 0 || gid != 0 {
		t.Errorf("sudoers recorded as %d:%d, want 0:0 -- sudo refuses to run otherwise", uid, gid)
	}
	if mode&0o7777 != 0o440 {
		t.Errorf("sudoers mode recorded as %o, want 440", mode&0o7777)
	}
	if _, uid, gid := readGuestAttr(t, filepath.Join(dir, "etc/shadow"), false); uid != 0 || gid != 42 {
		t.Errorf("shadow recorded as %d:%d, want 0:42", uid, gid)
	}

	// The mode still has to end up on the file itself, not only in the record.
	if st, err := os.Stat(filepath.Join(dir, "etc/sudoers")); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o440 {
		t.Errorf("sudoers left as %o on the host, want 440: recording must not cost the chmod", st.Mode().Perm())
	}
}
