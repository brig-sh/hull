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
	"errors"
	"fmt"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// sharedDirMounts turns the --shared-dir flags into the bind mounts of the
// instance's spec, rbind and rprivate as nerdctl makes -v host:guest. urunc
// takes each into the guest on the roots containerd.CheckShares allows. Its
// ro is not recursive, as nerdctl's rro would be: urunc remounts only the
// top of the bind read-only (applyMount in pkg/unikontainers/mount_linux.go).
//
// The host path is made absolute here: containerd's shim resolves a
// relative source from its own working directory, not hull's.
func sharedDirMounts(values []string) ([]specs.Mount, error) {
	var mounts []specs.Mount
	taken := map[string]bool{}
	for _, spec := range values {
		parts, readOnly, err := splitShareSpec("shared-dir", spec)
		if err != nil {
			return nil, err
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid shared-dir %q, expected /host/path:/guest/path[:mode]", spec)
		}
		host, err := resolveShareHost(parts[0])
		if err != nil {
			return nil, err
		}
		host, err = filepath.Abs(host)
		if err != nil {
			return nil, fmt.Errorf("shared-dir %q: %w", spec, err)
		}
		err = validateGuestPath(parts[1])
		if err != nil {
			return nil, err
		}
		err = claimGuestPath(taken, parts[1])
		if err != nil {
			return nil, err
		}
		mode := "rw"
		if readOnly {
			mode = "ro"
		}
		mounts = append(mounts, specs.Mount{
			Type:        "bind",
			Source:      host,
			Destination: parts[1],
			Options:     []string{"rbind", "rprivate", mode},
		})
	}
	return mounts, nil
}

// checkNetworkShares refuses a share at /etc/resolv.conf for an instance on
// a network. hull binds the network's resolver configuration at that path
// after the shares (withNetwork in internal/containerd/run.go), so the
// share would be hidden, and urunc takes the guest's nameserver from the
// last mount there (resolvConfMount in pkg/unikontainers/utils.go).
func checkNetworkShares(mounts []specs.Mount) error {
	for _, m := range mounts {
		if filepath.Clean(m.Destination) == "/etc/resolv.conf" {
			return errors.New("--shared-dir cannot share at /etc/resolv.conf with --net: hull puts the " +
				"network's resolver configuration there")
		}
	}
	return nil
}
