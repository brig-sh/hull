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
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// The flags urunc's Linux hvi monitor leaves out are the ones this builder
// exists for: without --agent-sock there is no hull exec, without
// --net-gateway the guest has no network or policy, and without --net-mac two
// guests share a lease.
func TestHviLinuxArgvCarriesAgentGatewayAndMAC(t *testing.T) {
	got, err := hviLinuxArgv("/opt/hull/hvi", types.ExecArgs{
		ContainerID:   "t1",
		KernelPath:    "/s/i/t1/kernel",
		InitrdPath:    "/s/i/t1/initrd",
		BlockDevPath:  "/s/i/t1/bundle/rootfs.ext4",
		AgentSockPath: "/s/i/t1/agent.sock",
		Command:       "rdinit=/init console=ttyAMA0",
		MemSizeB:      1024 << 20,
		VCPUs:         2,
		Net:           types.NetDevParams{UnixSocket: "/g/gw.sock.qemu", MAC: "52:54:00:01:02:03"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/opt/hull/hvi", "boot",
		"--kernel", "/s/i/t1/kernel",
		"--mem-mib", "1024",
		"--cpus", "2",
		"--initramfs", "/s/i/t1/initrd",
		"--cmdline", "rdinit=/init console=ttyAMA0",
		"--disk", "/s/i/t1/bundle/rootfs.ext4",
		"--net-gateway", "/g/gw.sock.qemu",
		"--net-mac", "52:54:00:01:02:03",
		"--agent-sock", "/s/i/t1/agent.sock",
		"--sandbox-id", "t1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}
}

func TestHviLinuxArgvRefusesWhatKVMHviCannotServe(t *testing.T) {
	base := types.ExecArgs{KernelPath: "/k"}

	withShare := base
	withShare.SharedDirs = []types.SharedDirParams{{Path: "/home/u/src", Tag: "share0"}}
	if _, err := hviLinuxArgv("hvi", withShare); err == nil || !strings.Contains(err.Error(), "virtio-fs") {
		t.Errorf("a shared directory was not refused: %v", err)
	}

	withRootShare := base
	withRootShare.Sharedfs = types.SharedfsParams{Path: "/s/rootfs", Tag: "rootfs"}
	if _, err := hviLinuxArgv("hvi", withRootShare); err == nil {
		t.Error("a virtio-fs rootfs was not refused")
	}

	stubNet := base
	stubNet.Net = types.NetDevParams{TapDev: "en0"}
	if _, err := hviLinuxArgv("hvi", stubNet); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("a NIC with no gateway was not refused: %v", err)
	}

	if _, err := hviLinuxArgv("hvi", types.ExecArgs{}); err == nil {
		t.Error("an argv with no kernel was built")
	}
}
