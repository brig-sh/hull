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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
)

func sharedDirFlag() cli.Flag {
	return &cli.StringSliceFlag{
		Name:  "shared-dir",
		Usage: "share a host directory with the guest: /host/path:/guest/path[:ro|rw] (repeatable)",
	}
}

// splitShareSpec pulls the optional access mode off a share and returns what
// is left of it. Both flags come through here, so each carries its own name:
// an error naming a flag the caller did not pass sends them looking in the
// wrong place.
func splitShareSpec(flag, spec string) ([]string, bool, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return parts, false, nil
	}
	switch parts[2] {
	case "ro":
		return parts[:2], true, nil
	case "rw":
		return parts[:2], false, nil
	}
	return nil, false, fmt.Errorf("invalid %s mode %q in %q (want ro or rw)", flag, parts[2], spec)
}

// resolveShareHost returns the directory a --shared-dir's host path names,
// with ~ expanded, or why it cannot be shared.
func resolveShareHost(hostPath string) (string, error) {
	if !strings.HasPrefix(hostPath, "/") && !strings.HasPrefix(hostPath, ".") && !strings.HasPrefix(hostPath, "~") {
		return "", fmt.Errorf("named volumes are not supported (%q): only bind mounts of host paths", hostPath)
	}
	if strings.HasPrefix(hostPath, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to expand home directory: %w", err)
		}
		hostPath = filepath.Join(home, hostPath[1:])
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		return "", fmt.Errorf("shared directory not found: %s", hostPath)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file: file bind mounts are not supported (virtiofs/9p share directories)", hostPath)
	}
	return hostPath, nil
}

func validateGuestPath(guestPath string) error {
	if !strings.HasPrefix(guestPath, "/") {
		return fmt.Errorf("guest path %q must be absolute", guestPath)
	}
	// A share mounted over / would hide the guest's whole root, as docker
	// refuses a mount whose destination is /.
	if filepath.Clean(guestPath) == "/" {
		return fmt.Errorf("guest path %q is the guest's root, which a share cannot replace", guestPath)
	}
	return nil
}

// claimGuestPath records guestPath in taken, and refuses one an earlier
// share of the run took: the later mount would hide the earlier, which
// docker refuses as a duplicate mount point. Paths are compared clean, so
// /mnt and /mnt/ are one.
func claimGuestPath(taken map[string]bool, guestPath string) error {
	path := filepath.Clean(guestPath)
	if taken[path] {
		return fmt.Errorf("guest path %s is shared twice: two shares cannot be mounted at one path", path)
	}
	taken[path] = true
	return nil
}
