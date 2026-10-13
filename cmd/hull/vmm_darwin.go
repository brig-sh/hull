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
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// vzVmm names the Virtualization.framework backend. urunc declares it only in
// its darwin files, so code shared with the Linux build spells it through this
// constant instead; on macOS it is urunc's own value, which keeps a typo in
// the Linux copy from compiling here unnoticed (see vmm_linux.go).
const vzVmm = hypervisors.VzVmm

// newVMM is urunc's monitor factory. On macOS it already resolves vz-runner,
// hvi and qemu next to the hull executable and builds their argv, so hull has
// nothing to add.
func newVMM(vmmType hypervisors.VmmType, monitors map[string]types.MonitorConfig) (types.VMM, error) {
	return hypervisors.NewVMM(vmmType, monitors)
}

// setVzQMPSocket hands vz-runner the per-instance control socket path. The
// type it asserts on exists only in urunc's darwin build.
func setVzQMPSocket(vmm types.VMM, sock string) {
	if vzD, ok := vmm.(*hypervisors.VzDarwin); ok {
		vzD.SetQMPSocket(sock)
	}
}
