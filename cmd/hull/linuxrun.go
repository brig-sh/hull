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
	"fmt"
)

// runRequest is the part of a `hull run` command line that decides whether
// this host can run it at all, read before anything is pulled or created.
type runRequest struct {
	hypervisor   string
	rootfsType   string
	netMode      string
	gatewaySock  string
	sharedDirs   int
	sharedDirFds int
	gui          bool
	rosetta      bool
}

// checkLinuxRun narrows a run to what hull on Linux can deliver, and fills in
// the Linux defaults: hvi, with a block rootfs.
//
// Linux hull exists to be the other end of a moved hvi sandbox, so it offers
// the one configuration that can be moved and refuses the rest by name before
// anything is pulled -- a refusal after a multi-gigabyte pull and a 15 GiB
// image build is the same answer, late. What it refuses, and why:
//
//   - any backend but hvi: vz and the macOS qemu builder do not exist here.
//   - --rootfs-type virtiofs and 9pfs: hvi's KVM backend has no file server,
//     so a directory rootfs would boot with no root at all.
//   - --shared-dir and --shared-dir-fd: the same missing file server.
//   - --net shared without --gateway-sock: on macOS that is vmnet, and here it
//     would be hvi's stub NIC, which forwards nothing, plus a wait on macOS
//     DHCP leases that never come. The gateway is the network on Linux.
//   - --gui and --rosetta: Virtualization.framework features.
func checkLinuxRun(r runRequest) (runRequest, error) {
	switch r.hypervisor {
	case "", "hvi":
		r.hypervisor = "hvi"
	default:
		return r, fmt.Errorf("hull on Linux runs only --hypervisor hvi (got %q)", r.hypervisor)
	}
	switch r.rootfsType {
	case "", "block":
		r.rootfsType = "block"
	default:
		return r, fmt.Errorf("hull on Linux runs only --rootfs-type block (got %q): hvi's KVM "+
			"backend has no virtio-fs server to serve a directory rootfs", r.rootfsType)
	}
	if r.sharedDirs > 0 || r.sharedDirFds > 0 {
		return r, errors.New("hull on Linux cannot share host directories (--shared-dir, " +
			"--shared-dir-fd): hvi's KVM backend has no virtio-fs server")
	}
	if r.gui {
		return r, errors.New("--gui needs Virtualization.framework and is macOS only")
	}
	if r.rosetta {
		return r, errors.New("--rosetta needs Virtualization.framework and is macOS only")
	}
	if r.netMode != "none" && r.gatewaySock == "" {
		return r, fmt.Errorf("hull on Linux reaches the network only through a gateway: "+
			"pass --gateway-sock and --gateway-cidr, or --net none (got --net %s)", r.netMode)
	}
	return r, nil
}
