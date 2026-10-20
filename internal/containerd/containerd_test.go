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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeIDs(t, tc.euid, 1000)
			t.Setenv(AddressEnv, tc.env)
			t.Setenv("XDG_RUNTIME_DIR", tc.runtimeDir)
			got := ResolveAddress(tc.explicit)
			if got != tc.want {
				t.Fatalf("ResolveAddress(%q) = %q, want %q", tc.explicit, got, tc.want)
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
			t.Setenv(NamespaceEnv, tc.env)
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
