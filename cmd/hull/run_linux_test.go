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

//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// The refusals come before hull dials containerd, so the address names no
// daemon.
func TestRunRefusesBeforeDialling(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"busybox"}, "needs --detach"},
		{[]string{"-d", "--net", "shared", "busybox"}, "--net shared is not supported on Linux"},
		{[]string{"-d", "--pull", "sometimes", "busybox"}, `invalid --pull "sometimes"`},
		{[]string{"-d", "--hypervisor", "vz", "busybox"}, "--hypervisor vz: macOS only"},
		{[]string{"-d", "--hypervisor", "hvi", "busybox"}, "--hypervisor hvi: macOS only"},
		{[]string{"-d", "--hypervisor", "kvm", "busybox"}, "urunc has no such monitor"},
		{[]string{"-d", "--annotation", "novalue", "busybox"}, `invalid --annotation "novalue"`},
		{[]string{"-d", "--name", "../x", "busybox"}, "--name: invalid instance name"},
		{[]string{"-d"}, "image reference required"},
	} {
		app := newApp()
		app.Writer = &bytes.Buffer{}
		argv := append([]string{"hull", "--containerd-address", "/nonexistent.sock", "run"}, tc.args...)
		err := app.Run(context.Background(), argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run %v: error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// privateLogDirs points the instance logs of the user running the test, root
// or not, at a directory of the test's own.
func privateLogDirs(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := rootLogDir
	rootLogDir = t.TempDir()
	t.Cleanup(func() { rootLogDir = old })
}

// A namespace or an id that is not a single path element gives no path,
// whatever the caller checked before.
func TestInstanceLogPathRefusesPathElements(t *testing.T) {
	for _, tc := range []struct{ namespace, id string }{
		{"../x", "vm"},
		{"a/b", "vm"},
		{"..", "vm"},
		{"", "vm"},
		{"ns", "../vm"},
		{"ns", "a/b"},
		{"ns", ""},
	} {
		got, err := instanceLogPath(tc.namespace, tc.id)
		if err == nil {
			t.Errorf("instanceLogPath(%q, %q) = %q, want an error", tc.namespace, tc.id, got)
		}
	}
}

func TestInstanceLogPath(t *testing.T) {
	if os.Geteuid() == 0 {
		old := rootLogDir
		rootLogDir = "/srv/log/hull"
		t.Cleanup(func() { rootLogDir = old })
		t.Setenv("XDG_STATE_HOME", "/srv/state")
		got, err := instanceLogPath("ns", "vm")
		if err != nil || got != "/srv/log/hull/ns/vm.log" {
			t.Errorf("as root: instanceLogPath = %q, %v; want it under rootLogDir, whatever XDG_STATE_HOME says", got, err)
		}
		return
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct {
		xdg  string
		want string
	}{
		{"", filepath.Join(home, ".local", "state", "hull", "ns", "vm.log")},
		{"relative/state", filepath.Join(home, ".local", "state", "hull", "ns", "vm.log")},
		{"/srv/state", "/srv/state/hull/ns/vm.log"},
	} {
		t.Setenv("XDG_STATE_HOME", tc.xdg)
		got, err := instanceLogPath("ns", "vm")
		if err != nil || got != tc.want {
			t.Errorf("XDG_STATE_HOME=%q: instanceLogPath = %q, %v; want %q", tc.xdg, got, err, tc.want)
		}
	}
}

func TestInstanceRecordReadsHullLabels(t *testing.T) {
	rec := instanceRecord(containerd.Instance{ID: "vm", Labels: map[string]string{
		containerd.LabelBackend:     "cloud-hypervisor",
		containerd.LabelImageDigest: "sha256:abc",
		containerd.LabelLogFile:     "/logs/vm.log",
	}})
	if rec.Backend != "cloud-hypervisor" || rec.ImageDigest != "sha256:abc" || rec.LogFile != "/logs/vm.log" {
		t.Errorf("record = %+v, want the backend, digest and log file from the labels", rec)
	}
}

// noBundle stands in for a host with no runtime bundle installed.
func noBundle(t *testing.T) {
	t.Helper()
	old := bundleGuest
	bundleGuest = func() (string, string, bool, error) { return "", "", false, nil }
	t.Cleanup(func() { bundleGuest = old })
}

func TestGenericBootAssetsFetchNothing(t *testing.T) {
	noBundle(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")

	local := t.TempDir()
	t.Setenv("HULL_BOOT_ASSETS", local)
	_, _, err := genericBootAssets()
	if !errors.Is(err, bootassets.ErrNotPresent) {
		t.Errorf("empty HULL_BOOT_ASSETS: error = %v, want ErrNotPresent", err)
	}
	for _, name := range []string{bootassets.KernelName(), bootassets.InitrdName} {
		err = os.WriteFile(filepath.Join(local, name), []byte(name), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	kernel, initrd, err := genericBootAssets()
	if err != nil || kernel != filepath.Join(local, bootassets.KernelName()) || initrd != filepath.Join(local, bootassets.InitrdName) {
		t.Errorf("HULL_BOOT_ASSETS: %q, %q, %v; want the files in %s", kernel, initrd, err, local)
	}

	t.Setenv("HULL_BOOT_ASSETS", "")
	_, _, err = genericBootAssets()
	if err == nil || !strings.Contains(err.Error(), "does not fetch them") {
		t.Errorf("no bundle: error = %v, want a refusal that says the files are not fetched", err)
	}
	// A fallback to the published boot assets would fetch them here.
	_, err = os.Stat(filepath.Join(os.Getenv("HOME"), ".local", "share", "brig", "assets"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the boot asset directory: %v, want nothing fetched into it", err)
	}
}

func TestPlanBoot(t *testing.T) {
	generic := func() (string, string, error) { return "/guest/kernel", "/guest/initrd", nil }
	unikernel := map[string]string{
		ociclient.UnikernelTypeAnnotation:   "unikraft",
		ociclient.UnikernelBinaryAnnotation: "/unikernel/app",
		ociclient.HypervisorAnnotation:      "firecracker",
	}
	for _, tc := range []struct {
		name          string
		run, image    map[string]string
		noBootAssets  bool
		kernel        string
		backend       string
		hypervisorSet bool
		// unikernelType is the type planBoot sets on the run, if any.
		unikernelType string
	}{
		{name: "plain image", kernel: "/guest/kernel", backend: defaultHypervisor, hypervisorSet: true, unikernelType: "linux"},
		{name: "plain image on firecracker", run: map[string]string{ociclient.HypervisorAnnotation: "firecracker"},
			kernel: "/guest/kernel", backend: "firecracker", hypervisorSet: true, unikernelType: "linux"},
		{name: "image names its monitor", image: map[string]string{ociclient.HypervisorAnnotation: "firecracker"},
			kernel: "/guest/kernel", backend: "firecracker", unikernelType: "linux"},
		{name: "image names its type", image: map[string]string{ociclient.UnikernelTypeAnnotation: "freebsd"},
			kernel: "/guest/kernel", backend: defaultHypervisor, hypervisorSet: true},
		{name: "unikernel image", image: unikernel, backend: "firecracker"},
		{name: "unikernel named by the run", run: map[string]string{ociclient.UnikernelBinaryAnnotation: "/bin/app"}},
		{name: "run's own boot files", run: map[string]string{ociclient.BootKernelAnnotation: "/mine/kernel"},
			kernel: "/mine/kernel", backend: defaultHypervisor, hypervisorSet: true, unikernelType: "linux"},
		{name: "no boot assets", noBootAssets: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := make(map[string]string)
			for k, v := range tc.run {
				run[k] = v
			}
			backend, err := planBoot(run, tc.image, tc.noBootAssets, generic)
			if err != nil {
				t.Fatal(err)
			}
			if run[ociclient.BootKernelAnnotation] != tc.kernel {
				t.Errorf("boot kernel = %q, want %q", run[ociclient.BootKernelAnnotation], tc.kernel)
			}
			if backend != tc.backend {
				t.Errorf("backend = %q, want %q", backend, tc.backend)
			}
			_, set := run[ociclient.HypervisorAnnotation]
			if set != (tc.hypervisorSet || tc.run[ociclient.HypervisorAnnotation] != "") {
				t.Errorf("hypervisor annotation set = %v: %v", set, run)
			}
			if run[ociclient.UnikernelTypeAnnotation] != tc.unikernelType {
				t.Errorf("unikernel type = %q, want %q", run[ociclient.UnikernelTypeAnnotation], tc.unikernelType)
			}
		})
	}
	_, err := planBoot(map[string]string{}, nil, false, func() (string, string, error) {
		return "", "", errors.New("no files")
	})
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("generic fails: error = %v, want it passed on", err)
	}
}

func TestCheckMonitor(t *testing.T) {
	for _, m := range containerd.Monitors {
		err := checkMonitor(m)
		if err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
}

// urunc refuses a relative boot path, so the run's are made absolute,
// whether --annotation or HULL_BOOT_ASSETS named them.
func TestAbsBootFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	annotations := map[string]string{
		ociclient.BootKernelAnnotation: "guest/kernel",
		ociclient.BootInitrdAnnotation: "/guest/initrd",
		ociclient.HypervisorAnnotation: "cloud-hypervisor",
	}
	err := absBootFiles(annotations)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		ociclient.BootKernelAnnotation: filepath.Join(dir, "guest", "kernel"),
		ociclient.BootInitrdAnnotation: "/guest/initrd",
		ociclient.HypervisorAnnotation: "cloud-hypervisor",
	}
	if !reflect.DeepEqual(annotations, want) {
		t.Errorf("annotations = %v, want %v", annotations, want)
	}
	none := map[string]string{}
	err = absBootFiles(none)
	if err != nil || len(none) != 0 {
		t.Errorf("no boot files: %v, %v; want none added", none, err)
	}
}

// A second holder of a namespace's log directory waits for the first.
func TestLockLogDir(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockLogDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan func())
	go func() {
		second, err := lockLogDir(dir)
		if err != nil {
			t.Error(err)
			second = func() {}
		}
		locked <- second
	}()
	select {
	case second := <-locked:
		second()
		t.Fatal("a second lock was taken while the first was held")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case second := <-locked:
		second()
	case <-time.After(5 * time.Second):
		t.Fatal("the second lock was not taken once the first was released")
	}
	_, err = lockLogDir(filepath.Join(dir, "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: error = %v, want ErrNotExist", err)
	}
}

func TestVerifyBootFiles(t *testing.T) {
	err := verifyBootFiles("", "")
	if err != nil {
		t.Errorf("no boot files: %v", err)
	}
	err = verifyBootFiles("/boot/kernel", "")
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Errorf("a kernel alone: error = %v, want the macOS refusal", err)
	}

	bundle := t.TempDir()
	kernel, initrd := filepath.Join(bundle, bootassets.KernelName()), filepath.Join(bundle, bootassets.InitrdName)
	var sums strings.Builder
	for _, path := range []string{kernel, initrd} {
		err = os.WriteFile(path, []byte(path), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(path)), filepath.Base(path))
	}
	err = os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), []byte(sums.String()), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyBootFiles(kernel, initrd)
	if err != nil {
		t.Errorf("a bundle's files: %v", err)
	}
	err = os.WriteFile(initrd, []byte("changed"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyBootFiles(kernel, initrd)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a bundle's changed initrd: error = %v, want a refusal", err)
	}
	// An initrd from elsewhere does not spare the bundle's kernel its check.
	elsewhere := filepath.Join(t.TempDir(), "initrd")
	err = os.WriteFile(elsewhere, []byte("initrd"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(kernel, []byte("changed"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyBootFiles(kernel, elsewhere)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a bundle's changed kernel beside an initrd from elsewhere: error = %v, want a refusal", err)
	}

	// Files with neither a bundle's SHA256SUMS nor a provenance record boot,
	// as on macOS, with a warning.
	local := t.TempDir()
	for _, name := range []string{"kernel", "initrd"} {
		err = os.WriteFile(filepath.Join(local, name), []byte(name), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = verifyBootFiles(filepath.Join(local, "kernel"), filepath.Join(local, "initrd"))
	if err != nil {
		t.Errorf("files with no record: %v, want them booted", err)
	}
}

// The refusals that need the image, and what run makes of CreateInstance's
// errors. None of these runs gets as far as a task, so the daemon needs no
// urunc.
func TestRunRefusalsAgainstDaemon(t *testing.T) {
	privateLogDirs(t)
	noBundle(t)
	t.Setenv("HULL_BOOT_ASSETS", "")
	f := newInstanceFixture(t)
	ctx := containerdtest.Lease(context.Background(), t, f.c)
	boot := t.TempDir()
	for _, name := range []string{"kernel", "initrd"} {
		err := os.WriteFile(filepath.Join(boot, name), []byte(name), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	bootFiles := []string{
		"--annotation", ociclient.BootKernelAnnotation + "=" + filepath.Join(boot, "kernel"),
		"--annotation", ociclient.BootInitrdAnnotation + "=" + filepath.Join(boot, "initrd"),
	}
	run := func(args ...string) error {
		t.Helper()
		argv := append([]string{"run", "-d"}, args...)
		_, err := f.run(f.ns, append(argv, containerdtest.TaskImage)...)
		return err
	}

	err := run("--no-boot-assets")
	if !errors.Is(err, containerd.ErrNotVM) || !strings.Contains(err.Error(), "with --annotation") {
		t.Errorf("--no-boot-assets of a plain image: error = %v, want ErrNotVM and how to name a unikernel", err)
	}
	err = run(append([]string{"--pull", "never", "--annotation", ociclient.HypervisorAnnotation + "=hvi"}, bootFiles...)...)
	if err == nil || !strings.Contains(err.Error(), ociclient.HypervisorAnnotation+"=hvi: macOS only") {
		t.Errorf("hvi named by an annotation: error = %v, want it refused", err)
	}
	err = run(append([]string{"--pull", "never", "--annotation", "io.kubernetes.cri.container-name=queue-proxy"}, bootFiles...)...)
	if err == nil || !strings.Contains(err.Error(), "io.kubernetes.cri.container-name is refused") {
		t.Errorf("a CRI container name: error = %v, want it refused", err)
	}

	// A container holds the name, as an instance's does, with its snapshot.
	snapshots := f.c.SnapshotService("")
	_, err = snapshots.Prepare(ctx, "held", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Remove(ctx, "held") })
	held, err := f.c.NewContainer(ctx, "held", client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Delete(context.Background()) })
	err = run(append([]string{"--pull", "never", "--name", "held"}, bootFiles...)...)
	if err == nil || err.Error() != "instance already exists: held" {
		t.Errorf("a name in use: error = %v, want instance already exists: held", err)
	}

	// A snapshot alone is no instance.
	_, err = snapshots.Prepare(ctx, "leftover", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Remove(ctx, "leftover") })
	err = run(append([]string{"--pull", "never", "--name", "leftover"}, bootFiles...)...)
	if err == nil || errors.Is(err, store.ErrInstanceExists) || !strings.Contains(err.Error(), "a snapshot with no container") {
		t.Errorf("a leftover snapshot: error = %v, want the snapshot named", err)
	}
}
