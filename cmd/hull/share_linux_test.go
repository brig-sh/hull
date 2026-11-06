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

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestSharedDirMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	t.Chdir(work)
	for _, dir := range []string{filepath.Join(home, "proj"), filepath.Join(work, "rel")} {
		err := os.Mkdir(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(work, "file")
	err := os.WriteFile(file, nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	got, err := sharedDirMounts([]string{work + ":/workspace", "~/proj:/mnt/proj:ro", "./rel:/mnt/rel:rw",
		work + ":/work/my 'project\""})
	if err != nil {
		t.Fatal(err)
	}
	// The shim resolves a relative source from its own directory, so every
	// source is absolute.
	want := []specs.Mount{
		{Type: "bind", Source: work, Destination: "/workspace", Options: []string{"rbind", "rprivate", "rw"}},
		{Type: "bind", Source: filepath.Join(home, "proj"), Destination: "/mnt/proj", Options: []string{"rbind", "rprivate", "ro"}},
		{Type: "bind", Source: filepath.Join(work, "rel"), Destination: "/mnt/rel", Options: []string{"rbind", "rprivate", "rw"}},
		// urunc puts a guest path through no shell, so spaces and quotes
		// are taken, as nerdctl takes them.
		{Type: "bind", Source: work, Destination: "/work/my 'project\"", Options: []string{"rbind", "rprivate", "rw"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts = %+v, want %+v", got, want)
	}

	for _, tc := range []struct{ spec, want string }{
		{"data:/mnt", "named volumes are not supported"},
		{filepath.Join(work, "missing") + ":/mnt", "shared directory not found"},
		{file + ":/mnt", "is a file"},
		{work + ":mnt", `guest path "mnt" must be absolute`},
		{work + ":/", "is the guest's root"},
		{work + ":/./", "is the guest's root"},
		{work + ":/mnt:rx", `invalid shared-dir mode "rx"`},
		{work, "expected /host/path:/guest/path[:mode]"},
		{work + ":/a:/b:ro", "expected /host/path:/guest/path[:mode]"},
	} {
		_, err := sharedDirMounts([]string{tc.spec})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error = %v, want %q", tc.spec, err, tc.want)
		}
	}
	// Two shares at one guest path, however it is written.
	_, err = sharedDirMounts([]string{work + ":/mnt", filepath.Join(home, "proj") + ":/mnt/:ro"})
	if err == nil || !strings.Contains(err.Error(), "guest path /mnt is shared twice") {
		t.Errorf("two shares at /mnt: error = %v, want it refused", err)
	}
}

func TestCheckNetworkShares(t *testing.T) {
	for _, dest := range []string{"/etc/resolv.conf", "/etc//resolv.conf/"} {
		err := checkNetworkShares([]specs.Mount{{Destination: "/mnt"}, {Destination: dest}})
		if err == nil || !strings.Contains(err.Error(), "cannot share at /etc/resolv.conf with --net") {
			t.Errorf("a share at %s: error = %v, want it refused", dest, err)
		}
	}
	err := checkNetworkShares([]specs.Mount{{Destination: "/etc"}, {Destination: "/etc/resolv.conf.d"}})
	if err != nil {
		t.Errorf("shares beside /etc/resolv.conf: %v, want them taken", err)
	}
}
