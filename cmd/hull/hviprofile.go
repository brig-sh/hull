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
	"fmt"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// hviProfileAnnotation selects the hvi machine profile, as --profile does.
//
// It is read from this invocation's --annotation flags only, never from the
// image's metadata or urunc.json, for the same reason as bootKernel: the
// profile decides which host kernel boots and what CPU the guest is shown,
// and an image must not choose either. That is also how brig passes it.
const hviProfileAnnotation = "com.nofire.hvi.profile"

// resolveRunProfile combines --profile and the profile annotation into the
// profile hull records: "" for hvi's native profile. The two may both be
// given, but only for the same profile.
func resolveRunProfile(flag string, invocationAnnotations map[string]string) (string, error) {
	fromFlag, err := bootassets.NormalizeProfile(flag)
	if err != nil {
		return "", fmt.Errorf("--profile: %w", err)
	}
	annotation, set := invocationAnnotations[hviProfileAnnotation]
	if !set {
		return fromFlag, nil
	}
	fromAnnotation, err := bootassets.NormalizeProfile(annotation)
	if err != nil {
		return "", fmt.Errorf("annotation %s: %w", hviProfileAnnotation, err)
	}
	if flag != "" && fromFlag != fromAnnotation {
		return "", fmt.Errorf("--profile %s and annotation %s=%s name different profiles",
			flag, hviProfileAnnotation, annotation)
	}
	return fromAnnotation, nil
}

// checkProfileRun refuses a run the profile cannot describe, before the
// image is pulled. hypervisor and rootfsType are the flags as the platform
// preflight left them, so on Linux an unset --rootfs-type is already block.
//
// The portable profile is an hvi machine with one rootfs disk: its guest is
// the same machine on HVF and on KVM because nothing in it lives on the
// host. A virtio-fs rootfs or share is a host directory, which neither a
// checkpoint nor the other host carries, so they are refused rather than
// left to fail the first checkpoint. An unset --rootfs-type on macOS is
// virtiofs for hvi, so it is refused too, not switched to block behind the
// caller's back.
func checkProfileRun(profile string, r runRequest) error {
	if profile == "" {
		return nil
	}
	if r.hypervisor != "" && r.hypervisor != string(hypervisors.HviVmm) {
		return fmt.Errorf("--profile %s is an hvi machine profile, and --hypervisor is %s",
			profile, r.hypervisor)
	}
	if r.rootfsType != "block" {
		rootfs := r.rootfsType
		if rootfs == "" {
			rootfs = "virtiofs, the default"
		}
		return fmt.Errorf("--profile %s needs --rootfs-type block (got %s): the profile's guest "+
			"keeps its root on a disk a checkpoint carries, not in a host directory", profile, rootfs)
	}
	if r.sharedDirs > 0 || r.sharedDirFds > 0 {
		return fmt.Errorf("--profile %s cannot share host directories (--shared-dir, "+
			"--shared-dir-fd): a share is a host directory no checkpoint carries", profile)
	}
	return nil
}

// checkProfileBackend is the check that waits for the image: an image's own
// annotation can choose the backend when --hypervisor is not given.
func checkProfileBackend(profile, vmmName, source string) error {
	if profile == "" || vmmName == string(hypervisors.HviVmm) {
		return nil
	}
	return fmt.Errorf("--profile %s is an hvi machine profile, and this run uses %s (from %s); "+
		"pass --hypervisor hvi", profile, vmmName, source)
}

// hviProfileArgs is what a run passes hvi for its profile: nothing for the
// native one, so its command line stays the one an hvi from before profiles
// accepts, and --profile NAME otherwise, once the hvi that will run has said
// it takes the flag.
func hviProfileArgs(binary, profile string) ([]string, error) {
	if profile == "" {
		return nil, nil
	}
	takes, err := hviBootTakesProfile(binary)
	if err != nil {
		return nil, err
	}
	if !takes {
		return nil, fmt.Errorf("--profile %s needs an hvi that takes --profile, and %s does not "+
			"(it would boot hvi's native machine)", profile, binary)
	}
	return []string{"--profile", profile}, nil
}
