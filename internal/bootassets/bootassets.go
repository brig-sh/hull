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

// Package bootassets finds, and if necessary downloads, the kernel and initrd
// that boot an OCI image which was never built to be a guest.
//
// An ordinary image carries no kernel, so the runtime has to supply one, and it
// takes the pair as host paths in the bootKernel/bootInitrd annotations rather
// than from the image's own metadata -- an image must not be able to nominate a
// file on the host. That makes finding them hull's problem, which is what this
// package is.
//
// The assets are built and published by NOFireAI/hull-assets, one OCI artifact
// per host platform, with each file carried as its own layer under an
// org.opencontainers.image.title annotation. There is no multi-platform index:
// an OCI artifact has an empty config, so index descriptors come out without
// platform information and a platform-matching client resolves nothing. The
// platform is in the tag instead.
//
// The on-disk layout matches what brig looks for, so a host that has run either
// runtime has already seeded the other.
package bootassets

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("subsystem", "bootassets")

const (
	// InitrdName is the same on every platform: the initrd is a cpio built for
	// the guest, not for the host running hull.
	InitrdName = "container-initrd"

	// titleAnnotation is how a layer says which file it is. oras sets it from
	// the file name on push, so the bundle is self-describing and a consumer
	// never has to unpack a tar to find out what it got.
	titleAnnotation = "org.opencontainers.image.title"

	// DefaultRepo is where the published bundles live. The tag is appended per
	// platform by Ref.
	DefaultRepo = "ghcr.io/nofireai/hull-assets"
)

// KernelName is not platform-independent. A Linux kernel is a bzImage on
// x86_64 and a flat Image on arm64, and the guest architecture follows the
// host: an arm64 Mac boots an arm64 guest.
func KernelName() string {
	if runtime.GOARCH == "amd64" {
		return "bzImage"
	}
	return "Image"
}

// Dir is the directory the assets live in, for a given store.
//
// They live under the store rather than beside it, so that --store-dir is a
// complete isolation boundary. It used to isolate images and instances while
// the assets stayed at a fixed path in $HOME, which meant a run with its own
// store still booted whatever kernel the last `hull assets pull` happened to
// leave in the shared directory -- so an unrelated command in another shell
// could change what a CI job booted, which is exactly what happened.
//
// HULL_BOOT_ASSETS still points at a local build and wins over everything.
// BRIG_BOOT_ASSETS is honoured too, so a machine that already has brig pointed
// somewhere does not need the same path spelled twice.
func Dir(storeDir string) (string, error) {
	if d := os.Getenv("HULL_BOOT_ASSETS"); d != "" {
		return d, nil
	}
	if d := os.Getenv("BRIG_BOOT_ASSETS"); d != "" {
		return d, nil
	}
	if storeDir != "" {
		return filepath.Join(storeDir, "assets"), nil
	}
	// No store to hang them off: fall back to the per-platform default, which
	// is what brig looks for when it is driving something other than hull.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to find the boot assets in: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, ".hull", "assets"), nil
	}
	share := os.Getenv("XDG_DATA_HOME")
	if share == "" {
		share = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(share, "brig", "assets"), nil
}

// Ref is the registry reference for this host's bundle.
//
// HULL_BOOT_ASSETS_REF overrides it whole, which is how you pin a version
// (…:v0.1.0-darwin-arm64) or point at a staging registry.
func Ref() string {
	if r := os.Getenv("HULL_BOOT_ASSETS_REF"); r != "" {
		return r
	}
	return fmt.Sprintf("%s:%s-%s", DefaultRepo, runtime.GOOS, runtime.GOARCH)
}

// Paths returns where the kernel and initrd would be, whether or not they are
// there yet.
func Paths(storeDir string) (kernel, initrd string, err error) {
	dir, err := Dir(storeDir)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, KernelName()), filepath.Join(dir, InitrdName), nil
}

// Present reports whether both files are already on disk.
func Present(storeDir string) bool {
	kernel, initrd, err := Paths(storeDir)
	if err != nil {
		return false
	}
	for _, p := range []string{kernel, initrd} {
		if info, err := os.Stat(p); err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}

// Ensure returns the kernel and initrd, downloading the bundle first if they
// are not already on disk.
//
// It never re-downloads over an existing pair. The assets are large and the
// common case is that they are already there; a host that wants a newer bundle
// asks for one with Fetch.
func Ensure(ctx context.Context, storeDir string) (kernel, initrd string, err error) {
	kernel, initrd, err = Paths(storeDir)
	if err != nil {
		return "", "", err
	}
	if Present(storeDir) {
		// The files are there, which used to be the whole test. It is not
		// enough: the asset directory is user-writable and shared between hull
		// and brig, so "a kernel is present" says nothing about whether it is
		// the kernel that was fetched. Hash it against the record instead.
		switch err := VerifyCached(storeDir); {
		case err == nil:
			return kernel, initrd, nil
		case errors.Is(err, ErrNoProvenance):
			// A cache from a hull that predates the record. We cannot vouch for
			// it, and silently booting it would keep the old behaviour, so the
			// assets are fetched again to establish provenance. This costs one
			// download, once, per existing installation.
			log.Warnf("Cached boot assets in the store have no provenance record; " +
				"re-fetching so their contents can be verified from now on")
		default:
			// A record that disagrees with the bytes beside it is the case this
			// exists for. Re-fetching would paper over it, so stop and make the
			// operator look, which is also the only way they learn that
			// something on the host rewrote a kernel.
			return "", "", err
		}
	}
	if err := Fetch(ctx, Ref(), storeDir); err != nil {
		return "", "", err
	}
	if !Present(storeDir) {
		return "", "", fmt.Errorf("boot assets are still missing after fetching %s: "+
			"expected %s and %s", Ref(), kernel, initrd)
	}
	return kernel, initrd, nil
}

// Fetch downloads a bundle into the asset directory, overwriting what is there.
func Fetch(ctx context.Context, ref, storeDir string) error {
	dir, err := Dir(storeDir)
	if err != nil {
		return err
	}
	// Decide whether this is a reference we are willing to take a kernel from
	// before any bytes move, rather than after they are on disk.
	warning, err := checkRef(ref)
	if err != nil {
		return err
	}
	if warning != "" {
		log.Warnf("%s", warning)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create boot asset directory %s: %w", dir, err)
	}

	img, err := crane.Pull(ref, registryTransport(), crane.WithContext(ctx), registryAuth())
	if err != nil && strings.Contains(err.Error(), "error getting credentials") {
		// Docker's credential helper wants an unlocked login keychain, and a
		// headless session -- a CI runner, an ssh shell -- does not have one.
		// It fails with "the current session does not allow user interaction",
		// which is about the helper, not about the registry. A public bundle
		// must not depend on that, so try again with no credentials at all.
		img, err = crane.Pull(ref, registryTransport(), crane.WithContext(ctx),
			crane.WithAuth(authn.Anonymous))
	}
	if err != nil {
		return fmt.Errorf("pull boot assets from %s: %w (if the package is private, set "+
			"HULL_REGISTRY_TOKEN to a token with read:packages, or `docker login ghcr.io`; "+
			"or point HULL_BOOT_ASSETS at a local build)", ref, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return fmt.Errorf("read boot asset manifest from %s: %w", ref, err)
	}

	// The manifest digest is what a signature would be made over, so it is
	// recorded now while the bundle is resolved, rather than left to a later
	// step that would have to re-resolve a tag which may since have moved.
	bundleDigest := ""
	if d, derr := img.Digest(); derr == nil {
		bundleDigest = d.String()
	}

	wrote := 0
	files := make(map[string]File)
	for _, desc := range manifest.Layers {
		name := desc.Annotations[titleAnnotation]
		// A layer with no title is not addressable, and a title with a path
		// separator in it is a bundle trying to write outside the asset
		// directory. Neither is worth guessing about.
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			continue
		}
		layer, err := img.LayerByDigest(desc.Digest)
		if err != nil {
			return fmt.Errorf("read layer %s from %s: %w", name, ref, err)
		}
		// Compressed() is the blob exactly as stored. Uncompressed() would try
		// to gunzip it, and these layers are plain files with their own media
		// types -- notably container-initrd, which must stay uncompressed for
		// the runtime to append to it.
		rc, err := layer.Compressed()
		if err != nil {
			return fmt.Errorf("open layer %s from %s: %w", name, ref, err)
		}
		var rec File
		err = writeFile(filepath.Join(dir, name), rc, bundleFileMode, &rec)
		_ = rc.Close()
		if err != nil {
			return err
		}
		files[name] = rec
		wrote++
	}
	if wrote == 0 {
		return fmt.Errorf("%s carries no titled layers; it does not look like a boot asset bundle", ref)
	}
	// Written last, so an interrupted fetch leaves no record rather than one
	// describing files that are not all there.
	if err := writeProvenance(dir, ref, bundleDigest, files); err != nil {
		return err
	}
	return nil
}

// bundleFileMode is the permission every fetched file gets.
//
// Nothing in the bundle is executed on the host: the kernel and the initrd are
// data handed to a VMM, and the guest binaries are inside the initrd rather
// than beside it. So none of it needs an executable bit, and not setting one
// keeps a downloaded blob from being runnable by accident.
const bundleFileMode os.FileMode = 0o644

// writeFile writes through a temporary file in the same directory and renames
// it into place. A hull run that starts while a fetch is in flight then either
// sees the old file or the new one, never a half-written kernel.
//
// When rec is non-nil it is filled in with the sha256 and length of what was
// written. The hash is taken from the bytes on their way to disk rather than by
// reading the file back afterwards, so what gets recorded is what was received
// -- re-reading would hash whatever happened to be at the path by then.
func writeFile(path string, r io.Reader, perm os.FileMode, rec *File) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	var src io.Reader = r
	h := sha256.New()
	if rec != nil {
		src = io.TeeReader(r, h)
	}
	n, err := io.Copy(tmp, src)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if rec != nil {
		rec.SHA256 = hex.EncodeToString(h.Sum(nil))
		rec.Size = n
	}
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// registryAuth decides how to authenticate to the registry.
//
// HULL_REGISTRY_TOKEN comes first because it is the only thing that works
// where the keychain does not: a CI runner, or any headless session, where
// Docker's credential helper cannot prompt. GHCR takes a token with
// read:packages as the password against any username.
//
// Otherwise the usual keychain, so a developer who has run `docker login`
// needs to do nothing.
func registryAuth() crane.Option {
	if token := os.Getenv("HULL_REGISTRY_TOKEN"); token != "" {
		return crane.WithAuth(&authn.Basic{Username: "oauth2", Password: token})
	}
	return crane.WithAuthFromKeychain(authn.DefaultKeychain)
}

// registryTransport pins registry traffic to HTTP/1.1, for the same reason
// pkg/ociclient does: ghcr resets HTTP/2 streams part-way through large blobs
// often enough to matter, and Go surfaces that as a fatal read error with no
// retry. The kernel is tens of megabytes, so it is squarely in range.
func registryTransport() crane.Option {
	t := remote.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	return crane.WithTransport(t)
}

// ErrNotPresent is returned by Check when the assets are not on disk. It exists
// so a caller can tell "no assets" apart from "the registry is unreachable".
var ErrNotPresent = errors.New("boot assets are not present")

// Check returns the paths if both files are on disk, and ErrNotPresent if not.
// Nothing is downloaded.
func Check(storeDir string) (kernel, initrd string, err error) {
	kernel, initrd, err = Paths(storeDir)
	if err != nil {
		return "", "", err
	}
	if !Present(storeDir) {
		return "", "", fmt.Errorf("%w: expected %s and %s", ErrNotPresent, kernel, initrd)
	}
	return kernel, initrd, nil
}
