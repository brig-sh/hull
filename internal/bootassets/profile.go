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

package bootassets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// A machine profile other than hvi's native one can need its own kernel.
// portable-arm64-v1 does: its guest keeps time through the hvi-time device,
// and only a kernel built with that driver ticks on it. Such a kernel is a
// variant of the bundle: it lives in a directory of its own under the asset
// directory, named after the profile, with its own provenance record, and
// the initrd stays the default bundle's. The record is per directory
// (VerifyFiles groups by directory), so the variant kernel and the default
// initrd are each checked against the record that describes them.

// PortableArm64V1 is the machine profile whose guest is the same machine on
// an Apple host (hvi on Hypervisor.framework) and on a Linux arm64 host (hvi
// on KVM), so a checkpoint of it restores on either.
const PortableArm64V1 = "portable-arm64-v1"

// PortableKernelEnv names a local kernel Image for portable-arm64-v1. It is
// how the variant is installed until a published artifact exists: `hull
// assets pull --profile portable-arm64-v1` (or the first run that needs it)
// copies the file into the variant directory and records its sha256.
const PortableKernelEnv = "HULL_PORTABLE_KERNEL"

// PortableKernelRefEnv names an OCI artifact that carries the
// portable-arm64-v1 kernel, fetched with the same checks as the default
// bundle.
const PortableKernelRefEnv = "HULL_PORTABLE_KERNEL_REF"

// ProfileKernelName is the kernel file of a profile variant. The only
// profile with one is arm64, whose kernel is a flat Image.
const ProfileKernelName = "Image"

// hostArch is runtime.GOARCH, a variable so a test on an amd64 runner can
// exercise the arm64 paths.
var hostArch = runtime.GOARCH

// ErrUnknownProfile is a profile name hull has no boot assets for.
var ErrUnknownProfile = errors.New("unknown machine profile")

// NormalizeProfile returns the profile name hull records: "" for hvi's
// native profile, which needs no variant and no flag, and the name itself
// for one it knows.
func NormalizeProfile(name string) (string, error) {
	switch name {
	case "", "native":
		return "", nil
	case PortableArm64V1:
		return name, nil
	}
	return "", fmt.Errorf("%w %q: expected native or %s", ErrUnknownProfile, name, PortableArm64V1)
}

// checkProfile refuses a profile this host has no kernel variant for.
func checkProfile(profile string) error {
	name, err := NormalizeProfile(profile)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("the native profile boots the default boot assets; it has no variant")
	}
	if hostArch != "arm64" {
		return fmt.Errorf("%s is an arm64 machine, and this host is %s", name, hostArch)
	}
	return nil
}

// ProfileDir is the directory a profile's kernel lives in: a directory named
// after the profile inside the asset directory (Dir), so HULL_BOOT_ASSETS
// and BRIG_BOOT_ASSETS move it along with the default bundle.
func ProfileDir(storeDir, profile string) (string, error) {
	if err := checkProfile(profile); err != nil {
		return "", err
	}
	dir, err := Dir(storeDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, profile), nil
}

// ProfilePaths returns the kernel and initrd a profile boots: the profile's
// own kernel and the default bundle's initrd.
func ProfilePaths(storeDir, profile string) (kernel, initrd string, err error) {
	dir, err := ProfileDir(storeDir, profile)
	if err != nil {
		return "", "", err
	}
	if _, initrd, err = Paths(storeDir); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, ProfileKernelName), initrd, nil
}

// ProfileKernelPresent reports whether a profile's kernel is on disk.
func ProfileKernelPresent(storeDir, profile string) bool {
	kernel, _, err := ProfilePaths(storeDir, profile)
	if err != nil {
		return false
	}
	info, err := os.Stat(kernel)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// ProfileSource is where a profile's kernel comes from when hull has to
// install it: a local file (PortableKernelEnv) or an OCI reference
// (PortableKernelRefEnv). Both empty means nothing is configured; both set
// is refused, since the two would install different kernels.
func ProfileSource(profile string) (file, ref string, err error) {
	if err := checkProfile(profile); err != nil {
		return "", "", err
	}
	file, ref = os.Getenv(PortableKernelEnv), os.Getenv(PortableKernelRefEnv)
	if file != "" && ref != "" {
		return "", "", fmt.Errorf("both %s and %s are set; set one, so it is clear which kernel "+
			"%s boots", PortableKernelEnv, PortableKernelRefEnv, profile)
	}
	return file, ref, nil
}

// errNoProfileSource is the remedy when nothing says where the kernel is.
func errNoProfileSource(profile string) error {
	return fmt.Errorf("no %s kernel is installed, and none is published yet: set %s to a kernel "+
		"Image built with the hvi-time driver (or %s to an artifact that carries one) and run "+
		"`hull assets pull --profile %s`", profile, PortableKernelEnv, PortableKernelRefEnv, profile)
}

// InstallProfile installs a profile's kernel from its configured source
// (ProfileSource), replacing the one there.
func InstallProfile(ctx context.Context, storeDir, profile string) error {
	file, ref, err := ProfileSource(profile)
	if err != nil {
		return err
	}
	switch {
	case file != "":
		return ImportProfileKernel(storeDir, profile, file)
	case ref != "":
		return FetchProfile(ctx, ref, storeDir, profile)
	}
	return errNoProfileSource(profile)
}

// FetchProfile downloads an OCI artifact carrying a profile's kernel into
// the profile's directory. It goes through the same reference check,
// signature check and record as the default bundle (Fetch), and the artifact
// must carry a layer titled ProfileKernelName.
func FetchProfile(ctx context.Context, ref, storeDir, profile string) error {
	dir, err := ProfileDir(storeDir, profile)
	if err != nil {
		return err
	}
	if err := fetchInto(ctx, ref, dir); err != nil {
		return err
	}
	rec, err := readProvenance(dir)
	if err != nil {
		return err
	}
	if _, ok := rec.Files[ProfileKernelName]; !ok {
		return fmt.Errorf("%s carries no %s layer, so it holds no %s kernel", ref, ProfileKernelName, profile)
	}
	return nil
}

// ImportProfileKernel copies a local kernel into a profile's directory and
// records it. The record's Ref is the file's absolute path as a file: URL and
// its Digest the file's sha256; VerifiedDigest stays empty, because no
// signature covers a local build. That is why HULL_VERIFY=require refuses an
// import: it accepts only what a signature proved.
//
// The kernel is hashed as it is copied, the way Fetch hashes a layer on its
// way to disk, so the record describes the bytes that landed.
func ImportProfileKernel(storeDir, profile, path string) error {
	dir, err := ProfileDir(storeDir, profile)
	if err != nil {
		return err
	}
	if VerifyModeFromEnv() == VerifyRequire {
		return fmt.Errorf("%s=require, and a local kernel (%s=%s) carries no signature to verify",
			VerifyModeEnv, PortableKernelEnv, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	src, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("open the %s kernel: %w", profile, err)
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("%s is not a kernel Image: it is empty or not a regular file", abs)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	var rec File
	if err := writeFile(filepath.Join(dir, ProfileKernelName), src, bundleFileMode, &rec); err != nil {
		return err
	}
	log.Warnf("Installed %s as the %s kernel; it is a local file, so no signature vouches for it "+
		"(sha256:%s)", abs, profile, rec.SHA256)
	return writeProvenance(dir, "file://"+abs, "sha256:"+rec.SHA256, "",
		map[string]File{ProfileKernelName: rec})
}

// EnsureProfile returns the kernel and initrd a profile boots, installing
// the kernel from its configured source first if it is not on disk.
//
// The default initrd comes from Ensure, so it is fetched and checked exactly
// as for a default boot. The kernel already on disk is checked against its
// record; like Ensure it is never replaced unasked, so a changed
// HULL_PORTABLE_KERNEL file is picked up by `hull assets pull --profile
// ... --force`, and a run whose configured source differs from the one
// recorded says so.
func EnsureProfile(ctx context.Context, storeDir, profile string) (kernel, initrd string, err error) {
	kernel, initrd, err = ProfilePaths(storeDir, profile)
	if err != nil {
		return "", "", err
	}
	if _, _, err := Ensure(ctx, storeDir); err != nil {
		return "", "", err
	}
	if ProfileKernelPresent(storeDir, profile) {
		err := VerifyFiles(kernel)
		switch {
		case err == nil:
			warnIfSourceMoved(storeDir, profile)
			return kernel, initrd, nil
		case !errors.Is(err, ErrNoProvenance):
			return "", "", err
		}
		log.Warnf("The %s kernel in %s has no provenance record; installing it again from its "+
			"configured source", profile, filepath.Dir(kernel))
	}
	if err := InstallProfile(ctx, storeDir, profile); err != nil {
		return "", "", err
	}
	if !ProfileKernelPresent(storeDir, profile) {
		return "", "", fmt.Errorf("the %s kernel is still missing after installing it: expected %s",
			profile, kernel)
	}
	return kernel, initrd, nil
}

// warnIfSourceMoved says when the kernel on disk came from somewhere other
// than the source configured now: the run boots the recorded one.
func warnIfSourceMoved(storeDir, profile string) {
	file, ref, err := ProfileSource(profile)
	if err != nil || (file == "" && ref == "") {
		return
	}
	dir, err := ProfileDir(storeDir, profile)
	if err != nil {
		return
	}
	rec, err := readProvenance(dir)
	if err != nil {
		return
	}
	want := ref
	if file != "" {
		if abs, err := filepath.Abs(file); err == nil {
			want = "file://" + abs
		}
	}
	if rec.Ref != want {
		log.Warnf("The %s kernel installed in %s came from %s, not from %s; booting the installed "+
			"one (`hull assets pull --profile %s --force` replaces it)", profile, dir, rec.Ref, want, profile)
	}
}

// ReinstallCommand is the command that puts the files of an asset directory
// back: `hull assets pull --force` for the bundle, and `hull assets pull
// --profile NAME --force` for a profile's directory, which the plain one
// leaves alone. A profile's directory is recognised by its name, which
// ProfileDir makes the profile's.
func ReinstallCommand(dir string) string {
	if profile, err := NormalizeProfile(filepath.Base(dir)); err == nil && profile != "" {
		return fmt.Sprintf("`hull assets pull --profile %s --force`", profile)
	}
	return "`hull assets pull --force`"
}

// refetchHint is the remedy a failed check of dir names: the command, and
// for a profile's kernel where that command takes it from now.
func refetchHint(dir string, rec Provenance) string {
	profile, err := NormalizeProfile(filepath.Base(dir))
	if err != nil || profile == "" {
		return "Re-fetch with `hull assets pull --force`"
	}
	return fmt.Sprintf("Install it again with %s, which takes it from %s,", ReinstallCommand(dir),
		describeProfileSource(profile, rec.Ref))
}

// describeProfileSource names the source `hull assets pull --profile`
// would install from now, and says so when none is configured.
func describeProfileSource(profile, recorded string) string {
	file, ref, err := ProfileSource(profile)
	switch {
	case err != nil:
		return fmt.Sprintf("one of %s and %s (both are set now; unset one)", PortableKernelEnv,
			PortableKernelRefEnv)
	case file != "":
		return fmt.Sprintf("%s=%s", PortableKernelEnv, file)
	case ref != "":
		return fmt.Sprintf("%s=%s", PortableKernelRefEnv, ref)
	}
	return fmt.Sprintf("%s or %s (neither is set now; the installed kernel came from %s)",
		PortableKernelEnv, PortableKernelRefEnv, recorded)
}

// ProfileRecord returns a profile's provenance record.
func ProfileRecord(storeDir, profile string) (Provenance, error) {
	dir, err := ProfileDir(storeDir, profile)
	if err != nil {
		return Provenance{}, err
	}
	return readProvenance(dir)
}
