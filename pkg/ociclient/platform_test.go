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

package ociclient

import "testing"

func TestParsePlatform(t *testing.T) {
	cases := []struct {
		in       string
		os, arch string
		variant  string
		wantErr  bool
	}{
		{in: "linux/amd64", os: "linux", arch: "amd64"},
		{in: "linux/arm64", os: "linux", arch: "arm64"},
		{in: "linux/arm64/v8", os: "linux", arch: "arm64", variant: "v8"},
		{in: "linux", wantErr: true},
		{in: "linux/arm64/v8/extra", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, err := parsePlatform(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePlatform(%q) = %+v, want error", tc.in, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlatform(%q): %v", tc.in, err)
			}
			if p.OS != tc.os || p.Architecture != tc.arch || p.Variant != tc.variant {
				t.Fatalf("parsePlatform(%q) = %s/%s/%s, want %s/%s/%s",
					tc.in, p.OS, p.Architecture, p.Variant, tc.os, tc.arch, tc.variant)
			}
		})
	}
}
