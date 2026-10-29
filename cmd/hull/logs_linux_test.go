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
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"
	"golang.org/x/sys/unix"

	"github.com/brig-sh/hull/internal/containerd"
)

// newLoggedInstance creates the container id in the fixture's namespace and
// writes log to the path hull run gives its log. Its log-file label names a
// decoy, which logs must not print.
func newLoggedInstance(t *testing.T, f *instanceFixture, id, log string) string {
	t.Helper()
	ctx := context.Background()
	decoy := t.TempDir() + "/decoy"
	err := os.WriteFile(decoy, []byte("decoy\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := f.c.NewContainer(ctx, id, client.WithSpec(&oci.Spec{}),
		client.WithContainerLabels(map[string]string{containerd.LabelLogFile: decoy}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })
	logFile, err := instanceLogFile(f.ns, id)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(logFile, []byte(log), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return logFile
}

func TestLogsCommandAgainstDaemon(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	newLoggedInstance(t, f, "vm", "one\ntwo\nthree\n")
	ctr, err := f.c.NewContainer(context.Background(), "nolog", client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(context.Background()) })

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "vm"}, "one\ntwo\nthree\n"},
		{[]string{"logs", "--tail", "2", "vm"}, "two\nthree\n"},
		{[]string{"logs", "-n", "0", "vm"}, ""},
	} {
		out, err := f.run(f.ns, tc.args...)
		if err != nil || out != tc.want {
			t.Errorf("%s = %q, %v; want %q", strings.Join(tc.args, " "), out, err, tc.want)
		}
	}

	_, err = f.run(f.ns, "logs", "missing")
	if err == nil || err.Error() != "instance not found: missing" {
		t.Errorf("logs missing: error = %v, want instance not found: missing", err)
	}
	// A container hull run did not create has no log.
	_, err = f.run(f.ns, "logs", "nolog")
	if err == nil || !strings.HasPrefix(err.Error(), "log file not found: ") {
		t.Errorf("logs nolog: error = %v, want log file not found", err)
	}
}

// newPTY opens a pseudo-terminal and returns its two ends.
func newPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// A log replayed onto a terminal goes through the guest output filter, so a
// clipboard query the guest printed does not reach the terminal.
func TestLogsFilterWhatReachesATerminal(t *testing.T) {
	// A HULL_TERMINAL_FILTER=off in the shell running the tests would turn
	// the filter off and fail the test for no fault of the code.
	t.Setenv(TerminalFilterEnv, "")
	privateLogDirs(t)
	f := newInstanceFixture(t)
	newLoggedInstance(t, f, "vm", "boot ok\x1b]52;c;?\x07\n")
	master, slave := newPTY(t)

	app := newApp()
	app.Writer = slave
	err := app.Run(context.Background(), []string{"hull", "--containerd-address", f.addr,
		"--namespace", f.ns, "logs", "vm"})
	if err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	got, _ := io.ReadAll(master)
	if !strings.Contains(string(got), "boot ok") || strings.Contains(string(got), "\x1b]52") {
		t.Errorf("the terminal got %q, want the text without the clipboard query", got)
	}
}

// syncBuffer is a buffer a test can read while a command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput waits for out to read want, and fails the test if it does
// not within a few seconds.
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// brig follows a sandbox's log. The follow prints what the log gains and
// ends when hull rm deletes the log.
func TestLogsFollowUntilTheLogIsRemoved(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	logFile := newLoggedInstance(t, f, "vm", "booted\n")

	var out syncBuffer
	app := newApp()
	app.Writer = &out
	done := make(chan error, 1)
	go func() {
		done <- app.Run(context.Background(), []string{"hull", "--containerd-address", f.addr,
			"--namespace", f.ns, "logs", "--follow", "vm"})
	}()
	waitForOutput(t, &out, "booted\n")

	log, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = log.WriteString("later\n")
	_ = log.Close()
	if err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, &out, "booted\nlater\n")

	err = os.Remove(logFile)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Errorf("logs --follow: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logs --follow went on after the log was removed")
	}
}
