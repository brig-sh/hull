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

import "golang.org/x/sys/unix"

// hostHypervisorAvailable reports whether this Mac can run hvi guests:
// Hypervisor.framework answers kern.hv_support with 1 when the CPU and the
// OS support it. Whether this hull may use it is a matter of hvi's
// entitlement, which the restore itself will find out.
func hostHypervisorAvailable() (bool, string) {
	v, err := unix.SysctlUint32("kern.hv_support")
	if err != nil {
		return false, "kern.hv_support: " + err.Error()
	}
	if v != 1 {
		return false, "kern.hv_support is 0: Hypervisor.framework is not available"
	}
	return true, "Hypervisor.framework"
}
