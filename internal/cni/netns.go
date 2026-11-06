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
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// An instance's network namespace goes when its last process does, and
// postStop runs after that. The bridge plugin learns the instance's
// addresses from the interface in the namespace, and with no namespace to
// look in it releases the address but leaves the masquerade rule it added
// for it, one more for every run. So the namespace is pinned by a bind
// mount while the instance is attached, as CRI pins a pod's, and the
// detach is given the pin.

// PinNetns bind-mounts the network namespace at netns, such as
// /proc/<pid>/ns/net, onto target, which it creates, so the namespace
// outlives its processes until UnpinNetns. A pin left at target by an
// instance whose detach never ran is replaced.
func PinNetns(netns, target string) error {
	err := UnpinNetns(target)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("pin the network namespace: %w", err)
	}
	_ = f.Close()
	err = unix.Mount(netns, target, "", unix.MS_BIND, "")
	if err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("pin the network namespace %s at %s: %w", netns, target, err)
	}
	return nil
}

// UnpinNetns releases a pin PinNetns made at target and removes the file.
// It does nothing when there is none.
func UnpinNetns(target string) error {
	if Pinned(target) {
		err := unix.Unmount(target, unix.MNT_DETACH)
		if err != nil {
			return fmt.Errorf("release the network namespace at %s: %w", target, err)
		}
	}
	err := os.Remove(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release the network namespace at %s: %w", target, err)
	}
	return nil
}

// Pinned reports whether target is a network namespace pinned by
// PinNetns, rather than a plain file left where a pin was.
func Pinned(target string) bool {
	var st unix.Statfs_t
	err := unix.Statfs(target, &st)
	return err == nil && st.Type == unix.NSFS_MAGIC
}
