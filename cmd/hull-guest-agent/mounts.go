// Copyright (c) 2023-2026, Nubificus LTD
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

//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// configTag is the virtiofs tag of the read-only share hull passes to every
// macOS guest. It holds mounts.json, the mounts the guest should have.
const configTag = "hull-config"

// configMount is where the agent mounts the config share.
const configMount = "/private/var/run/hull-config"

// configWait bounds how long the agent waits for the config share at boot.
// A guest run by an older hull has none, and gets no mounts.
const configWait = 30 * time.Second

// guestMount is one entry of mounts.json.
type guestMount struct {
	// Kind is "virtiofs", a host directory shared under Tag, or "ram", a
	// memory-only volume of SizeMB.
	Kind     string `json:"kind"`
	Tag      string `json:"tag,omitempty"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	SizeMB   int    `json:"sizeMB,omitempty"`
}

// applyGuestMounts mounts the config share and then every entry of its
// mounts.json. It runs once at agent start. A mount that is already there,
// as after a restore, is left alone.
func applyGuestMounts() {
	if !isMountPoint(configMount) {
		if err := waitForConfigShare(); err != nil {
			log.Printf("guest mounts: %v", err)
			return
		}
	}
	data, err := os.ReadFile(filepath.Join(configMount, "mounts.json"))
	if err != nil {
		log.Printf("guest mounts: %v", err)
		return
	}
	var mounts []guestMount
	if err := json.Unmarshal(data, &mounts); err != nil {
		log.Printf("guest mounts: mounts.json: %v", err)
		return
	}
	for _, m := range mounts {
		if err := applyMount(m); err != nil {
			log.Printf("guest mounts: %s: %v", m.Path, err)
			continue
		}
		log.Printf("guest mounts: %s %s", m.Kind, m.Path)
	}
}

// waitForConfigShare mounts the config share, retrying while the virtiofs
// device comes up.
func waitForConfigShare() error {
	if err := os.MkdirAll(configMount, 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(configWait)
	for {
		err := run("/sbin/mount_virtiofs", "-r", configTag, configMount)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no %s share: %w", configTag, err)
		}
		time.Sleep(time.Second)
	}
}

// applyMount creates m.Path and mounts m on it, owned by whoever owns the
// directory it is created in.
func applyMount(m guestMount) error {
	if !filepath.IsAbs(m.Path) {
		return errors.New("path is not absolute")
	}
	if isMountPoint(m.Path) {
		return nil
	}
	uid, gid, err := parentOwner(m.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Path, 0o755); err != nil {
		return err
	}
	_ = os.Chown(m.Path, uid, gid)

	switch m.Kind {
	case "virtiofs":
		args := []string{"-u", strconv.Itoa(uid), "-g", strconv.Itoa(gid)}
		if m.ReadOnly {
			args = append(args, "-r")
		}
		return run("/sbin/mount_virtiofs", append(args, m.Tag, m.Path)...)
	case "ram":
		return mountRAM(m.Path, m.SizeMB, uid, gid)
	default:
		return fmt.Errorf("unknown mount kind %q", m.Kind)
	}
}

// mountRAM mounts a memory-only HFS+ volume of sizeMB on path. Nothing
// written there reaches the guest's disk, and it is gone at shutdown.
func mountRAM(path string, sizeMB, uid, gid int) error {
	if sizeMB <= 0 {
		sizeMB = 64
	}
	// hdiutil sizes a RAM device in 512-byte sectors.
	out, err := exec.Command("/usr/bin/hdiutil", "attach", "-nomount",
		fmt.Sprintf("ram://%d", sizeMB*2048)).Output()
	if err != nil {
		return fmt.Errorf("hdiutil attach: %w", err)
	}
	dev := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dev, "/dev/disk") {
		return fmt.Errorf("hdiutil attach returned %q", dev)
	}
	if err := run("/sbin/newfs_hfs", "-v", filepath.Base(path), dev); err != nil {
		_ = run("/usr/bin/hdiutil", "detach", dev)
		return err
	}
	if err := run("/sbin/mount_hfs", dev, path); err != nil {
		_ = run("/usr/bin/hdiutil", "detach", dev)
		return err
	}
	return os.Chown(path, uid, gid)
}

// parentOwner returns the owner of the nearest existing ancestor of path.
func parentOwner(path string) (int, int, error) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		var st syscall.Stat_t
		if err := syscall.Stat(dir, &st); err == nil {
			return int(st.Uid), int(st.Gid), nil
		}
		if dir == "/" {
			return 0, 0, errors.New("no existing ancestor")
		}
	}
}

// isMountPoint reports whether path is the root of a mounted file system.
func isMountPoint(path string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(path, &st) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// run runs a command and returns its output in the error when it fails.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // G204: fixed tools, arguments from hull's mount table
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(out)))
	}
	return nil
}
