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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// inspectProcess reads what processIsAVMM needs to know about pid from /proc.
//
// procps' ps takes the same "-p PID -o lstart=,command=" the macOS build runs,
// but its lstart has one-second resolution and is printed in local time to be
// parsed back, and its command column has already joined argv with spaces.
// /proc has the argv exactly and the start time in clock ticks, so it is read
// directly. argv0 comes from cmdline, not from comm: comm is cut to fifteen
// characters and is whatever the process last set with prctl, while argv[0] is
// what hull itself put there when it spawned the VMM.
func inspectProcess(pid int) (procInfo, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	argv, ok := readProcArgv(dir)
	if !ok {
		return procInfo{}, false
	}
	info := procInfo{argv0: argv[0], command: strings.Join(argv, " ")}
	started, err := processStartTime(dir)
	if err != nil {
		log.Debugf("could not read the start time of pid %d: %v", pid, err)
		return info, true
	}
	info.started, info.startKnown = started, true
	return info, true
}

// readProcArgv reads a process's argv, waiting briefly when it reads empty.
//
// Empty is what a kernel thread and a zombie read as, and neither is a VMM
// hull can stop. But it is also what a process reads as for a moment in the
// middle of execve: the kernel releases a vfork parent -- Go's os/exec spawns
// with CLONE_VFORK, so that is every child hull starts -- as soon as the new
// address space is installed, and lays out the new argv in it only after. A
// check that runs straight after spawning a VMM, as reapOrphanVMM's re-check
// can, would otherwise see "not a VMM" and walk past one. 50ms is far longer
// than that window and is paid only on an empty read.
func readProcArgv(dir string) ([]string, bool) {
	for attempt := 0; ; attempt++ {
		raw, err := os.ReadFile(dir + "/cmdline")
		if err != nil {
			return nil, false
		}
		if argv := parseProcCmdline(raw); len(argv) > 0 {
			return argv, true
		}
		if attempt == 10 {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func processStartTime(dir string) (t time.Time, err error) {
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return t, err
	}
	ticks, err := parseProcStatStartTicks(stat)
	if err != nil {
		return t, err
	}
	sys, err := os.ReadFile("/proc/stat")
	if err != nil {
		return t, err
	}
	boot, err := parseProcBootTime(sys)
	if err != nil {
		return t, err
	}
	return procStartTime(boot, ticks), nil
}

// listProcesses returns every process's pid and space-joined argv, from the
// numeric directories under /proc.
func listProcesses() ([]procEntry, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []procEntry
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid <= 0 {
			continue
		}
		// A process can exit between the listing and the read; that is not
		// an error, it is one fewer process.
		raw, rerr := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if rerr != nil {
			continue
		}
		argv := parseProcCmdline(raw)
		if len(argv) == 0 {
			continue
		}
		procs = append(procs, procEntry{pid: pid, command: strings.Join(argv, " ")})
	}
	return procs, nil
}

// processArgv returns the argv of pid as the kernel holds it.
func processArgv(pid int) ([]string, error) {
	argv, ok := readProcArgv("/proc/" + strconv.Itoa(pid))
	if !ok {
		return nil, fmt.Errorf("pid %d has no argv", pid)
	}
	return argv, nil
}
