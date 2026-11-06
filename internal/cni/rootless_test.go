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

package cni

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRootlessKitDNS(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/info" {
			http.NotFound(w, r)
			return
		}
		// The answer rootlesskit v3.2.0 gives under slirp4netns.
		_, _ = w.Write([]byte(`{"apiVersion":"1.1.3","version":"3.2.0","childPID":1543,` +
			`"networkDriver":{"driver":"slirp4netns","dns":["10.0.2.3"],"childIP":"10.0.2.100"},` +
			`"portDriver":{"driver":"builtin"}}`))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	got, err := RootlessKitDNS(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"10.0.2.3"}) {
		t.Errorf("RootlessKitDNS = %q, want [10.0.2.3]", got)
	}

	_, err = RootlessKitDNS(context.Background(), t.TempDir())
	if !errors.Is(err, ErrRootlessKitNotRunning) {
		t.Errorf("RootlessKitDNS with no API socket = %v, want ErrRootlessKitNotRunning", err)
	}

	// A socket a killed rootlesskit left, which nothing listens on.
	stale := t.TempDir()
	sl, err := net.Listen("unix", filepath.Join(stale, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	err = sl.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = RootlessKitDNS(context.Background(), stale)
	if !errors.Is(err, ErrRootlessKitNotRunning) {
		t.Errorf("RootlessKitDNS with a stale API socket = %v, want ErrRootlessKitNotRunning", err)
	}

	// A rootlesskit that answers, but not with its info, is a fault.
	bad := t.TempDir()
	bl, err := net.Listen("unix", filepath.Join(bad, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	bsrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})}
	go func() { _ = bsrv.Serve(bl) }()
	t.Cleanup(func() { _ = bsrv.Close() })
	_, err = RootlessKitDNS(context.Background(), bad)
	if err == nil || errors.Is(err, ErrRootlessKitNotRunning) {
		t.Errorf("RootlessKitDNS with a failing API = %v, want an error other than ErrRootlessKitNotRunning", err)
	}
}

func TestEnterDetachedNetnsWithNothingToEnter(t *testing.T) {
	// A state directory whose netns is this process's own namespace.
	same := t.TempDir()
	err := os.Symlink("/proc/self/ns/net", filepath.Join(same, "netns"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		dir    string
		userNS bool
	}{
		{name: "no state directory", dir: "", userNS: true},
		{name: "not in a user namespace", dir: same, userNS: false},
		{name: "no detached namespace", dir: t.TempDir(), userNS: true},
		{name: "already in the namespace", dir: same, userNS: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := runningInUserNS
			runningInUserNS = func() bool { return tc.userNS }
			t.Cleanup(func() { runningInUserNS = old })
			// Anything but a return would run the test binary again, or
			// fail.
			err := EnterDetachedNetns(tc.dir)
			if err != nil {
				t.Errorf("EnterDetachedNetns(%q) = %v, want nil", tc.dir, err)
			}
		})
	}

	// A netns that cannot be read is an error, not a reason to stay.
	bad := t.TempDir()
	err = os.Symlink("netns", filepath.Join(bad, "netns"))
	if err != nil {
		t.Fatal(err)
	}
	old := runningInUserNS
	runningInUserNS = func() bool { return true }
	t.Cleanup(func() { runningInUserNS = old })
	err = EnterDetachedNetns(bad)
	if err == nil || !strings.Contains(err.Error(), "find rootlesskit's network namespace") {
		t.Errorf("EnterDetachedNetns with a netns in a symlink loop = %v", err)
	}
}

func TestPinNetns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pinning a namespace is a mount, which needs root")
	}
	target := filepath.Join(t.TempDir(), "netns")
	err := PinNetns("/proc/self/ns/net", target)
	if err != nil {
		t.Fatal(err)
	}
	var want, got unix.Stat_t
	err = unix.Stat("/proc/self/ns/net", &want)
	if err != nil {
		t.Fatal(err)
	}
	err = unix.Stat(target, &got)
	if err != nil || got.Ino != want.Ino || !Pinned(target) {
		t.Errorf("the pin at %s is not this process's namespace: %v", target, err)
	}
	// A second pin replaces the first.
	err = PinNetns("/proc/self/ns/net", target)
	if err != nil {
		t.Fatal(err)
	}
	err = UnpinNetns(target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(target)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s after UnpinNetns: error = %v, want it gone", target, err)
	}
	err = UnpinNetns(target)
	if err != nil {
		t.Errorf("UnpinNetns with no pin = %v", err)
	}
}
