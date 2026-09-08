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

// The kernel silently caps unix socket paths at sun_path; hull must say so
// before staging an instance, naming the path and the fix, and must accept a
// path of exactly the maximum length.
func TestCheckUnixSocketPath(t *testing.T) {
	longest := "/" + strings.Repeat("a", unixSocketPathMax-1)
	if err := checkUnixSocketPath("agent", longest); err != nil {
		t.Fatalf("path of %d bytes must be accepted: %v", len(longest), err)
	}

	tooLong := longest + "b"
	err := checkUnixSocketPath("agent", tooLong)
	if err == nil {
		t.Fatalf("path of %d bytes must be rejected", len(tooLong))
	}
	for _, want := range []string{"agent socket path", "--store-dir", tooLong} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
