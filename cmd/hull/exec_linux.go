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
	"os"
	"os/signal"
	"syscall"

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

// execUserUsage is the help of exec's --user. urunc hands the guest only a
// uid and a gid, and the names in the image's /etc/passwd are not read; see
// numericUser in internal/containerd.
const execUserUsage = "run as this guest user (uid, uid:gid, or root); default: the instance's user"

// execInstance runs a command in an instance's guest through containerd,
// and ends hull with the command's exit status.
func execInstance(ctx context.Context, cmd *cli.Command) error {
	instanceID, argv, err := execArgs(cmd)
	if err != nil {
		return err
	}
	err = store.ValidateInstanceID(instanceID)
	if err != nil {
		return err
	}
	stdin, stdout, stderr := cmd.Reader, cmd.Writer, cmd.ErrWriter
	opts, err := newExecOptions(cmd, fileFd(stdout), fileFd(stdin), fileFd(stderr))
	if err != nil {
		return err
	}
	fifoDir, err := execFIFODir()
	if err != nil {
		return err
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// Guest output reaching a terminal goes through the filter, as on
	// macOS; see guestTerminalWriter.
	out, errOut := stdout, stderr
	f, ok := stdout.(*os.File)
	if ok {
		out = guestTerminalWriter(f)
	}
	f, ok = stderr.(*os.File)
	if ok {
		errOut = guestTerminalWriter(f)
	}
	// SIGINT, SIGTERM, SIGHUP, SIGQUIT and SIGPIPE, which would end hull,
	// end the command instead. On macOS the guest ends a session whose
	// connection drops, but on Linux `urunc exec` holds the connection, not
	// hull. A hull ended by a signal would leave the command running in the
	// guest, its exec process in the shim and its FIFOs behind, and once the
	// command filled the pipe its output goes to, `urunc exec` would block
	// for good and the shim would not report the instance stopped until
	// `urunc exec` is killed. docs/build.md says what each signal does to
	// the command, and which signals still end hull.
	//
	// SIGQUIT and SIGPIPE kill the exec rather than going to it: urunc does
	// not pass SIGQUIT on, and dies of it with a Go stack dump on the
	// command's stderr, and a SIGPIPE about hull's output means nothing to
	// the command. The signals are caught from before the start, which can
	// wait about 18 seconds for the guest's agent, so one sent meanwhile
	// still ends the exec.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGPIPE)
	// A signal that hull was started with ignored is not meant for the
	// command either: nohup ignores SIGHUP, and a shell ignores SIGINT and
	// SIGQUIT for a background job. Go leaves SIGHUP and SIGINT ignored only
	// while Notify does not ask for them, but it catches an ignored SIGTERM
	// or SIGQUIT regardless, and signal.Ignored cannot tell that either was
	// ignored. So SIGTERM is always caught: left to Go, it would end hull
	// rather than be ignored. A shell ignores SIGINT and SIGQUIT together,
	// so an ignored SIGINT stands for an ignored SIGQUIT too.
	if !signal.Ignored(syscall.SIGHUP) {
		signal.Notify(sigs, syscall.SIGHUP)
	}
	if signal.Ignored(syscall.SIGINT) {
		signal.Ignore(syscall.SIGQUIT)
	} else {
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT)
	}
	defer func() {
		// Once Stop returns no signal is sent on sigs, so closing it is
		// safe, and it ends the relay's loop, which would otherwise wait
		// for good.
		signal.Stop(sigs)
		close(sigs)
	}()
	// A resize during the wait for the agent is caught too, and reaches the
	// guest once the command has started. The size ExecInstance sets right
	// after the start is the one measured before it.
	var followWindowSize func(resize func(rows, cols uint16))
	if opts.tty && opts.sizeFdIsTerminal {
		var stop func()
		followWindowSize, stop = catchWindowSize(opts.sizeFd)
		defer stop()
	}
	e, err := containerd.ExecInstance(ctx, c, instanceID, containerd.ExecConfig{
		Args:     argv,
		Env:      opts.env,
		Cwd:      cmd.String("cwd"),
		User:     cmd.String("user"),
		Terminal: opts.tty,
		Rows:     opts.rows,
		Cols:     opts.cols,
		Stdin:    stdin,
		Stdout:   out,
		Stderr:   errOut,
		FIFODir:  fifoDir,
	})
	switch {
	case errdefs.IsNotFound(err):
		return fmt.Errorf("instance not found: %s", instanceID)
	case err != nil:
		return err
	}

	go func() {
		for sig := range sigs {
			s := sig.(syscall.Signal)
			if s == syscall.SIGQUIT || s == syscall.SIGPIPE {
				s = syscall.SIGKILL
			}
			_ = e.Signal(ctx, s)
		}
	}()

	// Raw mode is stdin's business: it is the descriptor whose echo and
	// line discipline the session takes over. A terminal then carries ^C
	// as a byte.
	stdinFd := fileFd(stdin)
	restore := func() {}
	if opts.tty && isTerminal(stdinFd) {
		state, err := term.MakeRaw(stdinFd)
		if err != nil {
			_ = e.Signal(ctx, syscall.SIGKILL)
			_, _ = e.Wait(ctx)
			return fmt.Errorf("set raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(stdinFd, state) }
		defer restore()
	}
	if followWindowSize != nil {
		followWindowSize(func(rows, cols uint16) { _ = e.Resize(ctx, rows, cols) })
	}

	code, err := e.Wait(ctx)
	switch {
	case errors.Is(err, containerd.ErrExecNotDeleted):
		// The command's status stands: only the clean-up after it failed.
		// The terminal goes back to cooked mode first, or the warning's
		// newline would not return the cursor to the start of the line.
		restore()
		_, _ = fmt.Fprintf(stderr, "warning: %v\n", err)
	case err != nil:
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// fileFd returns the descriptor of s when it is a file, and -1, which no
// terminal check accepts, when it is not.
func fileFd(s any) int {
	f, ok := s.(*os.File)
	if !ok {
		return -1
	}
	return int(f.Fd())
}

// execFIFODir returns containerd.ExecFIFODir, and creates it.
func execFIFODir() (string, error) {
	dir := containerd.ExecFIFODir()
	if dir == "" {
		return "", nil
	}
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return "", fmt.Errorf("create the exec FIFO directory: %w", err)
	}
	return dir, nil
}
