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
	"reflect"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// hviLinuxArgv is a copy of urunc's darwin builder minus shares, and it is
// only worth anything while it stays one: a sandbox moved from a Mac to Linux
// must come up with the same devices. This fails the day urunc's darwin argv
// grows, drops or reorders a flag the Linux copy has not.
func TestHviLinuxArgvMatchesTheDarwinBuilder(t *testing.T) {
	for _, args := range []types.ExecArgs{
		{KernelPath: "/k"},
		{
			ContainerID:   "t1",
			KernelPath:    "/s/i/t1/kernel",
			InitrdPath:    "/s/i/t1/initrd",
			BlockDevPath:  "/s/i/t1/bundle/rootfs.ext4",
			AgentSockPath: "/s/i/t1/agent.sock",
			Command:       "rdinit=/init console=ttyAMA0",
			MemSizeB:      768 << 20,
			VCPUs:         3,
			Net:           types.NetDevParams{UnixSocket: "/g/gw.sock.qemu", MAC: "52:54:00:aa:bb:cc"},
		},
		{KernelPath: "/k", Net: types.NetDevParams{MAC: "52:54:00:00:00:01"}, ContainerID: "none"},
	} {
		darwin, err := hypervisors.NewHviDarwin("/opt/hull/hvi").BuildExecCmd(args, nil)
		if err != nil {
			t.Fatal(err)
		}
		linux, err := hviLinuxArgv("/opt/hull/hvi", args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(linux, darwin) {
			t.Errorf("argv drifted from urunc's darwin builder:\n linux  %q\n darwin %q", linux, darwin)
		}
	}
}

// vmm_linux.go spells the vz backend as a literal, because urunc declares it
// only here. Pin the literal to urunc's value.
func TestLinuxVzLiteralMatchesUrunc(t *testing.T) {
	if hypervisors.VzVmm != "vz" {
		t.Fatalf("urunc's VzVmm is %q; update vzVmm in vmm_linux.go to match", hypervisors.VzVmm)
	}
}
