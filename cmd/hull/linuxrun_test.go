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
	"strings"
	"testing"
)

// What brig sends, in both network modes, must pass and come out as hvi on a
// block rootfs.
func TestCheckLinuxRunAcceptsWhatBrigSends(t *testing.T) {
	for _, r := range []runRequest{
		{hypervisor: "hvi", rootfsType: "block", netMode: "none"},
		{hypervisor: "hvi", rootfsType: "block", netMode: "shared", gatewaySock: "/tmp/gw.sock"},
		{netMode: "none"},
	} {
		got, err := checkLinuxRun(r)
		if err != nil {
			t.Errorf("%+v refused: %v", r, err)
			continue
		}
		if got.hypervisor != "hvi" || got.rootfsType != "block" {
			t.Errorf("%+v came out as %q/%q, want hvi/block", r, got.hypervisor, got.rootfsType)
		}
	}
}

// Each refusal has to name the thing that was refused, so the operator knows
// which flag to drop.
func TestCheckLinuxRunRefusesWhatLinuxCannotRun(t *testing.T) {
	for _, tc := range []struct {
		r    runRequest
		want string
	}{
		{runRequest{hypervisor: "vz", netMode: "none"}, "--hypervisor hvi"},
		{runRequest{hypervisor: "qemu", netMode: "none"}, "--hypervisor hvi"},
		{runRequest{rootfsType: "virtiofs", netMode: "none"}, "--rootfs-type block"},
		{runRequest{rootfsType: "9pfs", netMode: "none"}, "--rootfs-type block"},
		{runRequest{netMode: "none", sharedDirs: 1}, "--shared-dir"},
		{runRequest{netMode: "none", sharedDirFds: 1}, "--shared-dir-fd"},
		{runRequest{netMode: "none", gui: true}, "--gui"},
		{runRequest{netMode: "none", rosetta: true}, "--rosetta"},
		{runRequest{netMode: "shared"}, "--gateway-sock"},
	} {
		_, err := checkLinuxRun(tc.r)
		if err == nil {
			t.Errorf("%+v was accepted", tc.r)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: error %q does not mention %q", tc.r, err, tc.want)
		}
	}
}
