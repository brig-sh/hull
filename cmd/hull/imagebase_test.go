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
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Every instance of an image starts from one base, built once: the base
// holds the image and nothing of an instance, and each disk is the base
// with that instance's own files written in. Another size is another base,
// and a local image, which has no digest of its content, gets a base of
// its own every time.
func TestImageBaseIsSharedByAnImagesInstances(t *testing.T) {
	if _, err := os.Stat(mke2fsBin); err != nil {
		t.Skipf("needs e2fsprogs: %v", err)
	}
	s := shortStore(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "bin", "sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := func(i int) []byte { return []byte("API_KEY=secret-of-instance-" + string(rune('a'+i)) + "\n") }
	var bases []string
	for i := 0; i < 2; i++ {
		disk := filepath.Join(t.TempDir(), "rootfs.ext4")
		sum, err := diskFromImageBase(s, "sha256:feed", disk, rootfs,
			[]blockInject{{guestPath: "/urunit.conf", content: secret(i), mode: 0o600}}, 32)
		if err != nil {
			t.Fatal(err)
		}
		bases = append(bases, sum)
		data, err := os.ReadFile(disk)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, secret(i)) {
			t.Errorf("instance %d's disk lacks its own file", i)
		}
		if info, err := os.Stat(disk); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("instance %d's disk: %v %v", i, info.Mode(), err)
		}
	}
	if bases[0] != bases[1] {
		t.Fatalf("two instances of one image got bases %s and %s", bases[0], bases[1])
	}
	base, err := os.ReadFile(s.BasePath(bases[0]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(base, []byte("API_KEY")) {
		t.Error("the base holds an instance's /urunit.conf")
	}
	if got, _, err := diskDigestFile(s.BasePath(bases[0])); err != nil || got != bases[0] {
		t.Errorf("the base hashes to %s, %v", got, err)
	}
	if e, err := readImageBaseEntry(s, imageBaseKey("sha256:feed", 32)); err != nil || e == nil ||
		e.Digest != bases[0] || e.Image != "sha256:feed" {
		t.Errorf("entry %+v, %v", e, err)
	}
	if sum, built, err := imageBase(s, "sha256:feed", rootfs, 32); err != nil || built || sum != bases[0] {
		t.Errorf("the image's base again: %s built %v, %v", sum, built, err)
	}
	if sum, built, err := imageBase(s, "sha256:feed", rootfs, 48); err != nil || !built || sum == bases[0] {
		t.Errorf("another size: %s built %v, %v", sum, built, err)
	}
	for i := 0; i < 2; i++ {
		if _, built, err := imageBase(s, "local:/some/bundle", rootfs, 32); err != nil || !built {
			t.Errorf("a local image's base was not built afresh: %v, %v", built, err)
		}
	}
	// A base gone from the store is built again, and recorded.
	if err := os.Chmod(s.BasePath(bases[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.BasePath(bases[0])); err != nil {
		t.Fatal(err)
	}
	if sum, built, err := imageBase(s, "sha256:feed", rootfs, 32); err != nil || !built || !baseHeld(s, sum) {
		t.Errorf("a lost base: %s built %v, %v", sum, built, err)
	}
}
