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

import "os"

// hypervisorAvailable reports whether this user can open /dev/kvm read-write,
// which is what hvi does first. A user not in the kvm group finds out here,
// before the source fences anything, rather than at the restore.
func hypervisorAvailable() (bool, string) {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false, "/dev/kvm: " + err.Error()
	}
	_ = f.Close()
	return true, "/dev/kvm"
}
