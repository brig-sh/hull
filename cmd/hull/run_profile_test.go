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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
)

// These tests run `hull run` itself, up to the exec of a fake hvi that
// records its command line, so they check the call site that puts the
// profile on the hvi argv, in state.json and in the kernel staged, not only
// the helpers it calls.

const (
	runNativeKernel   = "the default bundle's kernel"
	runPortableKernel = "the portable profile's kernel"
)

// runProfileEnv is a store whose asset directory holds a default bundle and
// a portable kernel, each with its record, so nothing is fetched; a local
// OCI bundle to run; and a fake hvi first on PATH. controlFlags says whether
// that hvi takes the control flags. It returns the store directory, the
// bundle and the file the fake hvi writes its argv to.
func runProfileEnv(t *testing.T, controlFlags bool) (storeDir, bundle, argvFile string) {
	t.Helper()
	if _, err := os.Stat(mke2fsBin); err != nil {
		t.Skipf("the block rootfs needs e2fsprogs: %v", err)
	}
	for _, k := range []string{"HULL_BOOT_ASSETS", "BRIG_BOOT_ASSETS", bootassets.PortableKernelEnv,
		bootassets.PortableKernelRefEnv, bootassets.VerifyModeEnv} {
		t.Setenv(k, "")
	}
	t.Setenv("DO_NOT_TRACK", "1")
	storeDir = shortSocketDir(t)
	// The macOS store marker, so the run takes /tmp for a store hull
	// mounted rather than creating a disk image; Linux ignores the file.
	if err := os.WriteFile(filepath.Join(storeDir, ".case-sensitive-apfs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(storeDir); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(storeDir, "assets")
	seedAssetDir(t, assets, map[string]string{
		bootassets.KernelName(): runNativeKernel,
		bootassets.InitrdName:   "initrd",
	})
	seedAssetDir(t, filepath.Join(assets, bootassets.PortableArm64V1), map[string]string{
		bootassets.ProfileKernelName: runPortableKernel,
	})

	bundle = filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "rootfs", "bin", "sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{"ociVersion":"1.0.2","process":{"args":["/bin/sh"],"env":["PATH=/bin"],"cwd":"/"},` +
		`"root":{"path":"rootfs"},"annotations":{}}`
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	argvFile = filepath.Join(bin, "argv")
	checkpointRoot := `echo "error: --checkpoint-root needs a directory" >&2; exit 2`
	if !controlFlags {
		checkpointRoot = `echo 'error: unknown boot arg "--checkpoint-root"' >&2; exit 2`
	}
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"\"boot --checkpoint-root\") " + checkpointRoot + " ;;\n" +
		"\"boot --profile\") echo 'error: --profile needs a name' >&2; exit 2 ;;\n" +
		"\"boot --disk-written\") if [ -f '" + filepath.Join(bin, "knows-base") + "' ]; then " +
		"echo 'error: --disk-written needs a file' >&2; else " +
		"echo 'error: unknown boot arg \"--disk-written\"' >&2; fi; exit 2 ;;\n" +
		"esac\n" +
		"printf '%s\\n' \"$@\" > '" + argvFile + ".tmp' && mv '" + argvFile + ".tmp' '" + argvFile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "hvi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return storeDir, bundle, argvFile
}

// seedAssetDir writes files and the provenance record a fetch would write
// beside them.
func seedAssetDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{"ref": "file:///test", "digest": "sha256:test"}
	entries := map[string]any{}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		entries[name] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(body)}
	}
	rec["files"] = entries
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// hullRun runs `hull --store-dir STORE run --detach ...` in this process.
func hullRun(t *testing.T, storeDir string, args ...string) error {
	t.Helper()
	app := &cli.Command{
		Name:     "hull",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir", Value: storeDir}},
		Commands: []*cli.Command{runCommand()},
	}
	argv := append([]string{"hull", "run", "--detach", "--hypervisor", "hvi", "--rootfs-type", "block",
		"--net", "none"}, args...)
	return app.Run(context.Background(), argv)
}

// recordedArgv waits for the fake hvi to write its command line, which it
// renames into place whole.
func recordedArgv(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake hvi never ran: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stagedKernel(t *testing.T, storeDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storeDir, "instances", name, stagedHostKernelName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func instanceState(t *testing.T, storeDir, name string) *store.InstanceState {
	t.Helper()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	return mustInstance(t, s, name)
}

func requireArm64(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("portable-arm64-v1 runs on arm64 hosts only")
	}
}

// A portable run passes --profile to hvi after the control flags, records
// the profile, and boots the profile's kernel; a native run does none of
// it, so its command line is the one an hvi from before profiles takes.
func TestRunWiresTheProfile(t *testing.T) {
	requireArm64(t)
	storeDir, bundle, argvFile := runProfileEnv(t, true)

	if err := hullRun(t, storeDir, "--name", "port", "--profile", bootassets.PortableArm64V1, bundle); err != nil {
		t.Fatal(err)
	}
	argv := recordedArgv(t, argvFile)
	i := slices.Index(argv, "--profile")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != bootassets.PortableArm64V1 {
		t.Fatalf("the portable run's hvi argv has no --profile %s: %v", bootassets.PortableArm64V1, argv)
	}
	if !slices.Contains(argv, "--control-sock") {
		t.Fatalf("the portable run's hvi argv has no --control-sock: %v", argv)
	}
	state := instanceState(t, storeDir, "port")
	if state.Profile != bootassets.PortableArm64V1 {
		t.Fatalf("state.json profile = %q", state.Profile)
	}
	if got := stagedKernel(t, storeDir, "port"); got != runPortableKernel {
		t.Fatalf("the portable run staged %q, not the profile's kernel", got)
	}

	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	if err := hullRun(t, storeDir, "--name", "nat", bundle); err != nil {
		t.Fatal(err)
	}
	argv = recordedArgv(t, argvFile)
	if slices.Contains(argv, "--profile") {
		t.Fatalf("the native run's hvi argv has --profile: %v", argv)
	}
	state = instanceState(t, storeDir, "nat")
	if state.Profile != "" {
		t.Fatalf("the native run recorded profile %q", state.Profile)
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "instances", "nat", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"profile"`) {
		t.Fatalf("the native run's state.json names a profile: %s", raw)
	}
	if got := stagedKernel(t, storeDir, "nat"); got != runNativeKernel {
		t.Fatalf("the native run staged %q, not the default kernel", got)
	}
}

// A profile boots its own kernel however the run arrives at one. A
// bootKernel annotation is checked by its bytes: the installed kernel, or a
// copy of it elsewhere (brig's BRIG_PORTABLE_KERNEL), boots; the default
// bundle's kernel is refused, as is a run whose kernel comes from the image.
// Each refusal comes before the disk is built and leaves no instance.
func TestRunRefusesAKernelThatIsNotTheProfiles(t *testing.T) {
	requireArm64(t)
	storeDir, bundle, argvFile := runProfileEnv(t, true)
	assets := filepath.Join(storeDir, "assets")
	initrd := "com.urunc.unikernel.bootInitrd=" + filepath.Join(assets, bootassets.InitrdName)
	copyOf := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(copyOf, []byte(runPortableKernel), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := []string{"--profile", bootassets.PortableArm64V1}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default", []string{"--annotation", "com.urunc.unikernel.bootKernel=" +
			filepath.Join(assets, bootassets.KernelName()), "--annotation", initrd},
			"is not the " + bootassets.PortableArm64V1 + " kernel"},
		{"noassets", []string{"--no-boot-assets"}, "--no-boot-assets"},
	} {
		err := hullRun(t, storeDir, append(append([]string{"--name", tc.name}, profile...),
			append(tc.args, bundle)...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", tc.name, err, tc.want)
		}
		if _, statErr := os.Stat(filepath.Join(storeDir, "instances", tc.name)); !os.IsNotExist(statErr) {
			t.Errorf("%s: the refused run left its instance directory: %v", tc.name, statErr)
		}
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Fatalf("a refused run started hvi: %v", err)
	}

	for _, tc := range []struct{ name, kernel string }{
		{"installed", filepath.Join(assets, bootassets.PortableArm64V1, bootassets.ProfileKernelName)},
		{"copy", copyOf},
	} {
		_ = os.Remove(argvFile)
		err := hullRun(t, storeDir, append(append([]string{"--name", tc.name}, profile...),
			"--annotation", "com.urunc.unikernel.bootKernel="+tc.kernel, "--annotation", initrd, bundle)...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if argv := recordedArgv(t, argvFile); !slices.Contains(argv, bootassets.PortableArm64V1) {
			t.Fatalf("%s: the hvi argv has no profile: %v", tc.name, argv)
		}
		if got := stagedKernel(t, storeDir, tc.name); got != runPortableKernel {
			t.Fatalf("%s: staged %q", tc.name, got)
		}
	}
}

// A portable run on an hvi that takes --profile but not the control flags
// is refused: it would be a portable instance that can never be
// checkpointed or moved.
func TestRunRefusesAProfileWithoutTheControlSocket(t *testing.T) {
	requireArm64(t)
	storeDir, bundle, argvFile := runProfileEnv(t, false)
	err := hullRun(t, storeDir, "--name", "nosock", "--profile", bootassets.PortableArm64V1, bundle)
	if err == nil || !strings.Contains(err.Error(), "--control-sock") {
		t.Fatalf("err = %v, want the refusal naming the control socket", err)
	}
	if _, statErr := os.Stat(filepath.Join(storeDir, "instances", "nosock")); !os.IsNotExist(statErr) {
		t.Fatalf("the refused run left its instance directory: %v", statErr)
	}
	if _, statErr := os.Stat(argvFile); !os.IsNotExist(statErr) {
		t.Fatal("the refused run started hvi")
	}
	// A native run on the same hvi still boots, without the socket.
	if err := hullRun(t, storeDir, "--name", "nat", bundle); err != nil {
		t.Fatal(err)
	}
	if argv := recordedArgv(t, argvFile); slices.Contains(argv, "--control-sock") {
		t.Fatalf("the native run passed --control-sock to an hvi that takes none: %v", argv)
	}
}

// `assets pull --profile` without --force does not call a kernel that fails
// its record "already present": it says what is wrong and names the pull
// that replaces it.
func TestAssetsPullProfileReportsATamperedKernel(t *testing.T) {
	requireArm64(t)
	storeDir, _, _ := runProfileEnv(t, true)
	kernel := filepath.Join(storeDir, "assets", bootassets.PortableArm64V1, bootassets.ProfileKernelName)
	if err := os.WriteFile(kernel, []byte("something else"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &cli.Command{
		Name:     "hull",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir", Value: storeDir}},
		Commands: []*cli.Command{assetsCommand()},
	}
	err := app.Run(context.Background(), []string{"hull", "assets", "pull", "--profile", bootassets.PortableArm64V1})
	if err == nil || !strings.Contains(err.Error(), "`hull assets pull --profile "+bootassets.PortableArm64V1+" --force`") {
		t.Fatalf("err = %v, want the mismatch and the profile's pull", err)
	}
}

// A run on a block rootfs starts its disk from the image's base, kept under
// the store's bases by its digest, and records it. An hvi that takes
// --disk-written is booted against the base: its digest, and the bitmap of
// the blocks the run wrote over it, so hvi's blocks written since boot are
// every block that differs from the base. One that does not is given
// neither.
func TestRunKeepsTheBase(t *testing.T) {
	storeDir, bundle, argvFile := runProfileEnv(t, true)
	if err := hullRun(t, storeDir, "--name", "b0", bundle); err != nil {
		t.Fatal(err)
	}
	if argv := recordedArgv(t, argvFile); slices.Contains(argv, hviDiskBaseFlag) ||
		slices.Contains(argv, hviDiskWrittenFlag) {
		t.Fatalf("an hvi without %s was given the base: %v", hviDiskWrittenFlag, argv)
	}
	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(argvFile), "knows-base"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hullRun(t, storeDir, "--name", "b1", bundle); err != nil {
		t.Fatal(err)
	}
	argv := recordedArgv(t, argvFile)
	state := instanceState(t, storeDir, "b1")
	if state.Base == nil {
		t.Fatalf("state.json's base: %+v", state.Base)
	}
	if got, _ := flagValue(argv, hviDiskBaseFlag); got != state.Base.SHA256 {
		t.Fatalf("hvi was given base %q, the record says %s: %v", got, state.Base.SHA256, argv)
	}
	written, ok := flagValue(argv, hviDiskWrittenFlag)
	if !ok {
		t.Fatalf("hvi was not given %s: %v", hviDiskWrittenFlag, argv)
	}
	disk0 := filepath.Join(storeDir, "instances", "b1", "bundle", "rootfs.ext4")
	info, err := os.Stat(disk0)
	if err != nil {
		t.Fatal(err)
	}
	bits, err := os.ReadFile(written)
	if err != nil || int64(len(bits)) != (diskBlocks(info.Size())+7)/8 {
		t.Fatalf("the written bitmap: %d bytes for %d blocks, %v", len(bits), diskBlocks(info.Size()), err)
	}
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(storeDir, "instances", "b1", "bundle", "rootfs.ext4")
	sum, _, err := diskDigestFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	// This image has nothing of the instance's to write in.
	if sum != state.Base.SHA256 || !baseHeld(s, sum) {
		t.Fatalf("base %s recorded, the disk hashes %s, held %v", state.Base.SHA256, sum, baseHeld(s, sum))
	}
	if !isZero(bits) {
		t.Errorf("the written bitmap sets blocks of a disk that is its base")
	}

	// The restore command line drops both: a restored disk has moved on
	// from the base, and disk.json carries what hvi recorded.
	if _, err := hviRestoreArgv(argv[:0:0], "", "", ""); err == nil {
		t.Fatal("an empty command line mapped")
	}
	full := append([]string{"hvi"}, argv...)
	restore, err := hviRestoreArgv(full, "/b", "/c", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(restore, hviDiskBaseFlag) || slices.Contains(restore, hviDiskWrittenFlag) {
		t.Fatalf("the restore argv carries the base: %v", restore)
	}
}
