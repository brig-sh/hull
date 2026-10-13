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

// hviLinuxArgv is a copy of urunc's darwin builder minus shares, kept so hvi
// has one command-line contract on every host hull runs it on. This compares
// the two on the argument sets below, so it catches a flag urunc's darwin argv
// adds, drops or reorders for them; a flag added under some other condition
// needs a case here before it is caught.
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

// vmm_linux.go spells the vz backend through vzVmmName, because urunc
// declares it only here. Pin that constant to urunc's value.
func TestLinuxVzNameMatchesUrunc(t *testing.T) {
	if hypervisors.VzVmm != vzVmmName {
		t.Fatalf("urunc's VzVmm is %q but vzVmmName is %q; update vzVmmName in hviargv.go",
			hypervisors.VzVmm, vzVmmName)
	}
}

// --fs-uid and --fs-gid describe virtio-fs shares to hvi's file server, so the
// Linux argv, which has no shares, drops them where urunc's darwin builder
// would add them. Everything else must still match, in order.
func TestHviLinuxArgvDropsOnlyTheShareOwnerFlags(t *testing.T) {
	args := types.ExecArgs{
		ContainerID:  "t1",
		KernelPath:   "/k",
		BlockDevPath: "/s/rootfs.ext4",
		GuestUID:     1000,
		GuestGID:     1000,
		Net:          types.NetDevParams{UnixSocket: "/g/gw.sock", MAC: "52:54:00:aa:bb:cc"},
	}
	darwin, err := hypervisors.NewHviDarwin("/opt/hull/hvi").BuildExecCmd(args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	dropped := 0
	for i := 0; i < len(darwin); i++ {
		if darwin[i] == "--fs-uid" || darwin[i] == "--fs-gid" {
			i++ // and its value
			dropped++
			continue
		}
		want = append(want, darwin[i])
	}
	if dropped != 2 {
		t.Fatalf("urunc's darwin argv no longer carries --fs-uid and --fs-gid for a "+
			"non-root guest; re-check what hviLinuxArgv should drop: %q", darwin)
	}
	linux, err := hviLinuxArgv("/opt/hull/hvi", args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(linux, want) {
		t.Errorf("argv drifted from urunc's darwin builder:\n linux  %q\n darwin %q (minus the share owner)", linux, want)
	}
}
