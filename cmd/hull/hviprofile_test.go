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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const portable = "portable-arm64-v1"

func TestResolveRunProfile(t *testing.T) {
	cases := []struct {
		flag        string
		annotations map[string]string
		want        string
		refusal     string
	}{
		{flag: "", want: ""},
		{flag: "native", want: ""},
		{flag: portable, want: portable},
		{annotations: map[string]string{hviProfileAnnotation: portable}, want: portable},
		{flag: portable, annotations: map[string]string{hviProfileAnnotation: portable}, want: portable},
		{flag: "native", annotations: map[string]string{hviProfileAnnotation: portable},
			refusal: "different profiles"},
		{flag: "portable", refusal: "unknown machine profile"},
		{annotations: map[string]string{hviProfileAnnotation: "x"}, refusal: hviProfileAnnotation},
	}
	for _, c := range cases {
		got, err := resolveRunProfile(c.flag, c.annotations)
		if c.refusal != "" {
			if err == nil || !strings.Contains(err.Error(), c.refusal) {
				t.Errorf("%q %v: err = %v, want %q", c.flag, c.annotations, err, c.refusal)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q %v = %q, %v; want %q", c.flag, c.annotations, got, err, c.want)
		}
	}
}

func TestCheckProfileRun(t *testing.T) {
	block := runRequest{hypervisor: "hvi", rootfsType: "block"}
	if err := checkProfileRun(portable, block); err != nil {
		t.Fatalf("hvi with a block rootfs was refused: %v", err)
	}
	// No --hypervisor: the image annotation or the platform picks the
	// backend, and checkProfileBackend checks it once known.
	if err := checkProfileRun(portable, runRequest{rootfsType: "block"}); err != nil {
		t.Fatalf("an unset --hypervisor was refused before the image was read: %v", err)
	}
	for _, c := range []struct {
		r       runRequest
		refusal string
	}{
		{runRequest{hypervisor: "vz", rootfsType: "block"}, "--hypervisor is vz"},
		{runRequest{hypervisor: "qemu", rootfsType: "block"}, "--hypervisor is qemu"},
		{runRequest{hypervisor: "hvi", rootfsType: "virtiofs"}, "got virtiofs"},
		{runRequest{hypervisor: "hvi"}, "virtiofs, the default"},
		{runRequest{hypervisor: "hvi", rootfsType: "block", sharedDirs: 1}, "cannot share"},
		{runRequest{hypervisor: "hvi", rootfsType: "block", sharedDirFds: 1}, "cannot share"},
	} {
		err := checkProfileRun(portable, c.r)
		if err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%+v: err = %v, want %q", c.r, err, c.refusal)
		}
		if err := checkProfileRun("", c.r); err != nil {
			t.Errorf("a native run was refused by the profile check: %v", err)
		}
	}
	if err := checkProfileBackend(portable, "vz", "image annotation"); err == nil ||
		!strings.Contains(err.Error(), "image annotation") {
		t.Errorf("an image that picks vz: err = %v", err)
	}
	if err := checkProfileBackend(portable, "hvi", "flag"); err != nil {
		t.Errorf("hvi refused: %v", err)
	}
	if err := checkProfileBackend("", "vz", "flag"); err != nil {
		t.Errorf("native vz refused: %v", err)
	}
}

// A native run's hvi command line is the one an hvi from before profiles
// accepts; a portable run adds --profile only when the hvi takes it, and
// otherwise refuses rather than boot the native machine.
func TestHviProfileArgs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("old", `echo 'hvi: unknown boot arg "--profile"' >&2; exit 1`)
	current := write("current", `echo 'hvi: --profile needs a name' >&2; exit 1`)

	if args, err := hviProfileArgs(old, ""); err != nil || args != nil {
		t.Fatalf("native: %v %v", args, err)
	}
	if args, err := hviProfileArgs(current, portable); err != nil ||
		!slices.Equal(args, []string{"--profile", portable}) {
		t.Fatalf("portable: %v %v", args, err)
	}
	if _, err := hviProfileArgs(old, portable); err == nil || !strings.Contains(err.Error(), old) {
		t.Fatalf("an hvi without profiles: err = %v, want a refusal naming it", err)
	}
}
