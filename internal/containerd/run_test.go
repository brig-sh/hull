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

package containerd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/brig-sh/hull/internal/imageplatform"
	"github.com/brig-sh/hull/pkg/ociclient"
)

func TestUruncAnnotations(t *testing.T) {
	image := imageUrunc(
		map[string]string{
			ociclient.UnikernelBinaryAnnotation: "/unikernel/app",
			ociclient.BootKernelAnnotation:      "/etc/shadow",
			"com.urunc.unikernel.vAccel":        "exec",
			"org.opencontainers.image.title":    "app",
		},
		map[string]string{
			ociclient.UnikernelBinaryAnnotation: "/from/labels",
			ociclient.HypervisorAnnotation:      "firecracker",
			ociclient.BootInitrdAnnotation:      "/etc/passwd",
		},
	)
	want := map[string]string{ociclient.UnikernelBinaryAnnotation: "/unikernel/app", ociclient.HypervisorAnnotation: "firecracker"}
	if !reflect.DeepEqual(image, want) {
		t.Errorf("imageUrunc = %v, want %v: the manifest over the labels, and no host keys", image, want)
	}

	got := UruncAnnotations(map[string]string{
		ociclient.HypervisorAnnotation: "cloud-hypervisor",
		ociclient.BootKernelAnnotation: "/host/kernel",
		"example.com/other":            "kept",
	}, image)
	for _, k := range uruncKeys {
		_, ok := got[k]
		if !ok {
			t.Errorf("no %s in the spec annotations, so the shim would take the image's", k)
		}
	}
	for k, v := range map[string]string{
		ociclient.UnikernelBinaryAnnotation: "/unikernel/app",
		ociclient.HypervisorAnnotation:      "cloud-hypervisor",
		ociclient.BootKernelAnnotation:      "/host/kernel",
		ociclient.BootInitrdAnnotation:      "",
		"example.com/other":                 "kept",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestProcessFromImage(t *testing.T) {
	config := ocispec.ImageConfig{Entrypoint: []string{"/init"}, Cmd: []string{"serve"}, User: "1000:100"}
	p, err := processFromImage(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Args, []string{"/init", "serve"}) || p.Cwd != "/" || p.User.UID != 1000 || p.User.GID != 100 {
		t.Errorf("process = %+v, want /init serve in / as 1000:100", p)
	}
	p, err = processFromImage(config, []string{"sleep", "1"})
	if err != nil || !reflect.DeepEqual(p.Args, []string{"/init", "sleep", "1"}) {
		t.Errorf("with args: %v, %v; want the entrypoint kept and the cmd replaced", p.Args, err)
	}
	_, err = processFromImage(ocispec.ImageConfig{}, nil)
	if err == nil {
		t.Error("an image with no command and no args was accepted")
	}
}

func TestNumericUser(t *testing.T) {
	for _, tc := range []struct {
		user     string
		uid, gid uint32
	}{
		{"", 0, 0},
		{"root", 0, 0},
		{"0", 0, 0},
		{"1000", 1000, 0},
		{"1000:1000", 1000, 1000},
		{"root:5", 0, 5},
	} {
		uid, gid, err := numericUser(tc.user)
		if err != nil || uid != tc.uid || gid != tc.gid {
			t.Errorf("numericUser(%q) = %d, %d, %v; want %d, %d", tc.user, uid, gid, err, tc.uid, tc.gid)
		}
	}
	for _, user := range []string{"nobody", "1000:staff", "-1"} {
		_, _, err := numericUser(user)
		if err == nil || !strings.Contains(err.Error(), "numeric") {
			t.Errorf("numericUser(%q): error = %v, want a refusal", user, err)
		}
	}
}

// annotatedImage records the image name, whose manifest is TaskImage's for
// the host with annotations added, so it unpacks to TaskImage's layers.
func annotatedImage(ctx context.Context, t *testing.T, c *client.Client, name string, annotations map[string]string) {
	t.Helper()
	base, err := c.Pull(ctx, containerdtest.TaskImage, client.WithPullUnpack)
	if err != nil {
		t.Fatal(err)
	}
	cs := c.ContentStore()
	manifest, err := images.Manifest(ctx, cs, base.Target(), platforms.Default())
	if err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = annotations
	p := platforms.DefaultSpec()
	desc := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageManifest, manifest)
	desc.Platform = &p
	containerdtest.CreateImage(ctx, t, c, name, desc)
}

func runPlatform(t *testing.T) ocispec.Platform {
	t.Helper()
	p, err := imageplatform.Parse("linux/" + runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// An image cannot name the host kernel or initrd a VM boots: the run's are
// in the spec, and every other urunc key is too, so urunc's shim has none
// left to copy from the manifest.
func TestNewContainerThroughDaemon(t *testing.T) {
	c := instancesClient(t)
	ctx := containerdtest.Lease(context.Background(), t, c)
	const name = "example.com/hull-test/annotated:1"
	annotatedImage(ctx, t, c, name, map[string]string{
		ociclient.BootKernelAnnotation: "/etc/shadow",
		ociclient.BootInitrdAnnotation: "/etc/shadow",
		ociclient.HypervisorAnnotation: "firecracker",
	})
	img, err := ImageForRun(ctx, c, name, "never", runPlatform(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := LoadRunImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "vm.log")
	ctr, err := newContainer(ctx, c, image, RunConfig{
		ID:       "vm",
		Args:     []string{"sleep", "1"},
		Env:      []string{"PATH=/custom", "FOO=bar"},
		MemoryMB: 256,
		CPUs:     2,
		Annotations: map[string]string{
			ociclient.BootKernelAnnotation:    "/run/kernel",
			ociclient.BootInitrdAnnotation:    "/run/initrd",
			ociclient.UnikernelTypeAnnotation: "linux",
		},
		Backend: "firecracker",
		LogFile: logFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(context.Background(), client.WithSnapshotCleanup) })

	info, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Runtime.Name != RuntimeName {
		t.Errorf("runtime = %q, want %q", info.Runtime.Name, RuntimeName)
	}
	wantLabels := map[string]string{
		LabelBackend:     "firecracker",
		LabelImageDigest: img.Target().Digest.String(),
		LabelLogFile:     logFile,
	}
	if !reflect.DeepEqual(info.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", info.Labels, wantLabels)
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range uruncKeys {
		_, ok := spec.Annotations[k]
		if !ok {
			t.Errorf("no %s in the spec", k)
		}
	}
	if spec.Annotations[ociclient.BootKernelAnnotation] != "/run/kernel" || spec.Annotations[ociclient.BootInitrdAnnotation] != "/run/initrd" {
		t.Errorf("boot kernel %q, initrd %q, want the run's, not the image's", spec.Annotations[ociclient.BootKernelAnnotation],
			spec.Annotations[ociclient.BootInitrdAnnotation])
	}
	if spec.Annotations[ociclient.HypervisorAnnotation] != "firecracker" {
		t.Errorf("hypervisor = %q, want the image's", spec.Annotations[ociclient.HypervisorAnnotation])
	}
	if !reflect.DeepEqual(spec.Process.Args, []string{"sleep", "1"}) || spec.Process.Cwd != "/" || spec.Hostname != "vm" {
		t.Errorf("process %v in %q on host %q, want sleep 1 in / on vm", spec.Process.Args, spec.Process.Cwd, spec.Hostname)
	}
	var paths []string
	foo := false
	for _, e := range spec.Process.Env {
		if strings.HasPrefix(e, "PATH=") {
			paths = append(paths, e)
		}
		foo = foo || e == "FOO=bar"
	}
	if !reflect.DeepEqual(paths, []string{"PATH=/custom"}) || !foo {
		t.Errorf("env = %v, want the run's PATH alone, and FOO", spec.Process.Env)
	}
	res := spec.Linux.Resources
	if res.Memory == nil || *res.Memory.Limit != 256<<20 || res.CPU == nil || *res.CPU.Quota != 2*cpuPeriod || *res.CPU.Period != cpuPeriod {
		t.Errorf("resources = %+v, want 256 MiB and 2 CPUs", res)
	}
}

// A task that cannot be created leaves no container, snapshot or log behind,
// so the name is free for the next run.
func TestCreateInstanceRemovesAContainerWhoseTaskFails(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := LoadRunImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	// urunc refuses a boot initrd without a boot kernel, and a daemon
	// without urunc's shim, such as CI's, cannot create the task at all, so
	// the task fails on either. A log left by an earlier instance of the
	// name is there too, and goes.
	logFile := filepath.Join(t.TempDir(), "vm.log")
	err = os.WriteFile(logFile, []byte("an earlier instance's console\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = CreateInstance(ctx, c, image, RunConfig{
		ID:   "vm",
		Args: []string{"true"},
		Annotations: map[string]string{
			ociclient.BootInitrdAnnotation:    "/nonexistent/initrd",
			ociclient.UnikernelTypeAnnotation: "linux",
			ociclient.HypervisorAnnotation:    "cloud-hypervisor",
		},
		LogFile: logFile,
	})
	if err == nil {
		t.Fatal("CreateInstance started a task with a boot initrd and no kernel")
	}
	assertNoContainer(ctx, t, c, "vm")
	_, err = os.Stat(logFile)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log %s: error = %v, want it removed", logFile, err)
	}
	snapshotter := c.SnapshotService("")
	_, err = snapshotter.Stat(ctx, "vm")
	if !errdefs.IsNotFound(err) {
		t.Errorf("snapshot vm: error = %v, want it removed", err)
	}
}

func TestCheckBootsVM(t *testing.T) {
	unikernel := map[string]string{
		ociclient.UnikernelTypeAnnotation:   "unikraft",
		ociclient.HypervisorAnnotation:      "firecracker",
		ociclient.UnikernelBinaryAnnotation: "/unikernel/app",
	}
	err := checkBootsVM(unikernel)
	if err != nil {
		t.Errorf("a unikernel with its type, monitor and binary: %v", err)
	}
	// A boot kernel and initrd stand in for the binary, but not for the type
	// or the monitor, which a urunc that does not fill them in needs. urunc
	// rejects half a boot pair itself, so it never reaches runc.
	for _, boot := range []string{ociclient.BootKernelAnnotation, ociclient.BootInitrdAnnotation} {
		err = checkBootsVM(map[string]string{
			boot:                              "/boot/file",
			ociclient.UnikernelTypeAnnotation: "linux",
			ociclient.HypervisorAnnotation:    "cloud-hypervisor",
		})
		if err != nil {
			t.Errorf("%s with a type and a monitor: %v", boot, err)
		}
		err = checkBootsVM(map[string]string{boot: "/boot/file"})
		if !errors.Is(err, ErrNotVM) || !strings.Contains(err.Error(), ociclient.UnikernelTypeAnnotation) ||
			!strings.Contains(err.Error(), ociclient.HypervisorAnnotation) || strings.Contains(err.Error(), ociclient.UnikernelBinaryAnnotation) {
			t.Errorf("%s alone: error = %v, want ErrNotVM naming the type and the monitor, not the binary", boot, err)
		}
	}
	for _, drop := range []string{ociclient.UnikernelTypeAnnotation, ociclient.HypervisorAnnotation, ociclient.UnikernelBinaryAnnotation} {
		partial := make(map[string]string)
		for k, v := range unikernel {
			if k != drop {
				partial[k] = v
			}
		}
		err = checkBootsVM(partial)
		if !errors.Is(err, ErrNotVM) || !strings.Contains(err.Error(), drop) {
			t.Errorf("no %s: error = %v, want ErrNotVM naming it", drop, err)
		}
	}
	err = checkBootsVM(UruncAnnotations(nil, nil))
	if !errors.Is(err, ErrNotVM) {
		t.Errorf("no urunc annotations: error = %v, want ErrNotVM", err)
	}
}

func TestCheckCRIKeys(t *testing.T) {
	err := checkCRIKeys(UruncAnnotations(map[string]string{"org.example/note": "kept"}, nil))
	if err != nil {
		t.Errorf("no CRI keys: %v", err)
	}
	for _, name := range []string{"queue-proxy", "user-container", ""} {
		err = checkCRIKeys(map[string]string{criContainerName: name})
		if err == nil {
			t.Errorf("%s=%q: no error, want it refused", criContainerName, name)
		}
	}
}

// A run urunc would hand to runc is refused before a container exists.
func TestCreateInstanceRefusesWhatWouldNotBootAVM(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := LoadRunImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	err = CreateInstance(ctx, c, image, RunConfig{
		ID:          "vm",
		Args:        []string{"true"},
		Annotations: map[string]string{ociclient.UnikernelBinaryAnnotation: "/bin/sh"},
		LogFile:     filepath.Join(t.TempDir(), "vm.log"),
	})
	if !errors.Is(err, ErrNotVM) {
		t.Fatalf("CreateInstance of a binary with no type or monitor: error = %v, want ErrNotVM", err)
	}
	assertNoContainer(ctx, t, c, "vm")
}

func TestImageForRunPolicies(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	_, err := ImageForRun(ctx, c, containerdtest.TaskImage, "never", runPlatform(t), nil)
	if err == nil || !strings.Contains(err.Error(), "--pull never") {
		t.Errorf("never, not pulled: error = %v, want a refusal", err)
	}
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), nil)
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	again, err := ImageForRun(ctx, c, containerdtest.TaskImage, "never", runPlatform(t), nil)
	if err != nil || again.Target().Digest != img.Target().Digest {
		t.Errorf("never, pulled: %v, %v; want the pulled image", again, err)
	}
}

// A ref pinned to a digest finds the image pulled by its tag, so --pull never
// runs it and missing does not fetch it again.
func TestImageForRunFindsADigestPulledByTag(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	pinned := containerdtest.TaskImage + "@" + img.Target().Digest.String()
	got, err := ImageForRun(ctx, c, pinned, "never", runPlatform(t), nil)
	if err != nil || got.Name() != img.Name() {
		t.Fatalf("never, %s: %v, %v; want the image pulled by its tag", pinned, got, err)
	}
	other := "ghcr.io/containerd/other@" + img.Target().Digest.String()
	_, err = ImageForRun(ctx, c, other, "never", runPlatform(t), nil)
	if err == nil {
		t.Errorf("never, %s: found an image of another repository", other)
	}
}

// An image pulled for another platform keeps an index that lists the host's
// manifest, without its content. --pull never refuses it, naming the
// platform, and missing pulls the host's.
func TestImageForRunWantsTheHostPlatform(t *testing.T) {
	c := instancesClient(t)
	ctx := containerdtest.Lease(context.Background(), t, c)
	host := runPlatform(t)
	other := ocispec.Platform{OS: "linux", Architecture: "s390x"}
	if host.Architecture == other.Architecture {
		other.Architecture = "riscv64"
	}
	cs := c.ContentStore()
	absent := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromString("the host's manifest, not pulled"),
		Size:      30,
		Platform:  &host,
	}
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{containerdtest.WriteImage(ctx, t, cs, other, nil, true), absent},
	})
	containerdtest.CreateImage(ctx, t, c, containerdtest.TaskImage, index)

	_, err := ImageForRun(ctx, c, containerdtest.TaskImage, "never", host, nil)
	if err == nil || !strings.Contains(err.Error(), "has not been pulled for "+platforms.Format(host)) {
		t.Errorf("never: error = %v, want a refusal naming %s", err, platforms.Format(host))
	}
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", host, nil)
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	_, err = LoadRunImage(ctx, img)
	if err != nil {
		t.Errorf("missing: the image pulled cannot be read for %s: %v", platforms.Format(host), err)
	}
}

// A snapshot left under a name, with no container, is reported as what it
// is: hull rm finds no instance of that name to remove.
func TestCreateInstanceNamesALeftoverSnapshot(t *testing.T) {
	c := instancesClient(t)
	ctx := containerdtest.Lease(context.Background(), t, c)
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := LoadRunImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := c.SnapshotService("")
	_, err = snapshots.Prepare(ctx, "vm", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Remove(ctx, "vm") })
	err = CreateInstance(ctx, c, image, RunConfig{
		ID:   "vm",
		Args: []string{"true"},
		Annotations: map[string]string{
			ociclient.BootKernelAnnotation:    "/boot/kernel",
			ociclient.BootInitrdAnnotation:    "/boot/initrd",
			ociclient.UnikernelTypeAnnotation: "linux",
			ociclient.HypervisorAnnotation:    "cloud-hypervisor",
		},
		LogFile: filepath.Join(t.TempDir(), "vm.log"),
	})
	if err == nil || errdefs.IsAlreadyExists(err) || !strings.Contains(err.Error(), "a snapshot with no container") {
		t.Errorf("CreateInstance: error = %v, want the snapshot named, and not AlreadyExists", err)
	}
	assertNoContainer(ctx, t, c, "vm")
}
