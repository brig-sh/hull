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
