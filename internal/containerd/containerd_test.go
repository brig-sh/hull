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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/pkg/identifiers"
)

func fakeIDs(t *testing.T, euid, uid int) {
	t.Helper()
	origEUID, origUID := geteuid, getuid
	geteuid = func() int { return euid }
	getuid = func() int { return uid }
	t.Cleanup(func() { geteuid, getuid = origEUID, origUID })
}

func TestResolveAddress(t *testing.T) {
	cases := []struct {
		name       string
		explicit   string
		env        string
		runtimeDir string
		euid       int
		want       string
	}{
		{name: "explicit wins over env", explicit: "/a.sock", env: "/b.sock", euid: 1000, want: "/a.sock"},
		{name: "env wins over default", env: "/b.sock", euid: 0, want: "/b.sock"},
		{name: "root uses the system socket", euid: 0, runtimeDir: "/run/user/0", want: "/run/containerd/containerd.sock"},
		{name: "user uses the rootless socket", euid: 1000, runtimeDir: "/run/user/1000", want: "/run/user/1000/containerd/containerd.sock"},
		{name: "user without XDG_RUNTIME_DIR", euid: 1000, want: "/run/user/1000/containerd/containerd.sock"},
		{name: "user with a relative XDG_RUNTIME_DIR", euid: 1000, runtimeDir: "rt", want: "/run/user/1000/containerd/containerd.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeIDs(t, tc.euid, 1000)
			// No rootlesskit, whatever runs on the host; see
			// TestResolveAddressInRootlessKit.
			t.Setenv("ROOTLESSKIT_STATE_DIR", filepath.Join(t.TempDir(), "none"))
			// The variable nerdctl and ctr read.
			t.Setenv("CONTAINERD_ADDRESS", tc.env)
			t.Setenv("XDG_RUNTIME_DIR", tc.runtimeDir)
			got, err := ResolveAddress(tc.explicit)
			if err != nil || got != tc.want {
				t.Fatalf("ResolveAddress(%q) = %q, %v; want %q", tc.explicit, got, err, tc.want)
			}
		})
	}
}

// With a rootlesskit running, a given address is a path in its mount
// namespace, as nerdctl, which runs there, reads it.
func TestResolveAddressInRootlessKit(t *testing.T) {
	fakeIDs(t, 1000, 1000)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	stateDir := t.TempDir()
	t.Setenv("ROOTLESSKIT_STATE_DIR", stateDir)
	err := os.WriteFile(filepath.Join(stateDir, "child_pid"), []byte("4242\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A directory stands in for the child's root. Its /var/run is an
	// absolute link to /run, as on most distributions, which the kernel
	// would follow to the host's /run.
	root := t.TempDir()
	err = os.MkdirAll(filepath.Join(root, "var"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("/run", filepath.Join(root, "var", "run"))
	if err != nil {
		t.Fatal(err)
	}
	origRoot := childRoot
	t.Cleanup(func() { childRoot = origRoot })
	childRoot = func(pid int) string {
		if pid != 4242 {
			t.Errorf("childRoot(%d), want the pid in child_pid, 4242", pid)
		}
		return root
	}
	for _, tc := range []struct {
		name, explicit, env, want string
	}{
		{name: "the environment", env: "unix:///run/containerd/containerd.sock", want: root + "/run/containerd/containerd.sock"},
		{name: "the flag", explicit: "/run/containerd/containerd.sock", want: root + "/run/containerd/containerd.sock"},
		{name: "a link in the namespace", env: "/var/run/containerd/containerd.sock", want: root + "/run/containerd/containerd.sock"},
		// nerdctl takes a relative one from the working directory it keeps
		// in rootlesskit's namespace, which is hull's.
		{name: "a relative path", env: "c.sock", want: "c.sock"},
		{name: "the default", want: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "containerd", "containerd.sock")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERD_ADDRESS", tc.env)
			got, err := ResolveAddress(tc.explicit)
			if err != nil || got != tc.want {
				t.Errorf("ResolveAddress(%q) = %q, %v; want %q", tc.explicit, got, err, tc.want)
			}
		})
	}

	// A stopped rootlesskit leaves its state directory, with a child that
	// is gone, and nerdctl fails then rather than reach the system daemon.
	t.Setenv("CONTAINERD_ADDRESS", "/run/containerd/containerd.sock")
	childRoot = func(int) string { return filepath.Join(root, "gone") }
	_, err = ResolveAddress("")
	if err == nil || !strings.Contains(err.Error(), "rootless containerd not running? rootlesskit's child, process 4242") {
		t.Errorf("with rootlesskit's child gone: error = %v, want rootless containerd not running", err)
	}
	for _, content := range []string{"", "x\n", "0\n"} {
		err = os.WriteFile(filepath.Join(stateDir, "child_pid"), []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ResolveAddress("")
		if err == nil || !strings.Contains(err.Error(), "holds no process ID") {
			t.Errorf("child_pid %q: error = %v, want it refused", content, err)
		}
	}
	err = os.Remove(filepath.Join(stateDir, "child_pid"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveAddress("")
	if err == nil || !strings.Contains(err.Error(), "rootless containerd not running?") {
		t.Errorf("with no child_pid: error = %v, want rootless containerd not running", err)
	}
	// Root drives the system daemon, and no rootlesskit runs for it.
	fakeIDs(t, 0, 0)
	got, err := ResolveAddress("")
	if err != nil || got != "/run/containerd/containerd.sock" {
		t.Errorf("as root: ResolveAddress = %q, %v; want the address unchanged", got, err)
	}
}

// The kernel's own walk of /proc/<pid>/root, which the test above stands
// in for: with this process as the child, the path resolves on its root.
func TestResolveAddressThroughProc(t *testing.T) {
	fakeIDs(t, 1000, 1000)
	stateDir := t.TempDir()
	t.Setenv("ROOTLESSKIT_STATE_DIR", stateDir)
	err := os.WriteFile(filepath.Join(stateDir, "child_pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	got, err := ResolveAddress(filepath.Join(dir, "c.sock"))
	want := fmt.Sprintf("/proc/%d/root%s", os.Getpid(), filepath.Join(dir, "c.sock"))
	if err != nil || got != want {
		t.Errorf("ResolveAddress = %q, %v; want %q", got, err, want)
	}
}

func TestRootlessKitStateDir(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	err := os.Mkdir("rk", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	rt := t.TempDir()
	err = os.Mkdir(filepath.Join(rt, "containerd-rootless"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		euid int
		env  string
		want string
	}{
		{name: "root", euid: 0, env: filepath.Join(cwd, "rk"), want: ""},
		{name: "the default", euid: 1000, want: filepath.Join(rt, "containerd-rootless")},
		{name: "the environment", euid: 1000, env: filepath.Join(cwd, "rk"), want: filepath.Join(cwd, "rk")},
		// The hook reads it from the runtime's working directory.
		{name: "a relative one is made absolute", euid: 1000, env: "rk", want: filepath.Join(cwd, "rk")},
		{name: "one that does not exist", euid: 1000, env: filepath.Join(cwd, "missing"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeIDs(t, tc.euid, 1000)
			t.Setenv("XDG_RUNTIME_DIR", rt)
			t.Setenv("ROOTLESSKIT_STATE_DIR", tc.env)
			got := RootlessKitStateDir()
			if got != tc.want {
				t.Errorf("RootlessKitStateDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveNamespace(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      string
		want     string
	}{
		{name: "explicit wins over env", explicit: "a", env: "b", want: "a"},
		{name: "env wins over default", env: "b", want: "b"},
		{name: "default", want: "hull"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The variable nerdctl and ctr read.
			t.Setenv("CONTAINERD_NAMESPACE", tc.env)
			got := ResolveNamespace(tc.explicit)
			if got != tc.want {
				t.Fatalf("ResolveNamespace(%q) = %q, want %q", tc.explicit, got, tc.want)
			}
		})
	}
}

// The default must be a namespace containerd accepts, or every request in it
// is refused.
func TestDefaultNamespaceIsValid(t *testing.T) {
	err := identifiers.Validate(DefaultNamespace)
	if err != nil {
		t.Fatal(err)
	}
}

// A namespace containerd would refuse fails before any dial, so it fails the
// same way with no daemon behind the address.
func TestConnectRefusesAnInvalidNamespace(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "containerd.sock")
	for _, ns := range []string{"", "has space", "-leading", "a/b"} {
		c, err := Connect(context.Background(), addr, ns)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Connect accepted namespace %q", ns)
		}
		if !strings.Contains(err.Error(), "namespace") {
			t.Errorf("namespace %q: error %q does not say the namespace is the problem", ns, err)
		}
	}
}

// With no daemon behind the address, Connect must fail promptly and name the
// address and the override.
func TestConnectFailsClearlyWithNoDaemon(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "containerd.sock")
	start := time.Now()
	c, err := Connect(context.Background(), addr, DefaultNamespace)
	if err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded with no daemon")
	}
	elapsed := time.Since(start)
	if elapsed > probeTimeout+time.Second {
		t.Errorf("Connect took %v to fail", elapsed)
	}
	for _, want := range []string{addr, AddressEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestConnectToDaemon checks that Connect reaches a live daemon and scopes the
// client to the namespace it was given.
func TestConnectToDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), addr, "hull-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	got := c.DefaultNamespace()
	if got != "hull-test" {
		t.Fatalf("client namespace = %q, want hull-test", got)
	}
}

// The same daemon is reachable with its address written as a unix:// URL.
func TestConnectToDaemonWithUnixScheme(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), "unix://"+addr, "hull-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
