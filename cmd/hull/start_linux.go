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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gatewayJoinTimeout bounds how long checkJoinedGateway waits for urunc to
// start hvi.
var gatewayJoinTimeout = 10 * time.Second

// procCmdline returns the arguments of the process pid.
var procCmdline = func(pid uint32) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// checkJoinedGateway fails a gateway run whose hvi did not join the
// gateway. A urunc that does not read the gateway's annotations, such as
// the brig-standalone-linux bundle's 3c00b69, ignores them and gives hvi a
// tap in the container's network namespace instead, where no network is
// attached, so the guest boots with none and nothing reports it. hull
// cannot tell beforehand which urunc the shim runs, so it reads what urunc
// started: the task's process, pid, execs hvi once urunc has set the
// instance up, and hvi joins a gateway only with --net-gateway.
//
// The process starts as urunc, so the check waits for hvi, for
// gatewayJoinTimeout at most. A task seen to end before then is not the
// gateway's to report. When the check cannot see hvi at all, it warns on
// w, and leaves the instance running.
//
// A process missing at the first read is not taken as a task that ended.
// It may have, but /proc can also hide it from hull: mounted with
// hidepid=invisible, it hides another user's processes, and a containerd in
// another PID namespace, as rootlesskit's --pidns puts it, reports a pid
// that hull's /proc does not have. hull cannot tell an ended task from a
// hidden one, and taking a hidden one for ended would let a guest with no
// network pass unreported, so the check warns.
func checkJoinedGateway(ctx context.Context, w io.Writer, pid uint32) error {
	deadline := time.Now().Add(gatewayJoinTimeout)
	seen := false
	for {
		args, err := procCmdline(pid)
		if errors.Is(err, fs.ErrNotExist) {
			if !seen {
				_, _ = fmt.Fprintf(w, "warning: hull cannot see the task's process %d, so it cannot tell whether hvi "+
					"joined the network gateway: the task ended at once, or /proc hides the process from hull\n", pid)
			}
			return nil
		}
		if err != nil {
			_, _ = fmt.Fprintf(w, "warning: cannot tell whether hvi joined the network gateway: %v\n", err)
			return nil
		}
		seen = true
		if filepath.Base(args[0]) == gatewayMonitor {
			if slices.Contains(args[1:], "--net-gateway") {
				return nil
			}
			return errors.New("urunc started hvi without --net-gateway, so the guest has no network: " +
				"the shim's urunc does not read the gateway's annotations, which takes " +
				"urunc-dev/urunc@034929e. The instance is removed")
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(w, "warning: urunc did not start hvi within %s, so hull cannot tell whether "+
				"it joined the network gateway\n", gatewayJoinTimeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
