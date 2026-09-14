// Copyright 2026 The hull Authors
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

package ociclient

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// The defaults are injected only when an image carries none of unikernelType,
// hypervisor or binary. An image that names a kernel and a hypervisor without
// a unikernelType therefore gets no type at all, and callers must not read
// the absence as "linux": hull's own bootable test images have this shape.
func TestNoUnikernelTypeIsInjectedBesideOtherUruncLabels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     map[string]string
		wantType   string
		wantExists bool
	}{
		{
			name: "ubuntu-simple: kernel, hypervisor and cmdline, no type",
			labels: map[string]string{
				"com.urunc.unikernel.kernel":     "/.boot/kernel",
				"com.urunc.unikernel.hypervisor": "qemu",
				"com.urunc.unikernel.cmdline":    "root=/dev/ram0 init=/init console=ttyS0",
			},
			wantExists: false,
		},
		{
			name:       "no urunc labels at all gets the linux defaults",
			labels:     map[string]string{"org.opencontainers.image.title": "x"},
			wantType:   "linux",
			wantExists: true,
		},
		{
			name:       "a declared type is kept",
			labels:     map[string]string{"com.urunc.unikernel.unikernelType": "unikraft"},
			wantType:   "unikraft",
			wantExists: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &v1.ConfigFile{}
			cfg.Config.Labels = tc.labels
			spec := createOCISpec(cfg, "rootfs")
			got, ok := spec.Annotations[UnikernelTypeAnnotation]
			if ok != tc.wantExists {
				t.Fatalf("%s present = %v, want %v (annotations: %v)",
					UnikernelTypeAnnotation, ok, tc.wantExists, spec.Annotations)
			}
			if ok && got != tc.wantType {
				t.Errorf("%s = %q, want %q", UnikernelTypeAnnotation, got, tc.wantType)
			}
		})
	}
}
