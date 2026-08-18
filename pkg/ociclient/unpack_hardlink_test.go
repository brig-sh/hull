// Copyright (c) 2023-2026, Nubificus LTD
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

package ociclient

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// These cover the types containerd's pass is no longer allowed to create.
//
// They run the real unpackLayer -- archive.Apply first, then the manual
// extractor -- because the whole point is that the first pass ran before any
// containment existed. A test that drove the manual extractor alone would pass
// against the vulnerable code.
//
// Remove the archive.WithFilter and every test here fails.

// staticLayer serves a tarball from memory as a v1.Layer.
type staticLayer struct{ data []byte }

func (s *staticLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s *staticLayer) Compressed() (io.ReadCloser, error)  { return s.Uncompressed() }
func (s *staticLayer) Size() (int64, error)                { return int64(len(s.data)), nil }
func (s *staticLayer) Digest() (v1.Hash, error)            { return v1.Hash{}, nil }
func (s *staticLayer) DiffID() (v1.Hash, error)            { return v1.Hash{}, nil }
func (s *staticLayer) MediaType() (types.MediaType, error) { return types.DockerLayer, nil }

func hostNlink(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Nlink)
}

// TestHardlinkThroughASymlinkCannotTouchAHostFile is the direct form: a symlink
// to a host file, then a hard link to that symlink. On darwin link(2)
// dereferences, so containerd's unresolved base component gives it the host
// file's inode under a name inside the rootfs, and it applies the entry's mode
// to it.
func TestHardlinkThroughASymlinkCannotTouchAHostFile(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	rootfs, outside := escapeSetup(t)
	victim := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(victim, []byte("ssh-ed25519 AAAA... user@host\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	layer := &staticLayer{data: rawTarball(t,
		tarEntry{hdr: tar.Header{
			Name: "ln", Typeflag: tar.TypeSymlink, Linkname: victim,
			Mode: 0o777, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{
			Name: "stolen", Typeflag: tar.TypeLink, Linkname: "ln",
			Mode: 0o777, Uid: uid, Gid: gid}},
	).Bytes()}

	_ = unpackLayer(layer, rootfs, 0, nil)

	after, err := os.Lstat(victim)
	if err != nil {
		t.Fatalf("the host file is gone: %v", err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("an OCI image changed a host file's mode 0600 -> %04o", after.Mode().Perm())
	}
	if n := hostNlink(t, victim); n != 1 {
		t.Errorf("the host file has %d links; the image linked it into the rootfs", n)
	}
}

// TestHardlinkSourceSplitCannotSetuidAHostFile is the evasion that defeats the
// Lstat guard: hull resolves the link source lexically (path.Clean -> "ln", an
// ordinary decoy) while containerd resolves "sub/.." physically and lands on
// the symlink to the host. The guard inspects a different file than the one
// containerd links, so the layer is accepted -- and the mode carries setuid.
func TestHardlinkSourceSplitCannotSetuidAHostFile(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	rootfs, outside := escapeSetup(t)
	victim := filepath.Join(outside, "helper")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\nid\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	layer := &staticLayer{data: rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "deep/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{Name: "deep/inner/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{Name: "sub", Typeflag: tar.TypeSymlink, Linkname: "deep/inner",
			Mode: 0o777, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{Name: "deep/ln", Typeflag: tar.TypeSymlink, Linkname: victim,
			Mode: 0o777, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{Name: "ln", Typeflag: tar.TypeReg, Mode: 0o644, Uid: uid, Gid: gid},
			body: "decoy"},
		tarEntry{hdr: tar.Header{Name: "stolen", Typeflag: tar.TypeLink, Linkname: "sub/../ln",
			Mode: 0o4755, Uid: uid, Gid: gid}},
	).Bytes()}

	_ = unpackLayer(layer, rootfs, 0, nil)

	after, err := os.Lstat(victim)
	if err != nil {
		t.Fatalf("the host file is gone: %v", err)
	}
	if after.Mode()&os.ModeSetuid != 0 {
		t.Errorf("an OCI image set the setuid bit on a host file (mode now %v)", after.Mode())
	}
	if after.Mode().Perm() != 0o755 {
		t.Errorf("an OCI image changed a host file's mode 0755 -> %04o", after.Mode().Perm())
	}
}

// TestUidMismatchIsNotAFreeGuess covers the reason the uid requirement is not a
// real obstacle: a failed Lchown is classified as a permission error and the
// pull continues to the next layer, so the attacker gets one guess per layer
// and no error is ever reported. Three layers, two wrong uids, one right.
func TestUidMismatchIsNotAFreeGuess(t *testing.T) {
	rootfs, outside := escapeSetup(t)
	victim := filepath.Join(outside, "target")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var layers []v1.Layer
	for i, g := range [][2]int{{0, 0}, {999, 999}, {os.Getuid(), os.Getgid()}} {
		tag := string(rune('a' + i))
		layers = append(layers, &staticLayer{data: rawTarball(t,
			tarEntry{hdr: tar.Header{Name: tag + "/", Typeflag: tar.TypeDir,
				Mode: 0o755, Uid: g[0], Gid: g[1]}},
			tarEntry{hdr: tar.Header{Name: tag + "/ln", Typeflag: tar.TypeSymlink, Linkname: victim,
				Mode: 0o777, Uid: g[0], Gid: g[1]}},
			tarEntry{hdr: tar.Header{Name: tag + "/stolen", Typeflag: tar.TypeLink,
				Linkname: tag + "/ln", Mode: 0o777, Uid: g[0], Gid: g[1]}},
		).Bytes()})
	}

	_ = UnpackLayers(layers, rootfs, nil)

	after, err := os.Lstat(victim)
	if err != nil {
		t.Fatalf("the host file is gone: %v", err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("uid guessing across layers changed a host file's mode to %04o",
			after.Mode().Perm())
	}
}

// TestAFifoInTheImageDoesNotBlockAReader covers the other half of the filter.
// mkfifo needs no privilege, our own extractor skips FIFOs, and containerd's
// pass created them anyway -- so one survived at a path we read, and open(2)
// blocked there forever. os.Root does not help: the block is in the syscall,
// not in the resolution.
func TestAFifoInTheImageDoesNotBlockAReader(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	rootfs, _ := escapeSetup(t)

	layer := &staticLayer{data: rawTarball(t,
		tarEntry{hdr: tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: gid}},
		tarEntry{hdr: tar.Header{Name: "etc/passwd", Typeflag: tar.TypeFifo,
			Mode: 0o644, Uid: uid, Gid: gid}},
	).Bytes()}

	if err := unpackLayer(layer, rootfs, 0, nil); err != nil {
		t.Fatalf("unpackLayer: %v", err)
	}

	if fi, err := os.Lstat(filepath.Join(rootfs, "etc", "passwd")); err == nil {
		if fi.Mode()&os.ModeNamedPipe != 0 {
			t.Errorf("the image planted a FIFO at etc/passwd; it survived extraction")
		}
	}

	// Even if one did survive, no reader may block on it. resolveImageUser is
	// the reader that used to.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = resolveImageUser(rootfs, "root")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolveImageUser blocked on an image-planted FIFO")
	}
}

// TestResolveImageUserCannotReadOutsideTheRootfs covers the unrooted reader:
// etc/passwd planted as a symlink to a host file put that file's sixth field
// into the OCI spec's HOME= and handed it to the guest.
func TestResolveImageUserCannotReadOutsideTheRootfs(t *testing.T) {
	rootfs, outside := escapeSetup(t)
	secret := filepath.Join(outside, "passwd")
	if err := os.WriteFile(secret,
		[]byte("root:x:0:0:root:/EXFILTRATED-HOST-CONTENT:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(rootfs, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}

	_, _, home, _ := resolveImageUser(rootfs, "root")
	if home == "/EXFILTRATED-HOST-CONTENT" {
		t.Errorf("resolveImageUser read a host file through an image-planted symlink: HOME=%q", home)
	}
}
