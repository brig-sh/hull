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

// Package containerdtest holds helpers for tests that run against
// containerd, for use from any package.
package containerdtest

import (
	"os"
	"testing"
)

// DaemonAddress returns the socket of the containerd daemon a test runs
// against, which HULL_TEST_CONTAINERD_ADDRESS names. Without it the test
// skips, unless HULL_TEST_REQUIRE_CONTAINERD is set: then it fails. CI sets
// that, so a daemon that went missing fails the job rather than passing it
// with the test skipped.
func DaemonAddress(t testing.TB) string {
	t.Helper()
	addr := os.Getenv("HULL_TEST_CONTAINERD_ADDRESS")
	if addr == "" {
		if os.Getenv("HULL_TEST_REQUIRE_CONTAINERD") != "" {
			t.Fatal("HULL_TEST_REQUIRE_CONTAINERD is set, but HULL_TEST_CONTAINERD_ADDRESS names no daemon")
		}
		t.Skip("HULL_TEST_CONTAINERD_ADDRESS not set")
	}
	return addr
}
