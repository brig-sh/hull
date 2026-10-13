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
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The parsers for Linux's /proc files that process identity rests on. They
// take bytes rather than a pid so they can be tested against fixtures on any
// host: proc_linux.go reads the files and hands them over.

// procClockTicks is USER_HZ, the unit of /proc/PID/stat's starttime. The kernel
// reports times there in USER_HZ whatever its internal HZ is, and USER_HZ is
// 100 on every architecture Linux ships for, which is what sysconf(_SC_CLK_TCK)
// returns; reading it through cgo would buy nothing but a cgo dependency.
const procClockTicks = 100

// parseProcCmdline splits /proc/PID/cmdline into argv.
//
// The file is the argv strings each followed by a NUL. Splitting on NUL keeps
// an argument that contains spaces whole, which a ps listing cannot: there the
// boundaries are already gone.
func parseProcCmdline(data []byte) []string {
	data = bytes.TrimSuffix(data, []byte{0})
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, len(parts))
	for i, p := range parts {
		argv[i] = string(p)
	}
	return argv
}

// parseProcStatStartTicks returns field 22 of /proc/PID/stat, the process's
// start time in clock ticks after boot.
//
// Fields are counted from after the LAST closing parenthesis. Field 2 is the
// command name in parentheses, and that name is whatever the process chose:
// it can contain spaces and parentheses of its own, so splitting the whole
// line on spaces lets a process called "a) 1 2 3" shift every field after it
// and pass off any number it likes as its start time.
func parseProcStatStartTicks(data []byte) (uint64, error) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, errors.New("stat: no command name")
	}
	// After ") " the next field is 3 (state); starttime is field 22, so it is
	// the twentieth field from here.
	fields := strings.Fields(string(data[i+1:]))
	const startField = 22 - 3
	if len(fields) <= startField {
		return 0, fmt.Errorf("stat: %d fields after the command name, want more than %d", len(fields), startField)
	}
	return strconv.ParseUint(fields[startField], 10, 64)
}

// parseProcBootTime returns btime from /proc/stat: the boot time in seconds
// since the epoch, which is what starttime counts from.
func parseProcBootTime(data []byte) (int64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("/proc/stat has no btime line")
}

// procStartTime converts a starttime in clock ticks after boot to wall time.
func procStartTime(bootTime int64, ticks uint64) time.Time {
	sec := int64(ticks / procClockTicks)
	nsec := int64(ticks%procClockTicks) * int64(time.Second/procClockTicks)
	return time.Unix(bootTime+sec, nsec)
}
