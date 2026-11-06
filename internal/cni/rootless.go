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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/moby/sys/userns"
	"golang.org/x/sys/unix"
)

// A rootless containerd runs inside rootlesskit's user and mount
// namespaces, and so do the hooks its runtime runs. With --detach-netns,
// which containerd-rootless.sh passes when rootlesskit supports it, the
// daemon stays in the host's network namespace and rootlesskit keeps the
// one slirp4netns serves at <state dir>/netns. A bridge has to be made in
// that one: the user namespace does not own the host's, and only there does
// traffic reach slirp4netns.

// runningInUserNS reports whether this process is in a user namespace. A
// variable, so tests, which do not run in one, can reach the checks after
// it.
var runningInUserNS = userns.RunningInUserNS

// EnterDetachedNetns runs this process again, with the same arguments, in
// the detached network namespace of the rootlesskit whose state directory
// is stateDir, so a hook's plugins make the bridge there. It returns nil,
// having done nothing, when stateDir is empty, when the process is not in
// a user namespace (the daemon is not rootless), when rootlesskit detached
// no namespace (the daemon, and so the hook, is already in rootlesskit's),
// and when the process is already in that namespace. It reads nothing from
// stdin, which the process run again inherits.
//
// It runs the process again rather than switching this one's namespace:
// setns changes the namespace of one thread, and the plugins are started
// from whichever thread the Go runtime picks.
func EnterDetachedNetns(stateDir string) error {
	if stateDir == "" || !runningInUserNS() {
		return nil
	}
	path := filepath.Join(stateDir, "netns")
	var target, self unix.Stat_t
	err := unix.Stat(path, &target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find rootlesskit's network namespace: %w", err)
	}
	err = unix.Stat("/proc/self/ns/net", &self)
	if err != nil {
		return fmt.Errorf("find this process's network namespace: %w", err)
	}
	if target.Dev == self.Dev && target.Ino == self.Ino {
		return nil
	}
	// The thread that calls setns must be the one that calls execve, whose
	// namespace the new program starts in.
	runtime.LockOSThread()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open rootlesskit's network namespace: %w", err)
	}
	err = unix.Setns(fd, unix.CLONE_NEWNET)
	if err != nil {
		return fmt.Errorf("enter rootlesskit's network namespace: %w", err)
	}
	err = syscall.Exec("/proc/self/exe", os.Args, os.Environ())
	return fmt.Errorf("run again in rootlesskit's network namespace: %w", err)
}

// ErrRootlessKitNotRunning is the error RootlessKitDNS returns when no
// rootlesskit answers on the state directory's API socket: the socket is
// gone, or nothing listens on it. rootlesskit serves the socket for as long
// as it runs, so a state directory like that is one a rootlesskit that
// stopped left behind, and no rootless daemon runs under it.
var ErrRootlessKitNotRunning = errors.New("rootlesskit is not running")

// rootlessKitInfo is the part of the answer to rootlesskit's GET /v1/info
// that hull reads.
type rootlessKitInfo struct {
	NetworkDriver struct {
		DNS []string `json:"dns"`
	} `json:"networkDriver"`
}

// RootlessKitDNS returns the nameservers the network driver of the
// rootlesskit whose state directory is stateDir provides, such as
// slirp4netns's 10.0.2.3, by asking its API socket. They forward to the
// host's own resolver, so they answer even when the host's nameservers
// are on its loopback.
func RootlessKitDNS(ctx context.Context, stateDir string) ([]string, error) {
	sock := filepath.Join(stateDir, "api.sock")
	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://rootlesskit/v1/info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("ask rootlesskit at %s for its DNS: %w: %w", sock, ErrRootlessKitNotRunning, err)
	}
	if err != nil {
		return nil, fmt.Errorf("ask rootlesskit at %s for its DNS: %w", sock, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ask rootlesskit at %s for its DNS: %s", sock, resp.Status)
	}
	var info rootlessKitInfo
	err = json.NewDecoder(resp.Body).Decode(&info)
	if err != nil {
		return nil, fmt.Errorf("read rootlesskit's answer from %s: %w", sock, err)
	}
	return info.NetworkDriver.DNS, nil
}
