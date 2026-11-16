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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"

	"github.com/brig-sh/hull/internal/containerd"
)

// fakeTask is a started task whose status reads as statuses, one per ask,
// the last one repeated.
type fakeTask struct {
	statuses []client.ProcessStatus
	err      error
	asked    int
}

func (f *fakeTask) Pid() uint32 { return 4242 }

func (f *fakeTask) Status(context.Context) (client.Status, error) {
	if f.err != nil {
		return client.Status{}, f.err
	}
	i := min(f.asked, len(f.statuses)-1)
	f.asked++
	return client.Status{Status: f.statuses[i]}, nil
}

// A run passes once urunc has exec'd the run's monitor, and fails at once
// when the task is any other program, which a shim that runs runc leaves
// there in place of a VM.
func TestCheckBooted(t *testing.T) {
	old, oldTimeout, oldGrace := procCmdline, bootTimeout, hiddenGrace
	t.Cleanup(func() { procCmdline, bootTimeout, hiddenGrace = old, oldTimeout, oldGrace })
	urunc := []string{"urunc", "--root", "/run/containerd/runc/hull", "create", "--bundle", "/b", "--reexec", "id"}
	libcontainer := []string{"/proc/self/exe", "monitor", "id"}
	hvi := []string{"/opt/brig/bin/hvi", "boot", "--net-tap", "tap0_urunc"}
	joined := []string{"/opt/brig/bin/hvi", "boot", "--net-gateway", "/urunc-net/gateway.sock", "--net-mac", "16:1f:43:50:64:d3"}
	qemu := []string{"/usr/bin/qemu-system-x86_64", "-m", "256M"}
	image := []string{"sleep", "infinity"}
	runcInit := []string{"runc", "init"}
	running := []client.ProcessStatus{client.Running}
	for _, tc := range []struct {
		name     string
		monitor  string
		gateway  bool
		seen     [][]string
		err      error
		statuses []client.ProcessStatus
		statErr  error
		// short is set by a case that needs bootTimeout or hiddenGrace to
		// run out, and only then are they short. Every other case gets a
		// minute, so its reads never race either bound, however slowly a
		// loaded machine under -race polls.
		short bool
		want  string
		// booted is the monitor's arguments checkBooted returns.
		booted []string
	}{
		{name: "hvi started", seen: [][]string{urunc, urunc, hvi}, booted: hvi},
		{name: "through libcontainer", seen: [][]string{libcontainer, hvi}, booted: hvi},
		// The command line reads empty while the process is mid-exec.
		{name: "caught mid-exec", seen: [][]string{{""}, hvi}, booted: hvi},
		{name: "qemu of the host's architecture", monitor: "qemu", seen: [][]string{urunc, qemu}, booted: qemu},
		{name: "the image under runc", seen: [][]string{image}, want: "the task runs sleep, where urunc runs hvi"},
		{name: "runc's init", seen: [][]string{runcInit}, want: "the task runs runc, where urunc runs hvi"},
		{name: "urunc handed it to runc", seen: [][]string{urunc, runcInit}, want: "the task runs runc"},
		{name: "another monitor", seen: [][]string{urunc, qemu}, want: "the task runs /usr/bin/qemu-system-x86_64, where urunc runs hvi"},
		{name: "a bare qemu prefix", monitor: "qemu", seen: [][]string{{"qemu-system-"}}, want: "where urunc runs qemu-system-"},
		{name: "no monitor in time", seen: [][]string{urunc}, short: true, want: "urunc did not start hvi within"},
		{name: "hvi joined the gateway", gateway: true, seen: [][]string{urunc, joined}, booted: joined},
		{name: "hvi on a tap of a gateway run", gateway: true, seen: [][]string{urunc, hvi}, want: "urunc started hvi without --net-gateway"},
		// A zombie's command line is empty, and a reaped process is gone.
		{name: "the task ended", seen: [][]string{urunc, {""}}, err: fs.ErrNotExist},
		{name: "the task ended before the first read", err: fs.ErrNotExist, statuses: []client.ProcessStatus{client.Stopped}},
		{name: "the task reaped mid-read", seen: [][]string{urunc}, err: &fs.PathError{Op: "read", Path: "/proc/4242/cmdline", Err: syscall.ESRCH}},
		{name: "containerd records the exit late", err: fs.ErrNotExist,
			statuses: []client.ProcessStatus{client.Running, client.Running, client.Stopped}},
		// hidepid=invisible, or a containerd in another PID namespace.
		{name: "the task hidden", err: fs.ErrNotExist, statuses: running, short: true, want: "hull cannot see the task's process 4242, which containerd reports running"},
		{name: "containerd does not answer", err: fs.ErrNotExist, statErr: errors.New("ttrpc: closed"), want: "ttrpc: closed"},
		{name: "no /proc access", seen: [][]string{urunc}, err: fs.ErrPermission, want: "read the command line of the task's process 4242"},
		{name: "an unknown monitor", monitor: "vz", seen: [][]string{urunc}, want: `the monitor "vz"`},
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
			monitor := tc.monitor
			if monitor == "" {
				monitor = "hvi"
			}
			bootTimeout, hiddenGrace = time.Minute, time.Minute
			if tc.short {
				bootTimeout, hiddenGrace = 200*time.Millisecond, 100*time.Millisecond
			}
			task := &fakeTask{statuses: tc.statuses, err: tc.statErr}
			if task.statuses == nil {
				task.statuses = running
			}
			booted, err := checkBooted(context.Background(), task, monitor, tc.gateway)
			if !slices.Equal(booted, tc.booted) {
				t.Errorf("monitor arguments = %q, want %q", booted, tc.booted)
			}
			if tc.want == "" && err != nil {
				t.Errorf("error = %v, want none", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Every monitor hull lets a run name has a program checkBooted waits for,
// or every run on it would fail.
func TestMonitorBinariesCoverTheMonitors(t *testing.T) {
	for _, m := range containerd.Monitors {
		if monitorBinaries[m] == "" {
			t.Errorf("monitor %s has no program in monitorBinaries", m)
		}
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
	// reads empty until then. checkBooted polls past it the same way.
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

// The record gives back the arguments as the monitor got them, whatever
// they hold, and nothing when there is none.
func TestRecordCmdLineRoundTrips(t *testing.T) {
	path, tmp := cmdLineFilesOf(filepath.Join(t.TempDir(), "vm.log"))
	got, err := readCmdLine(path)
	if err != nil || got != nil {
		t.Fatalf("no record: %q, %v; want nil, nil", got, err)
	}
	// What a run killed before its rename left is written over.
	err = os.WriteFile(tmp, []byte(`["/opt/brig/bin/hvi", "boot", "--mem-mib", "4096", "--cpus", "8", "--cmdline"]`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"/opt/brig/bin/hvi", "boot", "--mem-mib", "768", "--cpus", "2",
		"--cmdline", "panic=-1 console=ttyS0 root=fs0", "a\nnewline", "ünï"}
	err = recordCmdLine(path, tmp, args)
	if err != nil {
		t.Fatal(err)
	}
	got, err = readCmdLine(path)
	if err != nil || !slices.Equal(got, args) {
		t.Errorf("read back %q, %v; want %q", got, err, args)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, %v; want 0600, as a fresh file", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v (%v), want the record alone", entries, err)
	}
	err = os.WriteFile(path, []byte("hvi boot"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readCmdLine(path)
	if err == nil {
		t.Error("a record that is not JSON read without an error")
	}
}

// flagValue finds a flag's value in either spelling, and nothing for a flag
// that is missing or has no value after it.
func TestFlagValue(t *testing.T) {
	args := []string{"/opt/brig/bin/hvi", "boot", "--net-gateway", "/urunc-net/gateway.sock", "--net-mac", "c6:2f:93:3b:97:cf"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args, "c6:2f:93:3b:97:cf"},
		{[]string{"hvi", "--net-mac=c6:2f:93:3b:97:cf"}, "c6:2f:93:3b:97:cf"},
		{[]string{"hvi", "--net-tap", "tap0_urunc"}, ""},
		{[]string{"hvi", "--net-mac"}, ""},
		{nil, ""},
	} {
		got := flagValue(tc.args, "--net-mac")
		if got != tc.want {
			t.Errorf("flagValue(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
