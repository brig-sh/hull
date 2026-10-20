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

// What brig sends, in both network modes, must pass and come out on a block
// rootfs. An unset backend stays unset, so the image's annotation still gets
// its say once the image is pulled.
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
		if got.hypervisor != r.hypervisor || got.rootfsType != "block" {
			t.Errorf("%+v came out as %q/%q, want %q/block", r, got.hypervisor, got.rootfsType, r.hypervisor)
		}
	}
}

// The flag wins over the image's annotation, which wins over the platform's
// default, on both platforms.
func TestSelectHypervisorPrecedence(t *testing.T) {
	qemuImage := map[string]string{hypervisorAnnotation: "qemu"}
	for _, tc := range []struct {
		flag        string
		annotations map[string]string
		def         string
		want, src   string
	}{
		{"vz", qemuImage, "qemu", "vz", "flag"},
		{"", qemuImage, "qemu", "qemu", "annotation"},
		{"", nil, "qemu", "qemu", "default"},
		{"hvi", qemuImage, "hvi", "hvi", "flag"},
		{"", qemuImage, "hvi", "qemu", "annotation"},
		{"", map[string]string{"other": "x"}, "hvi", "hvi", "default"},
	} {
		name, src := selectHypervisor(tc.flag, tc.annotations, tc.def)
		if name != tc.want || src != tc.src {
			t.Errorf("selectHypervisor(%q, %v, %q) = %q from %s, want %q from %s",
				tc.flag, tc.annotations, tc.def, name, src, tc.want, tc.src)
		}
	}
}

// An image annotated for another backend is refused on Linux, naming the
// annotation, rather than booted on hvi without a word. The flag still
// overrides it, and the Linux default passes.
func TestLinuxRefusesAnImageAnnotatedForAnotherBackend(t *testing.T) {
	name, src := selectHypervisor("", map[string]string{hypervisorAnnotation: "qemu"}, "hvi")
	err := checkLinuxHypervisor(name, src)
	if err == nil {
		t.Fatal("an image annotated for qemu was accepted on Linux")
	}
	if !strings.Contains(err.Error(), hypervisorAnnotation) || !strings.Contains(err.Error(), "--hypervisor hvi") {
		t.Errorf("refusal %q names neither the annotation nor the way out", err)
	}
	for _, flag := range []string{"hvi", ""} {
		name, src := selectHypervisor(flag, nil, "hvi")
		if err := checkLinuxHypervisor(name, src); err != nil {
			t.Errorf("selectHypervisor(%q) on Linux was refused: %v", flag, err)
		}
	}
	name, src = selectHypervisor("hvi", map[string]string{hypervisorAnnotation: "firecracker"}, "hvi")
	if err := checkLinuxHypervisor(name, src); err != nil {
		t.Errorf("--hypervisor hvi did not override the annotation: %v", err)
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
