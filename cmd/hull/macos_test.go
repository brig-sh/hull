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

//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeMacOSBundle makes dir a guest directory whose config.json names guestOS.
func writeMacOSBundle(t *testing.T, dir, guestOS string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range macosBundleFiles {
		data := []byte(nil)
		if name == "config.json" {
			data = []byte(`{"os":"` + guestOS + `","macAddress":"9e:df:0d:1a:6f:d2"}`)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMacosRunnerArgs(t *testing.T) {
	cases := []struct {
		name string
		opts macosRunOptions
		want []string
	}{
		{
			name: "bundle defaults",
			want: []string{"--macos-bundle", "/vm"},
		},
		{
			name: "recorded instance",
			opts: macosRunOptions{AgentSock: "/i/agent.sock", StateDir: "/i/checkpoint", StopGrace: 10},
			want: []string{"--macos-bundle", "/vm", "--agent-sock", "/i/agent.sock",
				"--state-dir", "/i/checkpoint", "--stop-grace", "10"},
		},
		{
			name: "sizing overrides",
			opts: macosRunOptions{CPUs: 6, MemoryMB: 8192},
			want: []string{"--macos-bundle", "/vm", "--cpus", "6", "--mem", "8192"},
		},
		{
			name: "window and no network",
			opts: macosRunOptions{GUI: true, GUITitle: "mac", NoNet: true},
			want: []string{"--macos-bundle", "/vm", "--no-net", "--gui", "--gui-title", "mac"},
		},
		{
			name: "title without a window is dropped",
			opts: macosRunOptions{GUITitle: "mac"},
			want: []string{"--macos-bundle", "/vm"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := macosRunnerArgs("/vm", tc.opts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckMacOSBundle(t *testing.T) {
	dir := t.TempDir()
	for _, name := range macosBundleFiles[:2] {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := checkMacOSBundle(dir)
	if err == nil || !strings.Contains(err.Error(), "nvram.bin") {
		t.Fatalf("a directory without nvram.bin must be refused by name, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nvram.bin"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkMacOSBundle(dir); err != nil {
		t.Fatalf("a complete directory was refused: %v", err)
	}
}

func TestResolveMacOSGuest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TART_HOME", home)
	mac := filepath.Join(home, "vms", "mac")
	writeMacOSBundle(t, mac, "darwin")
	writeMacOSBundle(t, filepath.Join(home, "vms", "penguin"), "linux")
	// An OCI bundle is also a directory with a config.json.
	oci := filepath.Join(home, "oci")
	if err := os.MkdirAll(oci, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oci, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ref  string
		want string
	}{
		{ref: "mac", want: mac},
		{ref: mac, want: mac},
		{ref: "penguin"},
		{ref: "missing"},
		{ref: "mac:latest"},
		{ref: "library/mac"},
		{ref: oci},
	}
	for _, tc := range cases {
		got, ok := resolveMacOSGuest(tc.ref)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("resolveMacOSGuest(%q) = %q, %v; want %q", tc.ref, got, ok, tc.want)
		}
	}
}

func TestBundleOfCmdLine(t *testing.T) {
	if got := bundleOfCmdLine([]string{"/x/vz-runner", "--macos-bundle", "/vm", "--gui"}); got != "/vm" {
		t.Fatalf("got %q", got)
	}
	if got := bundleOfCmdLine([]string{"/x/vz-runner", "--kernel", "/k"}); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := bundleOfCmdLine([]string{"/x/vz-runner", "--macos-bundle"}); got != "" {
		t.Fatalf("a flag with no value must not match, got %q", got)
	}
}

func TestWithGUIRecord(t *testing.T) {
	base := []string{"/x/vz-runner", "--macos-bundle", "/vm", "--gui", "--gui-record", "/old.mov", "--restore"}
	got, err := withGUIRecord(base, "/new.mov")
	want := []string{"/x/vz-runner", "--macos-bundle", "/vm", "--gui", "--restore", "--gui-record", "/new.mov"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	got, err = withGUIRecord(base, "")
	want = []string{"/x/vz-runner", "--macos-bundle", "/vm", "--gui", "--restore"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("an old recording must be dropped: got %v, %v", got, err)
	}
	if _, err := withGUIRecord([]string{"/x/vz-runner", "--macos-bundle", "/vm"}, "/new.mov"); err == nil {
		t.Fatal("a recording without a window must be refused")
	}
}
