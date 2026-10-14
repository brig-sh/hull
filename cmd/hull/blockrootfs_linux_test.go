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

package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/ociclient"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// tarLayer is an uncompressed image layer held in memory, which is all
// UnpackLayers reads from one.
type tarLayer struct{ data []byte }

func (l tarLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.data)), nil
}
func (l tarLayer) Compressed() (io.ReadCloser, error) { return l.Uncompressed() }
func (l tarLayer) Size() (int64, error)               { return int64(len(l.data)), nil }
func (l tarLayer) Digest() (v1.Hash, error)           { return v1.Hash{}, nil }
func (l tarLayer) DiffID() (v1.Hash, error)           { return v1.Hash{}, nil }
func (l tarLayer) MediaType() (types.MediaType, error) {
	return types.DockerUncompressedLayer, nil
}

type layerEntry struct {
	hdr  tar.Header
	body string
}

func buildLayer(t *testing.T, entries ...layerEntry) tarLayer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return tarLayer{data: buf.Bytes()}
}

var (
	debugfsTypeMode = regexp.MustCompile(`Type: (\S+)\s+Mode:\s+(\d+)`)
	debugfsOwner    = regexp.MustCompile(`User:\s+(\d+)\s+Group:\s+(\d+)`)
)

// imageStat reads one inode back out of the finished image, as type, octal
// mode and "uid:gid".
func imageStat(t *testing.T, disk, path string) (fileType, mode, owner string) {
	t.Helper()
	out, err := exec.Command(debugfsBin, "-R", "stat "+path, disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat %s: %s: %v", path, out, err)
	}
	tm := debugfsTypeMode.FindSubmatch(out)
	ow := debugfsOwner.FindSubmatch(out)
	if tm == nil || ow == nil {
		t.Fatalf("debugfs stat %s: unexpected output:\n%s", path, out)
	}
	return string(tm[1]), string(tm[2]), string(ow[1]) + ":" + string(ow[2])
}

// The whole unprivileged Linux path, the one an ordinary user's `hull run`
// takes: a layer unpacked through ociclient, where archive.Apply stops on its
// first chown and the manual extractor records what the host cannot hold,
// then the block image built from that tree. The image has to come out as the
// layer describes it -- owners, modes, file types, symlink owners -- with
// none of hull's host-side records left in it.
//
// The layer carries the cases that broke: Fedora's 0000 shadow files and
// 04111 sudo, which an owner cannot read, so mke2fs could not either; a 0555
// directory, which the owner cannot delete from; symlinks, which cannot hold
// a user.* record on Linux; and an image-supplied /urunit.conf, whose mode and
// owner must not leak onto the injected 0600 one.
func TestUnprivilegedUnpackBuildsTheImageTheLayerDescribes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root unpack chowns for real and records nothing")
	}
	for _, bin := range []string{mke2fsBin, debugfsBin, e2fsckBin} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("e2fsprogs is not installed: %v", err)
		}
	}
	base := t.TempDir()
	rootfs := filepath.Join(base, "rootfs")
	dir := func(name string, mode int64, uid, gid int) layerEntry {
		return layerEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode, Uid: uid, Gid: gid}}
	}
	reg := func(name string, mode int64, uid, gid int, body string) layerEntry {
		return layerEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Uid: uid, Gid: gid}, body: body}
	}
	link := func(name, target string, uid, gid int) layerEntry {
		return layerEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Uid: uid, Gid: gid}}
	}
	layer := buildLayer(t,
		dir("bin/", 0o755, 0, 0),
		reg("bin/busybox", 0o755, 0, 0, "#!/bin/sh\n"),
		link("bin/sh", "busybox", 0, 0),
		dir("etc/", 0o755, 0, 0),
		reg("etc/shadow", 0o640, 0, 42, "root:!::0:::::\n"),
		reg("etc/gshadow", 0o000, 0, 0, "root:::\n"),
		dir("usr/", 0o755, 0, 0),
		dir("usr/bin/", 0o755, 0, 0),
		reg("usr/bin/sudo", 0o4111, 0, 0, "#!/bin/sh\n"),
		dir("usr/lib/", 0o755, 0, 0),
		dir("usr/lib/sealed/", 0o555, 0, 0),
		reg("usr/lib/sealed/f", 0o444, 0, 0, "x"),
		dir("home/", 0o755, 0, 0),
		dir("home/app/", 0o700, 1000, 1000),
		link("home/app/.profile", "/etc/profile", 1000, 1000),
		// 0000, so the host copy is widened and the builder emits a mode line
		// for it -- the line that must not land on the injected file.
		reg("urunit.conf", 0o000, 1000, 1000, "IMAGE=1\n"),
	)
	if err := ociclient.UnpackLayers([]v1.Layer{layer}, rootfs, nil); err != nil {
		t.Fatalf("UnpackLayers: %v", err)
	}
	if _, _, _, ok := ociclient.GuestAttr(filepath.Join(rootfs, "etc/shadow"), false); !ok {
		t.Skipf("%s has no user xattr support, so nothing could be recorded", base)
	}

	disk := filepath.Join(base, "rootfs.ext4")
	injects := []blockInject{{guestPath: "/urunit.conf", content: []byte("SECRET=1\n"), mode: 0o600}}
	if err := buildBlockRootfs(disk, rootfs, injects, 32); err != nil {
		t.Fatalf("buildBlockRootfs: %v", err)
	}

	for _, tc := range []struct{ path, fileType, mode, owner string }{
		{"/bin/busybox", "regular", "0755", "0:0"},
		{"/bin/sh", "symlink", "0777", "0:0"},
		{"/etc/shadow", "regular", "0640", "0:42"},
		{"/etc/gshadow", "regular", "0000", "0:0"},
		{"/usr/bin/sudo", "regular", "04111", "0:0"},
		{"/usr/lib/sealed", "directory", "0555", "0:0"},
		{"/usr/lib/sealed/f", "regular", "0444", "0:0"},
		{"/home/app", "directory", "0700", "1000:1000"},
		{"/home/app/.profile", "symlink", "0777", "1000:1000"},
		{"/urunit.conf", "regular", "0600", "0:0"},
	} {
		fileType, mode, owner := imageStat(t, disk, tc.path)
		if fileType != tc.fileType || mode != tc.mode || owner != tc.owner {
			t.Errorf("%s is a %s %s owned %s in the image, want a %s %s owned %s",
				tc.path, fileType, mode, owner, tc.fileType, tc.mode, tc.owner)
		}
	}

	for _, p := range []string{"/etc/shadow", "/bin/busybox", "/usr/lib/sealed"} {
		out, err := exec.Command(debugfsBin, "-R", "ea_list "+p, disk).CombinedOutput()
		if err != nil {
			t.Fatalf("debugfs ea_list %s: %s: %v", p, out, err)
		}
		if strings.Contains(string(out), "linux-attr") {
			t.Errorf("%s carries hull's host-side record into the image:\n%s", p, out)
		}
	}

	// And the owner can still delete what was unpacked.
	if err := os.RemoveAll(rootfs); err != nil {
		t.Errorf("the unpacked tree cannot be removed by its owner: %v", err)
	}
}
