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
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// A gateway run fails when urunc started hvi without --net-gateway, as a
// urunc that ignores the gateway's annotations does, and waits for urunc to
// exec hvi first.
func TestCheckJoinedGateway(t *testing.T) {
	old, oldTimeout := procCmdline, gatewayJoinTimeout
	t.Cleanup(func() { procCmdline, gatewayJoinTimeout = old, oldTimeout })
	gatewayJoinTimeout = 200 * time.Millisecond
	urunc := []string{"urunc", "--root", "/run/urunc", "reexec"}
	joined := []string{"/opt/brig/bin/hvi", "boot", "--net-gateway", "/urunc-net/gateway.sock", "--net-mac", "16:1f:43:50:64:d3"}
	tap := []string{"/opt/brig/bin/hvi", "boot", "--net-tap", "tap0_urunc"}
	for _, tc := range []struct {
		name    string
		seen    [][]string
		err     error
		want    string
		warning string
	}{
		{name: "hvi joined", seen: [][]string{urunc, urunc, joined}},
		{name: "hvi on a tap", seen: [][]string{urunc, tap}, want: "urunc started hvi without --net-gateway"},
		// A zombie's cmdline is empty.
		{name: "the task ended", seen: [][]string{urunc, {""}}, err: fs.ErrNotExist},
		// hidepid=invisible, or a containerd in another PID namespace.
		{name: "the task never seen", err: fs.ErrNotExist, warning: "hull cannot see the task's process 4242"},
		{name: "no hvi in time", seen: [][]string{urunc}, warning: "urunc did not start hvi within"},
		{name: "no /proc access", seen: [][]string{urunc}, err: fs.ErrPermission, warning: "cannot tell whether hvi joined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			procCmdline = func(pid uint32) ([]string, error) {
				if pid != 4242 {
					t.Errorf("read the arguments of %d, want the task's, 4242", pid)
				}
				calls++
				if calls > len(tc.seen) {
					if tc.err != nil {
						return nil, tc.err
					}
					return tc.seen[len(tc.seen)-1], nil
				}
				return tc.seen[calls-1], nil
			}
			var out bytes.Buffer
			err := checkJoinedGateway(context.Background(), &out, 4242)
			if tc.want == "" && err != nil {
				t.Errorf("error = %v, want none", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(out.String(), tc.warning) || (tc.warning == "" && out.Len() != 0) {
				t.Errorf("warned %q, want %q", out.String(), tc.warning)
			}
		})
	}
}

// procCmdline reads a process's arguments as it was given them, the first
// included, which names hvi for urunc's monitor.
func TestProcCmdline(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(sleep, "30")
	cmd.Args = []string{"/opt/brig/bin/hvi", "30"}
	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Start returns once the child has begun to exec, which can be before
	// the kernel has set up the new program's arguments: the command line
	// reads empty until then. checkJoinedGateway polls past it the same way.
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err = procCmdline(uint32(cmd.Process.Pid))
		if err == nil && slices.Equal(got, cmd.Args) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("procCmdline = %q, %v; want %q", got, err, cmd.Args)
		}
		time.Sleep(time.Millisecond)
	}
	_, err = procCmdline(1 << 30)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a process that does not exist: error = %v, want ErrNotExist", err)
	}
}
