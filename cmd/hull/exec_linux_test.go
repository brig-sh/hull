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
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
)

// runExec runs `hull exec` with args against the fixture's daemon, with
// stdin as its input, and returns what it printed on each stream.
func (f *instanceFixture) runExec(stdin string, args ...string) (stdout, stderr string, err error) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	app := newApp()
	app.Reader = strings.NewReader(stdin)
	app.Writer = &out
	app.ErrWriter = &errOut
	argv := append([]string{"hull", "--containerd-address", f.addr, "--namespace", f.ns, "exec"}, args...)
	err = app.Run(context.Background(), argv)
	return out.String(), errOut.String(), err
}

// privateFIFODir gives exec's FIFOs a directory of the test's own when the
// test is not root.
func privateFIFODir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// brig runs everything in a sandbox through `hull exec`: its readiness
// probe, the commands whose output it reads, and the ones it feeds a
// file on stdin.
func TestExecCommandAgainstDaemon(t *testing.T) {
	privateFIFODir(t)
	f := newInstanceFixture(t)
	ctx := context.Background()
	containerdtest.StartTask(ctx, t, f.c, "vm", "sleep", "300")
	newLoggedInstance(t, f, "idle", "")

	out, errOut, err := f.runExec("in\n", "vm", "--", "sh", "-c", "echo out; echo err >&2; cat; exit 3")
	var code exitCode
	if out != "out\nin\n" || errOut != "err\n" || !errors.As(err, &code) || code != 3 {
		t.Errorf("exec = %q, %q, %v; want %q, %q and exit status 3", out, errOut, err, "out\nin\n", "err\n")
	}
	// Nothing exec starts outlives it, the goroutine that passes signals on
	// to the command among them.
	waitForGoroutinesOf(t, execInstance)

	// A bare --env takes its value from hull's environment, so a secret
	// stays out of hull's argv.
	t.Setenv("HULL_TEST_SECRET", "s3cret")
	out, _, err = f.runExec("", "--env", "FOO=bar", "--env", "HULL_TEST_SECRET", "--cwd", "/tmp",
		"-u", "1000:1001", "vm", "--", "sh", "-c", `echo "$FOO $HULL_TEST_SECRET"; pwd; id -u; id -g`)
	want := "bar s3cret\n/tmp\n1000\n1001\n"
	if err != nil || out != want {
		t.Errorf("exec with env, cwd and user = %q, %v; want %q", out, err, want)
	}

	out, _, err = f.runExec("", "-u", "root", "vm", "--", "id", "-u")
	if err != nil || out != "0\n" {
		t.Errorf("exec -u root = %q, %v; want %q", out, err, "0\n")
	}

	// With no terminal among the streams, the guest's terminal is 24x80.
	out, _, err = f.runExec("", "-t", "vm", "--", "sh", "-c", "test -t 0 && stty size")
	if err != nil || out != "24 80\r\n" {
		t.Errorf("exec -t = %q, %v; want %q", out, err, "24 80\r\n")
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"missing", "true"}, "instance not found: missing"},
		{[]string{"idle", "true"}, "instance idle is not running"},
		{[]string{"vm"}, "usage: exec [-t] <instance-id> <command> [args...]"},
		{[]string{"../x", "true"}, "invalid instance name"},
		{[]string{"-u", "nobody", "vm", "true"}, "takes only a numeric uid and gid, or root"},
		{[]string{"--env", "=x", "vm", "true"}, "invalid --env"},
	} {
		_, _, err = f.runExec("", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("exec %s: error = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// On a terminal, exec sizes the guest's terminal from it, filters what the
// guest prints to it, and leaves it as it found it.
func TestExecOnATerminal(t *testing.T) {
	// A HULL_TERMINAL_FILTER=off in the shell running the tests would turn
	// the filter off and fail the test for no fault of the code.
	t.Setenv(TerminalFilterEnv, "")
	privateFIFODir(t)
	f := newInstanceFixture(t)
	containerdtest.StartTask(context.Background(), t, f.c, "vm", "sleep", "300")
	master, slave := newPTY(t)
	err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120})
	if err != nil {
		t.Fatal(err)
	}
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	// The terminal is read while exec runs. exec's stdin copy is still
	// reading the slave when exec returns, and the slave stays open until
	// that read ends, so the master would not reach its end.
	var got syncBuffer
	go func() { _, _ = io.Copy(&got, master) }()

	app := newApp()
	app.Reader = slave
	app.Writer = slave
	app.ErrWriter = slave
	err = app.Run(context.Background(), []string{"hull", "--containerd-address", f.addr,
		"--namespace", f.ns, "exec", "-t", "vm", "--", "sh", "-c", `stty size; printf 'x\033]52;c;?\007y\n'`})
	if err != nil {
		t.Fatal(err)
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Errorf("the terminal's settings after exec = %+v, want them as before, %+v", *after, *before)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(got.String(), "xy") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(got.String(), "40 120") || strings.Contains(got.String(), "\x1b]52") ||
		!strings.Contains(got.String(), "xy") {
		t.Errorf("the terminal got %q, want the size 40 120 and xy without the clipboard query", got.String())
	}
}

// A resize of hull's terminal reaches the guest's. runc, unlike urunc, sets
// the guest's terminal to the size the resize gives.
func TestExecFollowsTheWindowSize(t *testing.T) {
	t.Setenv(TerminalFilterEnv, "")
	privateFIFODir(t)
	f := newInstanceFixture(t)
	containerdtest.StartTask(context.Background(), t, f.c, "vm", "sleep", "300")
	master, slave := newPTY(t)
	err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120})
	if err != nil {
		t.Fatal(err)
	}
	var got syncBuffer
	go func() { _, _ = io.Copy(&got, master) }()

	app := newApp()
	app.Reader, app.Writer, app.ErrWriter = slave, slave, slave
	// The command waits up to 10 seconds for the size to change.
	script := `echo started; i=0; while [ "$(stty size)" = "40 120" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done; stty size`
	done := make(chan error, 1)
	go func() {
		done <- app.Run(context.Background(), []string{"hull", "--containerd-address", f.addr,
			"--namespace", f.ns, "exec", "-t", "vm", "--", "sh", "-c", script})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(got.String(), "started") {
		if time.Now().After(deadline) {
			t.Fatalf("the command did not start; output %q", got.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// hull is not in the terminal's foreground process group, so the
	// kernel does not send it the SIGWINCH.
	err = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 100})
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Kill(syscall.Getpid(), syscall.SIGWINCH)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("exec went on; output %q", got.String())
	}
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "50 100") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || !strings.Contains(got.String(), "50 100") {
		t.Errorf("exec = %v, the terminal got %q; want the size 50 100", err, got.String())
	}
}

// sigIgnPattern finds the mask of ignored signals in a /proc/<pid>/status.
var sigIgnPattern = regexp.MustCompile(`SigIgn:\s*([0-9a-f]+)`)

// The signals docs/build.md lists end the command, and hull then deletes
// its exec process, which removes its FIFOs. A signal hull was started
// with ignored stays ignored, and so does SIGQUIT when SIGINT was ignored.
func TestExecPassesSignalsToTheCommand(t *testing.T) {
	f := newInstanceFixture(t)
	containerdtest.StartTask(context.Background(), t, f.c, "vm", "sleep", "300")
	// A shell cannot trap a signal it was started with ignored, so the
	// command first prints the signals it started with ignored. grep reads
	// them from its own status, which it inherited from the shell.
	script := `grep SigIgn /proc/self/status; ` +
		`trap "echo TERM; exit 42" TERM; trap "echo HUP; exit 43" HUP; trap "echo INT; exit 44" INT; ` +
		`echo started; while :; do sleep 1; done`
	trapped := map[syscall.Signal]bool{syscall.SIGTERM: true, syscall.SIGHUP: true, syscall.SIGINT: true}
	// Tests run under nohup, or in the background from a script, start with
	// the signal ignored, and exec rightly leaves it so. An ignored SIGINT
	// has exec leave SIGQUIT ignored too.
	startIgnored := map[syscall.Signal]bool{
		syscall.SIGHUP:  signal.Ignored(syscall.SIGHUP),
		syscall.SIGINT:  signal.Ignored(syscall.SIGINT),
		syscall.SIGQUIT: signal.Ignored(syscall.SIGINT),
	}

	for _, tc := range []struct {
		name     string
		terminal bool
		// ignored is ignored before exec runs.
		ignored syscall.Signal
		// unheeded are sent before sig, and must not end exec.
		unheeded []syscall.Signal
		sig      syscall.Signal
		want     string
		code     exitCode
	}{
		{name: "SIGTERM without a terminal", sig: syscall.SIGTERM, want: "TERM", code: 42},
		{name: "SIGINT without a terminal", sig: syscall.SIGINT, want: "INT", code: 44},
		// The SIGHUP a closed terminal sends.
		{name: "SIGHUP on a terminal", terminal: true, sig: syscall.SIGHUP, want: "HUP", code: 43},
		{name: "SIGQUIT", sig: syscall.SIGQUIT, want: "started", code: 128 + 9},
		// The SIGPIPE of a write to a reader that has gone.
		{name: "SIGPIPE", sig: syscall.SIGPIPE, want: "started", code: 128 + 9},
		{name: "an ignored SIGHUP", ignored: syscall.SIGHUP, unheeded: []syscall.Signal{syscall.SIGHUP},
			sig: syscall.SIGTERM, want: "TERM", code: 42},
		// A shell's background job.
		{name: "an ignored SIGINT", ignored: syscall.SIGINT, unheeded: []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT},
			sig: syscall.SIGTERM, want: "TERM", code: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if startIgnored[tc.sig] {
				t.Skipf("%v was ignored when the tests started", tc.sig)
			}
			// FIFOs a case leaves behind then fail only that case.
			privateFIFODir(t)
			if tc.ignored != 0 {
				signal.Ignore(tc.ignored)
				// signal.Reset would leave signal.Ignored reporting the
				// signal as ignored, and a later exec would not catch it.
				// Notify clears that. exec ignores SIGQUIT along with an
				// ignored SIGINT, and Notify undoes that too.
				t.Cleanup(func() {
					c := make(chan os.Signal, 1)
					signal.Notify(c, tc.ignored, syscall.SIGQUIT)
					signal.Stop(c)
				})
			}
			var out syncBuffer
			app := newApp()
			args := []string{"hull", "--containerd-address", f.addr, "--namespace", f.ns, "exec"}
			var slave *os.File
			var cooked *unix.Termios
			if tc.terminal {
				var master *os.File
				master, slave = newPTY(t)
				go func() { _, _ = io.Copy(&out, master) }()
				app.Reader, app.Writer, app.ErrWriter = slave, slave, slave
				args = append(args, "-t")
				var err error
				cooked, err = unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				app.Reader, app.Writer, app.ErrWriter = strings.NewReader(""), &out, &out
			}
			done := make(chan error, 1)
			go func() {
				done <- app.Run(context.Background(), append(args, "vm", "--", "sh", "-c", script))
			}()
			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(out.String(), "started") {
				if time.Now().After(deadline) {
					t.Fatalf("the command did not start; output %q", out.String())
				}
				time.Sleep(50 * time.Millisecond)
			}
			// The daemon's runtime can start the command with a signal ignored
			// that hull does not ignore: runc passes on a SIGHUP that a
			// containerd started under nohup ignores, when the command starts
			// before runc's own signal handler is in place. Fail with that
			// cause rather than wait out the timeout for a command the signal
			// cannot end.
			m := sigIgnPattern.FindStringSubmatch(out.String())
			if m == nil {
				t.Fatalf("the command printed no SigIgn line; output %q", out.String())
			}
			ignoredMask, err := strconv.ParseUint(m[1], 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			if trapped[tc.sig] && ignoredMask&(1<<(tc.sig-1)) != 0 {
				t.Fatalf("the command started with %v ignored, so it cannot trap it; the runtime passed on an ignored %v (SigIgn %s)", tc.sig, tc.sig, m[1])
			}
			// exec makes the terminal raw only once the command has started,
			// so the command can print before that. The signal is to come in
			// raw mode, which exec has to undo.
			for tc.terminal {
				raw, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if raw.Lflag&unix.ICANON == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("exec did not make the terminal raw")
				}
				time.Sleep(10 * time.Millisecond)
			}

			for _, sig := range tc.unheeded {
				err := syscall.Kill(syscall.Getpid(), sig)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case err = <-done:
					t.Fatalf("exec ended after the ignored %v: error = %v, output %q", sig, err, out.String())
				case <-time.After(time.Second):
				}
			}
			err = syscall.Kill(syscall.Getpid(), tc.sig)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-time.After(15 * time.Second):
				t.Fatalf("exec went on after %v; output %q", tc.sig, out.String())
			}
			var code exitCode
			if !errors.As(err, &code) || code != tc.code || !strings.Contains(out.String(), tc.want) {
				t.Errorf("after %v: error = %v, output %q; want exit status %d and %s", tc.sig, err, out.String(), tc.code, tc.want)
			}
			fifoDir, err := execFIFODir()
			if err != nil {
				t.Fatal(err)
			}
			if fifoDir != "" {
				left, err := os.ReadDir(fifoDir)
				if err != nil || len(left) != 0 {
					t.Errorf("the FIFO directory after exec holds %v, %v; want it empty", left, err)
				}
			}
			if tc.terminal {
				after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if *after != *cooked {
					t.Errorf("the terminal's settings after exec = %+v, want them as before, %+v", *after, *cooked)
				}
			}
		})
	}
}
