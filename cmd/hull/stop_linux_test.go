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
	"context"
	"testing"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
)

// brig stops a sandbox and then reads `hull ps` to see whether it is still
// running, and removes it with `hull rm`.
func TestStopCommandAgainstDaemon(t *testing.T) {
	f := newInstanceFixture(t)
	ctx := context.Background()
	containerdtest.StartTask(ctx, t, f.c, "vm", "sleep", "300")

	_, err := f.run(f.ns, "stop", "missing")
	if err == nil || err.Error() != "instance not found: missing" {
		t.Errorf("stop missing: error = %v, want instance not found: missing", err)
	}

	// sleep ignores SIGTERM as the container's first process, so this one
	// ends with the SIGKILL that follows a timeout of 0.
	out, err := f.run(f.ns, "stop", "-t", "0", "vm")
	if err != nil || out != "Instance vm stopped\n" {
		t.Errorf("stop vm = %q, %v", out, err)
	}
	table, err := f.run(f.ns, "ps")
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	row := psRow(table, "vm")
	if len(row) < 2 || row[1] != "stopped" {
		t.Errorf("ps row for vm after the stop = %q, want status stopped", row)
	}

	out, err = f.run(f.ns, "stop", "vm")
	if err != nil || out != "Instance vm is already stopped\n" {
		t.Errorf("stop vm again = %q, %v", out, err)
	}
	out, err = f.run(f.ns, "rm", "vm")
	if err != nil || out != "Instance vm removed\n" {
		t.Errorf("rm vm after the stop = %q, %v", out, err)
	}
}
