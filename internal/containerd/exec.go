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

package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// ExecConfig describes a process ExecInstance runs in an instance. What it
// leaves unset comes from the instance's own process.
type ExecConfig struct {
	Args []string
	// Env adds to the instance's environment, and a variable set in both
	// takes the value Env gives it.
	Env []string
	Cwd string
	// User is a uid, a uid:gid or root; see numericUser.
	User string
	// Terminal runs the process on a terminal of Rows by Cols, and all of
	// its output then comes on Stdout.
	Terminal   bool
	Rows, Cols uint16
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	// FIFODir is where the FIFOs that carry the process's I/O to and from
	// the shim are made. Empty means containerd's default, which only root
	// can write.
	FIFODir string
}

// ExecFIFODir returns where an exec's FIFOs should be made, for
// ExecConfig.FIFODir. containerd's default, /run/containerd/fifo, is root's,
// so root gets "" for it. Anyone else gets a directory in the runtime
// directory, which a rootless daemon's shim, running as the same user, can
// open too; see runtimeDir. The directory may not exist yet.
func ExecFIFODir() string {
	if geteuid() == 0 {
		return ""
	}
	return filepath.Join(runtimeDir(), "hull", "fifo")
}

// Exec is a process ExecInstance started.
type Exec struct {
	process client.Process
	exited  <-chan client.ExitStatus
}

// ExecInstance starts cfg's process in the running task of instance id. The
// caller must call Wait. An error that errdefs.IsNotFound accepts means no
// container has that ID in the client's namespace.
//
// Under urunc the process runs in the guest: the shim runs `urunc exec`,
// which relays the I/O to the guest's urunit-agent and exits with the
// guest process's status.
func ExecInstance(ctx context.Context, c *client.Client, id string, cfg ExecConfig) (*Exec, error) {
	ctr, err := c.LoadContainer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load container %s: %w", id, err)
	}
	task, status, err := loadTask(ctx, ctr)
	if err != nil {
		return nil, err
	}
	// A container with no task is stopped, as Instances reports it.
	if task == nil {
		return nil, notRunning(id, client.Stopped)
	}
	if status.Status != client.Running {
		return nil, notRunning(id, status.Status)
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("spec of container %s: %w", id, err)
	}
	proc, err := execProcess(spec.Process, cfg)
	if err != nil {
		return nil, err
	}
	execID, err := newExecID()
	if err != nil {
		return nil, err
	}

	// The end of stdin is passed on only without a terminal, because
	// closing a terminal process's stdin hangs up its terminal. A command on
	// a terminal sees the end of its input only when a ^D typed on the
	// terminal reaches it; nothing is sent when stdin ends.
	var stdin *stdinCloser
	streams := cio.WithStreams(cfg.Stdin, cfg.Stdout, cfg.Stderr)
	if cfg.Stdin != nil && !cfg.Terminal {
		stdin = &stdinCloser{r: cfg.Stdin}
		streams = cio.WithStreams(stdin, cfg.Stdout, cfg.Stderr)
	}
	opts := []cio.Opt{streams}
	if cfg.Terminal {
		opts = append(opts, cio.WithTerminal)
	}
	if cfg.FIFODir != "" {
		opts = append(opts, cio.WithFIFODir(cfg.FIFODir))
	}
	p, err := task.Exec(ctx, execID, proc, cio.NewCreator(opts...))
	// The task can end and be deleted after loadTask saw it running, and
	// any call from here to the start can then fail with not found. That
	// error is not passed on, since it would read as no container.
	if errdefs.IsNotFound(err) {
		return nil, notRunning(id, client.Stopped)
	}
	if err != nil {
		return nil, fmt.Errorf("exec in %s: %w", id, err)
	}
	exited, err := p.Wait(ctx)
	if err != nil {
		_ = deleteExec(ctx, p)
		if errdefs.IsNotFound(err) {
			return nil, notRunning(id, client.Stopped)
		}
		return nil, fmt.Errorf("wait for exec in %s: %w", id, err)
	}
	err = p.Start(ctx)
	if err != nil {
		_ = deleteExec(ctx, p)
		if errdefs.IsNotFound(err) {
			return nil, notRunning(id, client.Stopped)
		}
		return nil, fmt.Errorf("exec in %s: %w", id, err)
	}
	if stdin != nil {
		stdin.started(func() { _ = p.CloseIO(ctx, client.WithStdinCloser) })
	}
	e := &Exec{process: p, exited: exited}
	// runc sizes the terminal from the spec's consoleSize, but urunc does
	// not: it makes a terminal of its own, whose size the shim sets only
	// when asked to resize it, and it passes the size on to the guest when
	// it sees it change. Until then the guest's terminal is 24x80.
	if cfg.Terminal {
		_ = e.Resize(ctx, cfg.Rows, cfg.Cols)
	}
	return e, nil
}

func notRunning(id string, status client.ProcessStatus) error {
	return fmt.Errorf("instance %s is not running (status: %s)", id, status)
}

// execProcess returns base, the process of an instance, changed into cfg's.
func execProcess(base *specs.Process, cfg ExecConfig) (*specs.Process, error) {
	if base == nil {
		return nil, errors.New("the container's spec has no process")
	}
	if len(cfg.Args) == 0 {
		return nil, errors.New("no command to run")
	}
	p := *base
	p.Args = cfg.Args
	p.Env = append([]string{}, base.Env...)
	// oci.WithEnv replaces a variable the instance sets rather than adding
	// a second copy, as nerdctl exec does, so the process gets one value
	// for it whatever the runtime makes of duplicates. It does not look
	// among the variables it adds, though, so each entry goes through it on its own, and of two
	// --env entries for a variable the last one wins.
	for _, entry := range cfg.Env {
		err := oci.WithEnv([]string{entry})(context.Background(), nil, nil, &oci.Spec{Process: &p})
		if err != nil {
			return nil, err
		}
	}
	p.Terminal = cfg.Terminal
	p.ConsoleSize = nil
	if cfg.Terminal {
		p.ConsoleSize = &specs.Box{Height: uint(cfg.Rows), Width: uint(cfg.Cols)}
	}
	if cfg.Cwd != "" {
		p.Cwd = cfg.Cwd
	}
	if cfg.User != "" {
		uid, gid, err := numericUser(cfg.User)
		if err != nil {
			return nil, fmt.Errorf("run as user %q: %w", cfg.User, err)
		}
		p.User = specs.User{UID: uid, GID: gid}
	}
	return &p, nil
}

// newExecID returns an ID for an exec process. The ID has to be unique only
// among the task's processes.
func newExecID() (string, error) {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("make an exec ID: %w", err)
	}
	return "hull-" + hex.EncodeToString(b), nil
}

// Resize sets the size of the process's terminal.
func (e *Exec) Resize(ctx context.Context, rows, cols uint16) error {
	return e.process.Resize(ctx, uint32(cols), uint32(rows))
}

// Signal sends sig to the process.
func (e *Exec) Signal(ctx context.Context, sig syscall.Signal) error {
	return e.process.Kill(ctx, sig)
}

// ErrExecNotDeleted is what Exec.Wait's error matches when the process
// exited but the shim could not delete it.
var ErrExecNotDeleted = errors.New("exec process not deleted")

// Wait waits for the process to exit, then for its output to be copied out,
// deletes it, and returns its exit status. When the delete fails, Wait
// returns the status still, with an error that errors.Is matches to
// ErrExecNotDeleted.
//
// The output is waited for first because Delete closes the FIFOs before it
// waits for the copying, and would cut off output still in them.
func (e *Exec) Wait(ctx context.Context) (int, error) {
	status := <-e.exited
	code, _, err := status.Result()
	if err != nil {
		_ = deleteExec(ctx, e.process, client.WithProcessKill)
		return 0, fmt.Errorf("wait for exec: %w", err)
	}
	e.process.IO().Wait()
	err = deleteExec(ctx, e.process)
	if err != nil {
		return int(code), fmt.Errorf("%w: %s: %w", ErrExecNotDeleted, e.process.ID(), err)
	}
	return int(code), nil
}

// deleteExec deletes p in the shim, and its FIFOs with it. Delete removes
// the FIFOs only once the shim has deleted the process, so when the shim or
// the task is gone and that fails, deleteExec removes them itself, or they
// would stay in the FIFO directory.
func deleteExec(ctx context.Context, p client.Process, opts ...client.ProcessDeleteOpts) error {
	_, err := p.Delete(context.WithoutCancel(ctx), opts...)
	if err != nil {
		pio := p.IO()
		if pio != nil {
			pio.Cancel()
			pio.Wait()
			_ = pio.Close()
		}
	}
	return err
}

// stdinCloser passes on a process's stdin, and closes the process's end
// once it reads EOF, so a command reading its stdin to the end, as cat
// does, gets the end of it. containerd's I/O copying closes only its own
// write end of the stdin FIFO. The shim keeps a write end of its own open
// on it, so the process sees no end of input until CloseIO has the shim
// close that one.
//
// EOF can come before the process has started, when CloseIO cannot be
// sent yet. started sends it then.
type stdinCloser struct {
	r     io.Reader
	mu    sync.Mutex
	eof   bool
	close func()
}

func (s *stdinCloser) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		s.mu.Lock()
		s.eof = true
		closeIO := s.close
		s.mu.Unlock()
		if closeIO != nil {
			closeIO()
		}
	}
	return n, err
}

// started records how to close the process's stdin, and closes it at once
// if stdin has already ended.
func (s *stdinCloser) started(closeIO func()) {
	s.mu.Lock()
	s.close = closeIO
	eof := s.eof
	s.mu.Unlock()
	if eof {
		closeIO()
	}
}
