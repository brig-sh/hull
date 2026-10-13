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
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// inspectProcess reads what processIsAVMM needs to know about pid from ps.
//
// ps prints lstart as a fixed 24-character date, then the command. The command
// is argv joined with spaces, which is also how the recorded argv is compared.
func inspectProcess(pid int) (procInfo, bool) {
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=,command=").Output()
	if err != nil {
		return procInfo{}, false
	}
	line := strings.TrimSpace(string(out))
	const lstartLen = 24
	if len(line) <= lstartLen {
		return procInfo{}, false
	}
	started, command := strings.TrimSpace(line[:lstartLen]), strings.TrimSpace(line[lstartLen:])
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return procInfo{}, false
	}
	info := procInfo{argv0: fields[0], command: command}
	// ParseInLocation, not Parse: `ps -o lstart=` prints LOCAL wall-clock time
	// with no zone, and parsing it as UTC makes every process look
	// (UTC-offset) hours out. On a UTC+3 machine that made every live VMM
	// appear to have started three hours in the future, so the reuse check
	// rejected it and stop refused to signal a VM it should have stopped.
	when, perr := time.ParseInLocation("Mon Jan  2 15:04:05 2006", started, time.Local)
	if perr != nil {
		// Unparseable: the caller falls back to the name match rather than
		// refusing to act at all; say so in the log rather than silently.
		log.Debugf("could not parse the start time %q for pid %d: %v", started, pid, perr)
		return info, true
	}
	info.started, info.startKnown = when, true
	return info, true
}

// listProcesses returns every process's pid and space-joined argv.
//
// -ww so long argv is not truncated to the terminal width; the paths that
// make a recorded argv unique are at the end of the line.
func listProcesses() ([]procEntry, error) {
	out, err := exec.Command("/bin/ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var procs []procEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidStr, cmd, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, perr := strconv.Atoi(pidStr)
		if perr != nil || pid <= 0 {
			continue
		}
		procs = append(procs, procEntry{pid: pid, command: strings.TrimSpace(cmd)})
	}
	return procs, nil
}

// processArgv returns the argv of pid as the kernel holds it, via the
// kern.procargs2 sysctl. The buffer is argc (a native int32), then the exec
// path, NUL padding, and argc NUL-terminated argv strings, then the
// environment, which is not read.
func processArgv(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(buf) < 4 {
		return nil, fmt.Errorf("procargs2 for pid %d: short buffer", pid)
	}
	argc := int(int32(binary.NativeEndian.Uint32(buf)))
	if argc < 0 {
		return nil, fmt.Errorf("procargs2 for pid %d: argc %d", pid, argc)
	}
	rest := buf[4:]
	// Skip the exec path, then the NUL padding after it.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, fmt.Errorf("procargs2 for pid %d: no exec path", pid)
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	argv := make([]string, 0, argc)
	for range argc {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil, fmt.Errorf("procargs2 for pid %d: truncated argv", pid)
		}
		argv = append(argv, string(rest[:j]))
		rest = rest[j+1:]
	}
	return argv, nil
}
