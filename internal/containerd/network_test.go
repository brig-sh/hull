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
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/cni"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/brig-sh/hull/pkg/ociclient"
)

func testNetwork(dir string) *Network {
	return &Network{
		Hook:       specs.Hook{Path: "/usr/bin/hull", Args: []string{"/usr/bin/hull", "internal", "oci-hook"}},
		Dir:        dir,
		ResolvConf: []byte("nameserver 192.0.2.1\n"),
	}
}

func TestWithNetwork(t *testing.T) {
	n := testNetwork("/state/vm.net")
	var s oci.Spec
	err := withNetwork(n)(context.Background(), nil, nil, &s)
	if err != nil {
		t.Fatal(err)
	}
	want := &specs.Hooks{
		CreateRuntime: []specs.Hook{{Path: "/usr/bin/hull", Args: []string{"/usr/bin/hull", "internal", "oci-hook", "createRuntime"}}},
		Poststop:      []specs.Hook{{Path: "/usr/bin/hull", Args: []string{"/usr/bin/hull", "internal", "oci-hook", "postStop"}}},
	}
	if !reflect.DeepEqual(s.Hooks, want) {
		t.Errorf("hooks = %+v, want %+v", s.Hooks, want)
	}
	if len(n.Hook.Args) != 3 {
		t.Errorf("Network.Hook.Args = %q, want it left as it was", n.Hook.Args)
	}
	// /etc/resolv.conf only: the guest keeps the image's /etc/hosts.
	wantMounts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: "/state/vm.net/resolv.conf", Options: []string{"bind", "rprivate"}},
	}
	if !reflect.DeepEqual(s.Mounts, wantMounts) {
		t.Errorf("mounts = %+v, want %+v", s.Mounts, wantMounts)
	}
}

func TestWriteNetworkFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm.net")
	err := os.Mkdir(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "network.json"), []byte(`{"ip":"10.4.0.9"}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// urunc copies the file's mode into the guest, where it must stay
	// readable whatever umask hull ran with.
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	n := testNetwork(dir)
	err = writeNetworkFiles(n)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(dir, "network.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an old instance's network.json: error = %v, want it gone", err)
	}
	path := filepath.Join(dir, ResolvConfFile)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(n.ResolvConf) {
		t.Errorf("%s = %q, %v; want %q", ResolvConfFile, got, err, n.ResolvConf)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("%s mode = %v, %v; want 0644", ResolvConfFile, info.Mode(), err)
	}
}

func TestWithHookError(t *testing.T) {
	dir := t.TempDir()
	base := errors.New("create the task of vm: failed to read from init socket: EOF")
	err := withHookError(base, dir)
	if err != base {
		t.Errorf("with no hook error = %v, want the task's error alone", err)
	}
	err = os.WriteFile(filepath.Join(dir, HookErrorFile), []byte("attach to CNI network bridge: no address left\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = withHookError(base, dir)
	want := base.Error() + ": the network hook failed: attach to CNI network bridge: no address left"
	if !errors.Is(err, base) || err.Error() != want {
		t.Errorf("with a hook error = %q, want %q wrapping the task's error", err, want)
	}
}

// TestWriteNetworkFilesReleasesAPin checks that the network directory of
// an instance whose postStop never ran, with its namespace still pinned,
// can be replaced, and removed.
func TestWriteNetworkFilesReleasesAPin(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pinning a namespace is a mount, which needs root")
	}
	dir := filepath.Join(t.TempDir(), "vm.net")
	err := os.Mkdir(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(dir, NetnsPinFile)
	err = cni.PinNetns("/proc/self/ns/net", pin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cni.UnpinNetns(pin) })
	err = writeNetworkFiles(testNetwork(dir))
	if err != nil {
		t.Fatalf("writeNetworkFiles over a pinned namespace: %v", err)
	}
	if cni.Pinned(pin) {
		t.Error("the old pin is still mounted")
	}
	err = cni.PinNetns("/proc/self/ns/net", pin)
	if err != nil {
		t.Fatal(err)
	}
	err = RemoveNetworkDir(dir)
	if err != nil {
		t.Fatalf("RemoveNetworkDir over a pinned namespace: %v", err)
	}
	_, err = os.Stat(dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("network directory: error = %v, want it removed", err)
	}
}

// TestCreateInstanceNetworkFiles checks that a run leaves the network files
// of a live instance of its name alone, whether it finds the name taken
// before it creates its container or after, and removes its own when it
// fails.
func TestCreateInstanceNetworkFiles(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	img, err := ImageForRun(ctx, c, containerdtest.TaskImage, "missing", runPlatform(t), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := LoadRunImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{
		Name: "vm",
		Args: []string{"true"},
		// As in TestCreateInstanceRemovesAContainerWhoseTaskFails: the task
		// fails, whether or not the daemon has urunc's shim.
		Annotations: map[string]string{
			ociclient.BootInitrdAnnotation:    "/nonexistent/initrd",
			ociclient.UnikernelTypeAnnotation: "linux",
			ociclient.HypervisorAnnotation:    "cloud-hypervisor",
		},
		LogFile: filepath.Join(t.TempDir(), "vm.log"),
		Network: testNetwork(filepath.Join(t.TempDir(), "vm.net")),
	}

	live, err := c.NewContainer(ctx, "vm", client.WithNewSnapshot("vm", img), client.WithNewSpec())
	if err != nil {
		t.Fatal(err)
	}
	err = os.Mkdir(cfg.Network.Dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	liveResolv := filepath.Join(cfg.Network.Dir, ResolvConfFile)
	err = os.WriteFile(liveResolv, []byte("nameserver 198.51.100.1\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = CreateInstance(ctx, c, image, cfg)
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("CreateInstance of a name in use = %v, want AlreadyExists", err)
	}
	got, err := os.ReadFile(liveResolv)
	if err != nil || string(got) != "nameserver 198.51.100.1\n" {
		t.Errorf("the live instance's resolv.conf = %q, %v; want it untouched", got, err)
	}
	err = live.Delete(ctx, client.WithSnapshotCleanup)
	if err != nil {
		t.Fatal(err)
	}

	// A run under another lock creates its container between this run's
	// create and its second check.
	afterCreate = func() { live = labeledContainer(ctx, t, c, "vm") }
	t.Cleanup(func() { afterCreate = func() {} })
	err = CreateInstance(ctx, c, image, cfg)
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("CreateInstance that loses the race for its name = %v, want AlreadyExists", err)
	}
	got, err = os.ReadFile(liveResolv)
	if err != nil || string(got) != "nameserver 198.51.100.1\n" {
		t.Errorf("after a lost race, the live instance's resolv.conf = %q, %v; want it untouched", got, err)
	}
	afterCreate = func() {}
	err = live.Delete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = CreateInstance(ctx, c, image, cfg)
	if err == nil {
		t.Fatal("CreateInstance started a task with a boot initrd and no kernel")
	}
	assertNoContainer(ctx, t, c, "vm")
	_, err = os.Stat(cfg.Network.Dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("network directory: error = %v, want it removed with the failed run", err)
	}
}
