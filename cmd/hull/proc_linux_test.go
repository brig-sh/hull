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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The Linux identity check reads /proc, and has to agree with the recorded
// argv exactly -- spaces inside an argument included -- and report a start
// time that the pid-reuse check can trust.
func TestInspectProcessReadsArgvAndStartTimeFromProc(t *testing.T) {
	before := time.Now()
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done # an argument with spaces")
	cmd.Args[0] = "hvi"
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	info, ok := inspectProcess(cmd.Process.Pid)
	if !ok {
		t.Fatal("a live child was not found in /proc")
	}
	if want := strings.Join(cmd.Args, " "); info.command != want {
		t.Errorf("command = %q, want %q", info.command, want)
	}
	if info.argv0 != "hvi" {
		t.Errorf("argv0 = %q, want hvi", info.argv0)
	}
	if !info.startKnown {
		t.Fatal("no start time")
	}
	// Clock ticks are 10ms and btime is whole seconds, so allow a second of
	// slack either side.
	if info.started.Before(before.Add(-2*time.Second)) || info.started.After(time.Now().Add(2*time.Second)) {
		t.Errorf("start time %v is not around %v", info.started, before)
	}

	procs, err := listProcesses()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range procs {
		if p.pid == cmd.Process.Pid && p.command == info.command {
			found = true
		}
	}
	if !found {
		t.Error("listProcesses did not list the child with its argv")
	}
}

// e2fsprogs lives in /sbin on Debian and Raspberry Pi OS, which an ordinary
// login's PATH does not include. The lookup must still find it there.
//
// findE2fsTool falls back to /usr/sbin/<name> when it finds nothing, so a
// prefix check alone would pass on a host with no e2fsprogs at all. The
// result has to exist.
func TestE2fsToolsAreFoundOutsidePath(t *testing.T) {
	installed := false
	for _, p := range []string{"/usr/sbin/mke2fs", "/sbin/mke2fs"} {
		if _, err := os.Stat(p); err == nil {
			installed = true
		}
	}
	if !installed {
		t.Skip("e2fsprogs is not installed in /usr/sbin or /sbin")
	}
	t.Setenv("PATH", "/nonexistent")
	got := findE2fsTool("mke2fs")
	if !strings.HasPrefix(got, "/usr/sbin/") && !strings.HasPrefix(got, "/sbin/") {
		t.Fatalf("findE2fsTool(mke2fs) = %q, want a path under /usr/sbin or /sbin", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("findE2fsTool(mke2fs) = %q, which does not exist: %v", got, err)
	}
}
