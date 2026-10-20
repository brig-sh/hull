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
	"errors"
	"strconv"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// vzVmmName is urunc's name for the Virtualization.framework backend, spelled
// out for the Linux build, where urunc does not declare it.
// hviargv_darwin_test.go pins it to urunc's own value.
const vzVmmName = "vz"

// hviLinuxArgv builds the hvi command line hull runs on Linux.
//
// It is hull's own copy of urunc's HviDarwin.BuildExecCmd with the virtio-fs
// shares taken out, rather than a call into urunc's Linux Hvi monitor. That
// one is written for the containerd shim, not for hull: it passes no
// --agent-sock, so hull exec has nothing to dial; no --net-gateway, so the
// guest gets hvi's stub NIC instead of the gateway's network and policy; and
// no --net-mac, so every guest presents the same default MAC and the gateway,
// which keys its leases by it, hands two sandboxes the same address. hull's
// guests should get one hvi command line on every host, so the argv here
// matches the darwin one flag for flag, shares aside. Patching urunc would
// fork a dependency hull pins at a branch tip for a contract that belongs to
// hull; mirroring the dozen lines keeps the contract here, and
// hviargv_darwin_test.go compares the two.
//
// Shares are refused rather than dropped: hvi's KVM backend has no virtio-fs
// server (it parses --share-* and configures nothing), so a guest that asked
// for a host directory would boot without it and fail somewhere far from
// here. --fs-uid and --fs-gid only describe shares, so they go too.
func hviLinuxArgv(binary string, args types.ExecArgs) ([]string, error) {
	kernel := args.KernelPath
	if kernel == "" {
		kernel = args.UnikernelPath
	}
	if kernel == "" {
		return nil, errors.New("hvi: no kernel image to boot")
	}
	if args.Sharedfs.Path != "" || len(args.SharedDirs) > 0 {
		return nil, errors.New("hvi on Linux has no virtio-fs server, so it cannot share a host " +
			"directory with the guest; use --rootfs-type block and no --shared-dir")
	}
	// A bare --net on hvi is its stub NIC, which forwards nothing. On macOS
	// that path exists for vmnet; on Linux there is no vmnet and no reason to
	// boot a guest whose network silently goes nowhere.
	if args.Net.TapDev != "" && args.Net.UnixSocket == "" {
		return nil, errors.New("hvi on Linux reaches the network only through a gateway: " +
			"pass --gateway-sock and --gateway-cidr, or --net none")
	}
	mem := hypervisors.DefaultMemory
	if args.MemSizeB != 0 {
		mem = args.MemSizeB / (1024 * 1024)
	}
	vcpus := args.VCPUs
	if vcpus == 0 {
		vcpus = 1
	}
	cmd := []string{
		binary, "boot",
		"--kernel", kernel,
		"--mem-mib", strconv.FormatUint(mem, 10),
		"--cpus", strconv.FormatUint(uint64(vcpus), 10),
	}
	if args.InitrdPath != "" {
		cmd = append(cmd, "--initramfs", args.InitrdPath)
	}
	if args.Command != "" {
		cmd = append(cmd, "--cmdline", args.Command)
	}
	if args.BlockDevPath != "" {
		cmd = append(cmd, "--disk", args.BlockDevPath)
	}
	if args.Net.UnixSocket != "" {
		cmd = append(cmd, "--net-gateway", args.Net.UnixSocket)
	}
	if args.Net.MAC != "" {
		cmd = append(cmd, "--net-mac", args.Net.MAC)
	}
	if args.AgentSockPath != "" {
		cmd = append(cmd, "--agent-sock", args.AgentSockPath)
	}
	if args.ContainerID != "" {
		cmd = append(cmd, "--sandbox-id", args.ContainerID)
	}
	return cmd, nil
}
