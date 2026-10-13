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

import "errors"

// requireSameAPFSVolume always refuses on Linux. The hvi writable-directory
// rootfs is an APFS clone served over virtio-fs, and Linux has neither: no
// APFS, and no virtio-fs server in hvi's KVM backend. checkLinuxRun refuses
// that mode before anything reaches here; this is the backstop, so that a
// path which skipped the preflight fails instead of falling back to a full
// copy of the image.
func requireSameAPFSVolume(_, _ string) error {
	return errors.New("the hvi writable-directory rootfs needs APFS and virtio-fs; " +
		"on Linux use --rootfs-type block")
}
