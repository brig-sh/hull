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

	"golang.org/x/sys/unix"
)

// requireSameAPFSVolume refuses an hvi writable rootfs unless the image store
// and the instance directory are one APFS volume, which is what makes the
// per-instance clone in cloneRootfsAPFS a clone rather than a full copy.
func requireSameAPFSVolume(source, destinationParent string) error {
	var sourceFS, destinationFS unix.Statfs_t
	if err := unix.Statfs(source, &sourceFS); err != nil {
		return fmt.Errorf("inspect source filesystem for APFS clone: %w", err)
	}
	if err := unix.Statfs(destinationParent, &destinationFS); err != nil {
		return fmt.Errorf("inspect destination filesystem for APFS clone: %w", err)
	}
	fsName := func(raw [16]byte) string {
		for i, value := range raw {
			if value == 0 {
				return string(raw[:i])
			}
		}
		return string(raw[:])
	}
	if fsName(sourceFS.Fstypename) != "apfs" || fsName(destinationFS.Fstypename) != "apfs" {
		return fmt.Errorf("HVI writable rootfs requires an APFS image store and instance directory")
	}
	if sourceFS.Fsid.Val != destinationFS.Fsid.Val {
		return fmt.Errorf("HVI writable rootfs requires the image store and instance directory on the same APFS volume")
	}
	return nil
}
