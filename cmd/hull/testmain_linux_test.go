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
	"testing"
)

// TestMain on Linux. The macOS one is in compose_down_test.go, with the
// compose tests that only build there; both let the move tests run this
// binary as ssh and as hull.
func TestMain(m *testing.M) {
	// The unit tests boot nothing, and the CI runners have no /dev/kvm:
	// the destination answers the probe as a host that can, whether it
	// runs inside the test binary or in the hull the fake ssh runs.
	hypervisorAvailable = func() (bool, string) { return true, "test stand-in" }
	testBinaryAsHull()
	code := m.Run()
	removeFakeHvi()
	os.Exit(code)
}
