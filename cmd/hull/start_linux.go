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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// bootTimeout bounds how long checkBooted waits for urunc to exec the
// monitor. Between the start and that exec urunc sets the instance up, and
// for a monitor that shares no directory with the guest that includes
// copying the image's root filesystem into an initrd or a disk, which
// grows with the image. A run that times out is removed, so the bound errs
// long.
var bootTimeout = time.Minute

// hiddenGrace is how long checkBooted waits for containerd to report a
// task stopped once its process is gone from /proc. The shim reaps the
// process before it records the exit, so for a moment the task can still
// read as running.
var hiddenGrace = time.Second

// monitorBinaries are the programs urunc execs as each monitor, by the
// names it looks them up under when its configuration gives a monitor no
// path (vmmFactories in pkg/unikontainers/hypervisors at
// urunc-dev/urunc@3c00b69). qemu's name ends in the host's architecture,
// so it is a prefix.
var monitorBinaries = map[string]string{
	"cloud-hypervisor":    "cloud-hypervisor",
	"firecracker":         "firecracker",
	"hvi":                 "hvi",
	"hvt":                 "solo5-hvt",
	"hyperlight-unikraft": "hluk",
	"qemu":                "qemu-system-",
	"spt":                 "solo5-spt",
}

// procCmdline returns the arguments of the process pid.
var procCmdline = func(pid uint32) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// startedTask is what checkBooted asks of a started task.
type startedTask interface {
	Pid() uint32
	Status(ctx context.Context) (client.Status, error)
}

// checkBooted fails a run whose task is not the VM it asked for. The shim
// of urunc's runtime runs whatever OCI runtime it was built to run, and a
// shim that runs runc runs the image as an ordinary container on the
// host's kernel: containerd reports the task running, and nothing else
// tells it from a VM. So hull reads what was started, the command line of
// the task's process. urunc starts as that process and, once it has set
// the instance up, execs the monitor in its place, with the monitor's path
// as the first argument. On a gateway the monitor is hvi, which has to
// have --net-gateway too: a urunc that does not read the gateway's
// annotations, such as the brig-standalone-linux bundle's 3c00b69, gives
// hvi a tap in the container's network namespace instead, where no network
// is attached, so the guest boots with none.
//
// The first argument tells the cases apart. urunc's is "urunc", the name
// the shim runs it by, or "/proc/self/exe" when its configuration has it
// start the monitor through libcontainer (runtime.libcontainer), and the
// check waits through those for bootTimeout at most. An empty one is a
// process caught mid-exec. Any other program is not urunc and not the
// monitor, and fails the run at once: runc names itself "runc", and the
// image's command is the image's. A monitor configured at a path of
// another name fails too, since hull cannot read urunc's configuration.
//
// A process gone after it was seen is a task that ended, which hull ps
// reports as the instance's exit. One gone at the first read may have
// ended too, but /proc also hides a live one: mounted with
// hidepid=invisible it hides another user's processes, and a containerd in
// another PID namespace, as rootlesskit's --pidns puts it, reports a pid
// hull's /proc does not have. containerd tells the two apart, and a task it
// still reports running fails the run, since hull cannot tell what runs.
// A task that ended passes whatever it ran, as nothing is left to read, so
// under a shim that runs runc an image whose command exits at once passes
// too. In another PID namespace the pid can also name an unrelated process
// of hull's, which fails the run as a program other than the monitor.
func checkBooted(ctx context.Context, task startedTask, monitor string, gateway bool) error {
	binary, ok := monitorBinaries[monitor]
	if !ok {
		return fmt.Errorf("hull does not know the program urunc runs as the monitor %q, so it cannot tell "+
			"whether a VM booted", monitor)
	}
	pid := task.Pid()
	deadline := time.Now().Add(bootTimeout)
	seen := false
	var hiddenSince time.Time
	for {
		args, err := procCmdline(pid)
		switch {
		// A process reaped between the open and the read of its cmdline
		// fails the read with ESRCH rather than ENOENT, and is gone too.
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ESRCH):
			if seen {
				return nil
			}
			if hiddenSince.IsZero() {
				hiddenSince = time.Now()
			}
			status, err := task.Status(ctx)
			if err != nil {
				return fmt.Errorf("ask containerd about the task's process %d, which /proc does not show: %w", pid, err)
			}
			if status.Status == client.Stopped {
				return nil
			}
			if time.Since(hiddenSince) > hiddenGrace {
				return fmt.Errorf("hull cannot see the task's process %d, which containerd reports %s, so it "+
					"cannot tell whether urunc started a VM: /proc hides the process from hull when it is "+
					"mounted with hidepid and the task runs as another user, or when containerd runs in "+
					"another PID namespace, such as rootlesskit's --pidns. The instance is removed",
					pid, status.Status)
			}
		case err != nil:
			return fmt.Errorf("read the command line of the task's process %d, to tell whether urunc "+
				"started a VM: %w. The instance is removed", pid, err)
		default:
			seen = true
			name := filepath.Base(args[0])
			switch {
			case isMonitor(name, binary):
				if gateway && !slices.Contains(args[1:], "--net-gateway") {
					return errors.New("urunc started hvi without --net-gateway, so the guest has no network: " +
						"the shim's urunc does not read the gateway's annotations, which takes " +
						"urunc-dev/urunc@034929e. The instance is removed")
				}
				return nil
			case args[0] == "", name == "urunc", args[0] == "/proc/self/exe":
			default:
				return fmt.Errorf("the task runs %s, where urunc runs %s, so no VM booted: the "+
					"containerd shim of urunc's runtime ran another OCI runtime, such as runc, which runs "+
					"the image as an ordinary container on the host's kernel. The instance is removed",
					args[0], binary)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("urunc did not start %s within %s, so hull cannot tell whether a VM booted. "+
				"The instance is removed", binary, bootTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// isMonitor reports whether name, the base name of a program, is binary,
// as monitorBinaries names it.
func isMonitor(name, binary string) bool {
	if strings.HasSuffix(binary, "-") {
		return strings.HasPrefix(name, binary) && len(name) > len(binary)
	}
	return name == binary
}
