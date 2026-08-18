// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

func TestContainerBootResolver(t *testing.T) {
	tests := []struct {
		name        string
		vmm         hypervisors.VmmType
		netMode     string
		gatewayCIDR string
		want        string
		wantErr     bool
	}{
		{name: "gateway", vmm: hypervisors.HviVmm, netMode: "shared", gatewayCIDR: "10.87.0.9/24", want: "10.87.0.1"},
		{name: "hvi built in", vmm: hypervisors.HviVmm, netMode: "shared", want: "10.0.2.3"},
		{name: "hvi disabled", vmm: hypervisors.HviVmm, netMode: "none"},
		{name: "vz dhcp", vmm: hypervisors.VzVmm, netMode: "shared"},
		{name: "invalid gateway", vmm: hypervisors.HviVmm, netMode: "shared", gatewayCIDR: "bad", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containerBootResolver(tt.vmm, tt.netMode, tt.gatewayCIDR)
			if (err != nil) != tt.wantErr {
				t.Fatalf("containerBootResolver error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("containerBootResolver = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVzNetArgsCarriesNetNoneToTheRunner is defect 5 on the Go side.
//
// The other backends take "no network" by omission -- no netdev, no interface.
// vz-runner cannot: its default when no --net-fd is given is Apple's NAT, so
// leaving netParams.TapDev empty told it nothing and `hull run --net none`
// produced a guest with a working route to the internet. An agent sandbox that
// is not offline when it was asked to be offline is the failure worth naming.
func TestVzNetArgsCarriesNetNoneToTheRunner(t *testing.T) {
	got := vzNetArgs("none")
	if !slices.Contains(got, "--no-net") {
		t.Fatalf("vzNetArgs(\"none\") = %v, want it to suppress the NIC", got)
	}
	for _, mode := range []string{"shared", ""} {
		if len(vzNetArgs(mode)) != 0 {
			t.Errorf("vzNetArgs(%q) = %v, want nothing: only --net none suppresses the NIC",
				mode, vzNetArgs(mode))
		}
	}
}

// TestVzRunnerHonoursNoNet reads the runner's source, which is unusual and
// deliberate.
//
// The two halves of this fix live in different languages and are built
// separately, so the Go side can keep passing a flag the Swift side quietly
// ignores -- which is precisely the shape of the bug: hull accepted --net none,
// dropped it before the runner, and nothing failed. Nothing else in the Go
// suite can see the runner at all, and the alternative is booting a VM in a
// unit test.
func TestVzRunnerHonoursNoNet(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "vz-runner", "Sources", "main.swift"))
	if err != nil {
		t.Skipf("vz-runner source not available: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, `case "--no-net":`) {
		t.Error("vz-runner does not parse --no-net, so hull is passing a flag it ignores")
	}
	if !strings.Contains(text, "if config.noNet {") || !strings.Contains(text, "vmConfig.networkDevices = []") {
		t.Error("vz-runner does not act on --no-net by leaving the network devices empty")
	}
}
