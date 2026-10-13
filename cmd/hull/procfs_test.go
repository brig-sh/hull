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
	"reflect"
	"testing"
	"time"
)

func TestParseProcCmdlineKeepsArgumentsWhole(t *testing.T) {
	raw := []byte("/home/u/hull-bin/hvi\x00boot\x00--cmdline\x00rdinit=/init console=ttyAMA0\x00")
	want := []string{"/home/u/hull-bin/hvi", "boot", "--cmdline", "rdinit=/init console=ttyAMA0"}
	if got := parseProcCmdline(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcCmdline = %q, want %q", got, want)
	}
	// A kernel thread or a zombie has an empty cmdline, and is no process
	// hull could have spawned.
	if got := parseProcCmdline(nil); got != nil {
		t.Fatalf("empty cmdline parsed as %q", got)
	}
	// An empty argument in the middle is still an argument.
	if got := parseProcCmdline([]byte("a\x00\x00b\x00")); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Fatalf("empty middle argument lost: %q", got)
	}
}

// The command name in /proc/PID/stat is the process's own choice and may hold
// spaces and parentheses. Counting fields from the first ")" would let a
// process named to imitate the rest of the line pick its own start time, and
// with it get past the pid-reuse check in processIsAVMM.
func TestParseProcStatStartTicksSkipsAHostileCommandName(t *testing.T) {
	// Fields 3..21 are filler; field 22 (starttime) is 4242.
	tail := "S 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 4242 1000 10"
	for _, comm := range []string{"hvi", "a b", "x) 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 99 (y", "))"} {
		line := []byte("1234 (" + comm + ") " + tail + "\n")
		got, err := parseProcStatStartTicks(line)
		if err != nil {
			t.Fatalf("comm %q: %v", comm, err)
		}
		if got != 4242 {
			t.Errorf("comm %q: starttime = %d, want 4242", comm, got)
		}
	}
	if _, err := parseProcStatStartTicks([]byte("1234 (hvi) S 1 2")); err == nil {
		t.Error("a truncated stat line was accepted")
	}
	if _, err := parseProcStatStartTicks([]byte("garbage")); err == nil {
		t.Error("a stat line with no command name was accepted")
	}
}

func TestParseProcBootTime(t *testing.T) {
	raw := []byte("cpu  1 2 3 4\nintr 5\nctxt 6\nbtime 1757000000\nprocesses 7\n")
	got, err := parseProcBootTime(raw)
	if err != nil || got != 1757000000 {
		t.Fatalf("parseProcBootTime = %d, %v", got, err)
	}
	if _, err := parseProcBootTime([]byte("cpu 1\n")); err == nil {
		t.Error("a /proc/stat with no btime was accepted")
	}
}

func TestProcStartTimeCountsClockTicksFromBoot(t *testing.T) {
	got := procStartTime(1000, 12345) // 123.45 s after boot
	want := time.Unix(1123, 450_000_000)
	if !got.Equal(want) {
		t.Fatalf("procStartTime = %v, want %v", got, want)
	}
}
