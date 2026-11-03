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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

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
}

// rootFIFODir is where root's exec FIFOs go. A variable, so tests run as
// root can point it at a directory of their own.
var rootFIFODir = "/run/hull/fifo"

// ExecFIFODir returns the directory that holds the FIFOs carrying the I/O of
// the execs in instance id of namespace to and from the shim. Each instance
// has one of its own, so that stopping or removing it can find the execs a
// killed hull left in it; see endExecs. Root's are under /run/hull/fifo.
// Anyone else's are in the runtime directory, where a rootless daemon's
// shim, running as the same user, can open them too; see runtimeDir.
// namespace and id must be valid containerd identifiers, which keeps the
// path inside that directory.
func ExecFIFODir(namespace, id string) string {
	base := rootFIFODir
	if geteuid() != 0 {
		base = filepath.Join(runtimeDir(), "hull", "fifo")
	}
	return filepath.Join(base, namespace, id)
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
	// Made only now, so that an exec refused because the instance is not
	// running leaves no directory behind. One that fails from here on
	// leaves it empty: removing it could pull it from under another
	// `hull exec` about to make its FIFOs in it. Stop and rm remove it. A
	// container that loads has a valid ID.
	fifoDir := ExecFIFODir(c.DefaultNamespace(), id)
	err = os.MkdirAll(fifoDir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("create the exec FIFO directory: %w", err)
	}
	opts = append(opts, cio.WithFIFODir(fifoDir))
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
	// Stopping or removing the instance kills and deletes its execs, this
	// one among them; see endExecs. The process is then gone already.
	if err != nil && !errdefs.IsNotFound(err) {
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

// endExecs kills and deletes the execs whose FIFOs are in fifoDir, an
// instance's ExecFIFODir, and removes fifoDir unless it left an exec in
// it; see endExec. An exec hull started is
// deleted by the hull that waits for it, and only one whose hull was
// killed stays, with its FIFOs. Such an exec keeps the instance from
// stopping: the shim reports its task exited only once the task's execs
// have exited, and under urunc, `urunc exec` blocks for good once the
// command's output fills a FIFO nobody reads. A `hull exec` still waiting
// sees its command killed, as stopping the instance would end it anyway.
//
// task is nil for an instance with no task. Its execs went with the task,
// and only their FIFOs are left. Each exec gets grace to exit after its
// SIGKILL.
func endExecs(ctx context.Context, task client.Task, fifoDir string, grace time.Duration) error {
	left := false
	if task != nil {
		ids, err := fifoExecIDs(fifoDir)
		if err != nil {
			return err
		}
		for _, id := range ids {
			err = endExec(ctx, task, id, grace)
			if errors.Is(err, errExecStarting) {
				left = true
				continue
			}
			if err != nil {
				return fmt.Errorf("exec %s: %w", id, err)
			}
		}
	}
	// The FIFOs of an exec left alone stay, so that a later stop or rm, or
	// the ctr commands in docs/build.md, can still find its ID. Each exec
	// that was deleted took its own FIFOs with it.
	if left {
		return nil
	}
	err := os.RemoveAll(fifoDir)
	if err != nil {
		return fmt.Errorf("remove the exec FIFO directory: %w", err)
	}
	return nil
}

// endExec kills and deletes the exec id of task, if it is still there.
// client.WithProcessKill would wait for the exit with no bound, and the
// delete does not pass a deadline on, so the wait is done here.
//
// The exec's FIFOs are attached to, and what comes out of them is thrown
// away. The shim deletes an exec only once it has copied the exec's output
// into them, or after 10 seconds (execProcess.delete in containerd's
// cmd/containerd-shim-runc-v2/process/exec.go; urunc's shim is built from
// the runc shim), and with no reader the copy cannot finish. A `hull exec`
// still waiting loses what the attach reads.
//
// An exec whose start has not returned within grace is left alone, and
// endExec returns errExecStarting. The shim answers nothing about an exec
// while it starts it, and under urunc the start waits for the guest's
// agent, for up to 30 seconds while the guest boots. Ending the instance
// ends such an exec anyway, since the agent it waits for goes with the VM.
func endExec(ctx context.Context, task client.Task, id string, grace time.Duration) error {
	loadCtx, cancelLoad := context.WithTimeout(ctx, grace)
	p, err := task.LoadProcess(loadCtx, id, cio.NewAttach(cio.WithStreams(nil, io.Discard, io.Discard)))
	starting := err != nil && loadCtx.Err() != nil && ctx.Err() == nil
	cancelLoad()
	if starting {
		return errExecStarting
	}
	// The FIFOs go when the hull that started the exec deletes it, between
	// the shim's answer and the attach.
	if errdefs.IsNotFound(err) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = killLeftExec(ctx, p, grace)
	if err != nil {
		// The exec may still be there, so its FIFOs stay for a later stop
		// or rm to find it by.
		stopCopying(p.IO())
		return err
	}
	return deleteLeftExec(ctx, p)
}

// killLeftExec sends p SIGKILL and waits up to grace for it to exit.
func killLeftExec(ctx context.Context, p client.Process, grace time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	exited, err := p.Wait(waitCtx)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	err = p.Kill(ctx, syscall.SIGKILL)
	// The shim refuses to signal an exec it has created but not started, as
	// one is when its hull was killed between the two. It has no process to
	// wait for, and deleting it is enough.
	if errdefs.IsFailedPrecondition(err) {
		return nil
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("send SIGKILL: %w", err)
	}
	// The channel also delivers when waitCtx ends, and the process may then
	// be running still.
	<-exited
	if waitCtx.Err() != nil {
		return fmt.Errorf("still running %s after SIGKILL", grace)
	}
	return nil
}

// errExecStarting is endExec's report that it left an exec alone, because
// the exec's start had not returned within the grace.
var errExecStarting = errors.New("exec still starting")

// deleteLeftExec deletes p, an exec endExec attached to. deleteExec is not
// used, because it removes the FIFOs of an exec the shim fails to delete,
// which a later stop or rm then could not find.
func deleteLeftExec(ctx context.Context, p client.Process) error {
	_, err := p.Delete(context.WithoutCancel(ctx))
	if err == nil {
		return nil
	}
	stopCopying(p.IO())
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete: %w", err)
	}
	// The shim has no such exec any more, and its FIFOs are of no use.
	_ = p.IO().Close()
	return nil
}

// stopCopying ends the copying of an attached exec's output, which holds
// the exec's FIFOs open, and leaves the FIFOs in place. cio.IO.Close would
// remove them as well.
func stopCopying(pio cio.IO) {
	pio.Cancel()
	pio.Wait()
}

// fifoExecIDs returns the IDs of the execs whose FIFOs are in dir. cio makes
// each exec's FIFOs in a directory of their own inside dir, named after the
// exec ID with -stdin, -stdout or -stderr after it. A dir that does not
// exist holds none.
func fifoExecIDs(dir string) ([]string, error) {
	sets, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the exec FIFO directory: %w", err)
	}
	var ids []string
	for _, set := range sets {
		if !set.IsDir() {
			continue
		}
		fifos, err := os.ReadDir(filepath.Join(dir, set.Name()))
		// An exec whose hull deletes it meanwhile takes its directory with
		// it.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the exec FIFO directory: %w", err)
		}
		for _, fifo := range fifos {
			for _, suffix := range []string{"-stdin", "-stdout", "-stderr"} {
				id, ok := strings.CutSuffix(fifo.Name(), suffix)
				if ok && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	slices.Sort(ids)
	return ids, nil
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
