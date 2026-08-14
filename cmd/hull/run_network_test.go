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
