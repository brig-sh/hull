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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
)

// execAndWait runs cfg in instance id with stdin as its input, and returns
// what it printed and its exit status.
func execAndWait(t *testing.T, c *client.Client, id, stdin string, cfg ExecConfig) (stdout, stderr string, code int) {
	t.Helper()
	ctx := context.Background()
	var out, errOut bytes.Buffer
	cfg.Stdin = strings.NewReader(stdin)
	cfg.Stdout = &out
	cfg.Stderr = &errOut
	e, err := ExecInstance(ctx, c, id, cfg)
	if err != nil {
		t.Fatalf("exec %q: %v", cfg.Args, err)
	}
	code, err = e.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for %q: %v", cfg.Args, err)
	}
	return out.String(), errOut.String(), code
}

// privateFIFODir points the FIFO directories of the test's execs at a
// directory of its own, whoever runs the test, and returns it.
func privateFIFODir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	orig := rootFIFODir
	rootFIFODir = filepath.Join(dir, "hull", "fifo")
	t.Cleanup(func() { rootFIFODir = orig })
	return filepath.Join(dir, "hull", "fifo")
}

func TestExecInstanceThroughDaemon(t *testing.T) {
	privateFIFODir(t)
	c := instancesClient(t)
	ctx := context.Background()
	_, task := containerdtest.StartTask(ctx, t, c, "vm", "sleep", "300")

	// cat ends only if the end of its stdin reaches it.
	out, errOut, code := execAndWait(t, c, "vm", "in\n", ExecConfig{
		Args: []string{"sh", "-c", "echo out; echo err >&2; cat; exit 3"},
	})
	if out != "out\nin\n" || errOut != "err\n" || code != 3 {
		t.Errorf("exec = %q, %q, %d; want %q, %q, 3", out, errOut, code, "out\nin\n", "err\n")
	}
	// Input that ends before the start, as an empty one does, too.
	out, _, code = execAndWait(t, c, "vm", "", ExecConfig{Args: []string{"cat"}})
	if out != "" || code != 0 {
		t.Errorf("cat of an empty stdin = %q, %d; want no output and 0", out, code)
	}

	// Input that ends after the start, as a long input does, reaches cat's
	// end too. ExecInstance returns once the process has started.
	stdinR, stdinW := io.Pipe()
	var late bytes.Buffer
	e, err := ExecInstance(ctx, c, "vm", ExecConfig{
		Args:   []string{"cat"},
		Stdin:  stdinR,
		Stdout: &late,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stdinW.Write([]byte("late\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()
	code, err = e.Wait(ctx)
	if err != nil || late.String() != "late\n" || code != 0 {
		t.Errorf("exec with stdin ending after the start = %q, %d, %v; want %q, 0", late.String(), code, err, "late\n")
	}

	// The flags reach the runtime, and the instance's PATH is replaced, not
	// repeated.
	out, _, _ = execAndWait(t, c, "vm", "", ExecConfig{
		Args: []string{"sh", "-c", `echo "$FOO $PATH"; env | grep -c ^PATH=; pwd; id -u; id -g`},
		Env:  []string{"FOO=bar", "PATH=/bin"},
		Cwd:  "/tmp",
		User: "1000:1001",
	})
	want := "bar /bin\n1\n/tmp\n1000\n1001\n"
	if out != want {
		t.Errorf("exec with env, cwd and user printed %q, want %q", out, want)
	}

	// runc sizes the terminal from the spec's consoleSize, so this does not
	// show the resize ExecInstance sends for urunc, which ignores it.
	out, _, code = execAndWait(t, c, "vm", "", ExecConfig{
		Args:     []string{"sh", "-c", "test -t 0 && stty size"},
		Terminal: true,
		Rows:     30,
		Cols:     100,
	})
	if out != "30 100\r\n" || code != 0 {
		t.Errorf("exec on a terminal = %q, %d; want %q, 0", out, code, "30 100\r\n")
	}

	// Wait deletes the process, so the shim keeps nothing of it.
	e, err = ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = task.LoadProcess(ctx, e.process.ID(), nil)
	if !errdefs.IsNotFound(err) {
		t.Errorf("loading the process after Wait: error = %v, want not found", err)
	}
}

func TestExecInstanceRefusals(t *testing.T) {
	fifoBase := privateFIFODir(t)
	c := instancesClient(t)
	ctx := context.Background()
	newIdleContainer(ctx, t, c, "idle")
	containerdtest.StartTask(ctx, t, c, "vm", "sleep", "300")

	_, err := ExecInstance(ctx, c, "missing", ExecConfig{Args: []string{"true"}})
	if !errdefs.IsNotFound(err) {
		t.Errorf("exec in a missing instance: error = %v, want not found", err)
	}
	_, err = ExecInstance(ctx, c, "idle", ExecConfig{Args: []string{"true"}})
	want := "instance idle is not running (status: stopped)"
	if err == nil || err.Error() != want {
		t.Errorf("exec in an instance with no task: error = %v, want %s", err, want)
	}
	_, err = ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"true"}, User: "nobody"})
	if err == nil || !strings.Contains(err.Error(), "takes only a numeric uid and gid, or root") {
		t.Errorf("exec as a user name: error = %v, want a refusal", err)
	}
	_, err = os.Stat(fifoBase)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused exec made a FIFO directory: %v", err)
	}
}

// The process starts as the instance's and keeps what the config leaves
// unset: the environment's other variables, the directory and the user.
func TestExecProcessStartsFromTheInstance(t *testing.T) {
	base := &specs.Process{
		Args: []string{"sleep", "infinity"},
		Env:  []string{"PATH=/usr/bin", "HOME=/home/app"},
		Cwd:  "/srv",
		User: specs.User{UID: 5, GID: 6},
	}
	p, err := execProcess(base, ExecConfig{Args: []string{"id"}, Env: []string{"PATH=/bin", "FOO=baz", "FOO=bar"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Args, " ") != "id" || strings.Join(p.Env, " ") != "PATH=/bin HOME=/home/app FOO=bar" ||
		p.Cwd != "/srv" || p.User.UID != 5 || p.User.GID != 6 || p.Terminal || p.ConsoleSize != nil {
		t.Errorf("process = %+v, want id with PATH replaced, HOME kept, the last FOO added, in /srv as 5:6", p)
	}
	if strings.Join(base.Env, " ") != "PATH=/usr/bin HOME=/home/app" {
		t.Errorf("the instance's environment became %q, want it unchanged", base.Env)
	}

	p, err = execProcess(base, ExecConfig{Args: []string{"sh"}, Cwd: "/tmp", User: "root", Terminal: true, Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	if p.Cwd != "/tmp" || p.User.UID != 0 || p.User.GID != 0 || !p.Terminal ||
		p.ConsoleSize == nil || p.ConsoleSize.Height != 30 || p.ConsoleSize.Width != 100 {
		t.Errorf("process = %+v, want sh in /tmp as root on a 30x100 terminal", p)
	}
}

func TestExecFIFODir(t *testing.T) {
	cases := []struct {
		name       string
		runtimeDir string
		euid       int
		want       string
	}{
		{name: "root uses /run/hull", euid: 0, runtimeDir: "/run/user/0", want: "/run/hull/fifo/ns/vm"},
		{name: "user uses XDG_RUNTIME_DIR", euid: 1000, runtimeDir: "/tmp/rt", want: "/tmp/rt/hull/fifo/ns/vm"},
		{name: "user without XDG_RUNTIME_DIR", euid: 1000, want: "/run/user/1000/hull/fifo/ns/vm"},
		{name: "user with a relative XDG_RUNTIME_DIR", euid: 1000, runtimeDir: "rt", want: "/run/user/1000/hull/fifo/ns/vm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeIDs(t, tc.euid, 1000)
			t.Setenv("XDG_RUNTIME_DIR", tc.runtimeDir)
			got := ExecFIFODir("ns", "vm")
			if got != tc.want {
				t.Fatalf("ExecFIFODir(ns, vm) = %q, want %q", got, tc.want)
			}
		})
	}
}

// undeletable is a process the shim does not delete, failing with err: not
// found when the shim, the task or the process is gone, and unavailable
// when the shim cannot be reached.
type undeletable struct {
	client.Process
	io  cio.IO
	err error
}

func (p undeletable) Delete(context.Context, ...client.ProcessDeleteOpts) (*client.ExitStatus, error) {
	return nil, p.err
}

func (p undeletable) IO() cio.IO { return p.io }

func (p undeletable) ID() string { return "hull-test" }

// A process whose deletion fails still has its FIFOs removed.
func TestDeleteExecRemovesTheFIFOsWhenDeleteFails(t *testing.T) {
	dir := t.TempDir()
	pio, err := cio.NewCreator(cio.WithStreams(nil, io.Discard, io.Discard), cio.WithFIFODir(dir))("hull-test")
	if err != nil {
		t.Fatal(err)
	}
	err = deleteExec(context.Background(), undeletable{io: pio, err: errdefs.ErrNotFound})
	if !errors.Is(err, errdefs.ErrNotFound) {
		t.Errorf("deleteExec = %v, want the error Delete returned", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Errorf("the FIFO directory after deleteExec holds %v, %v; want it empty", left, err)
	}
}

// A process that exited but could not be deleted still has its exit status
// returned, along with the error.
func TestWaitKeepsTheStatusWhenDeleteFails(t *testing.T) {
	pio, err := cio.NewCreator(cio.WithStreams(nil, io.Discard, io.Discard), cio.WithFIFODir(t.TempDir()))("hull-test")
	if err != nil {
		t.Fatal(err)
	}
	// No shim opens the FIFOs, and the copying would wait for one for good.
	pio.Cancel()
	exited := make(chan client.ExitStatus, 1)
	exited <- *client.NewExitStatus(3, time.Now(), nil)
	e := &Exec{process: undeletable{io: pio, err: errdefs.ErrUnavailable}, exited: exited}
	code, err := e.Wait(context.Background())
	if code != 3 || !errors.Is(err, ErrExecNotDeleted) || !errors.Is(err, errdefs.ErrUnavailable) {
		t.Errorf("Wait = %d, %v; want 3 and an error that is ErrExecNotDeleted and the delete's", code, err)
	}

	// A process that is not found was deleted by a stop or rm of its
	// instance, and is not left in the shim.
	exited <- *client.NewExitStatus(137, time.Now(), nil)
	e = &Exec{process: undeletable{io: pio, err: errdefs.ErrNotFound}, exited: exited}
	code, err = e.Wait(context.Background())
	if code != 137 || err != nil {
		t.Errorf("Wait for a process deleted already = %d, %v; want 137 and no error", code, err)
	}
}

func TestFIFOExecIDs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a/hull-1-stdin", "a/hull-1-stdout", "a/hull-1-stderr", "b/hull-2-stdout", "c/other", "stray"} {
		err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o700)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(dir, name), nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	ids, err := fifoExecIDs(dir)
	if err != nil || strings.Join(ids, " ") != "hull-1 hull-2" {
		t.Errorf("fifoExecIDs = %q, %v; want hull-1 and hull-2", ids, err)
	}
	ids, err = fifoExecIDs(filepath.Join(dir, "missing"))
	if err != nil || len(ids) != 0 {
		t.Errorf("fifoExecIDs of a missing directory = %q, %v; want none", ids, err)
	}
}

// The execs a killed hull left in an instance keep it from stopping under
// urunc, so stop and rm kill and delete them, and remove their FIFOs. runc
// ends them along with the task, so endExecs is called on its own first,
// on a task that keeps running.
func TestStopAndRemoveEndTheExecsLeftBehind(t *testing.T) {
	privateFIFODir(t)
	c := instancesClient(t)
	ctx := context.Background()
	_, task := containerdtest.StartTask(ctx, t, c, "vm", "sleep", "300")
	fifoDir := ExecFIFODir(c.DefaultNamespace(), "vm")

	// A hull still waiting for its exec gets the command's kill as its
	// status, with no warning that the exec was not deleted.
	e, err := ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"sleep", "300"}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		code, err := e.Wait(ctx)
		if err == nil && code != 137 {
			err = fmt.Errorf("exit status %d, want 137", code)
		}
		waited <- err
	}()
	// The FIFOs of an exec whose process is gone already.
	err = os.MkdirAll(filepath.Join(fifoDir, "gone"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(fifoDir, "gone", "hull-0000000000000000-stdout"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = endExecs(ctx, task, fifoDir, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = task.LoadProcess(ctx, e.process.ID(), nil)
	if !errdefs.IsNotFound(err) {
		t.Errorf("load the exec after endExecs: error = %v, want not found", err)
	}
	select {
	case err = <-waited:
		if err != nil {
			t.Errorf("Wait for the exec endExecs ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Wait for the exec endExecs ended did not return")
	}
	_, err = os.Stat(fifoDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the FIFO directory after endExecs: %v, want it gone", err)
	}

	// An exec left behind by a hull that no longer reads its output. Its
	// copying blocks once the FIFO is full, as a killed hull's would, and
	// the shim then waits 10 seconds to delete it unless endExecs drains the
	// FIFO.
	unread, stalled := io.Pipe()
	t.Cleanup(func() { _ = unread.Close() })
	_, err = ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"yes"}, Stdout: stalled, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// rm without --force refuses a running instance and leaves its execs.
	err = RemoveInstance(ctx, c, "vm", false, 5*time.Second, nil)
	if !errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("rm of a running instance: error = %v, want ErrInstanceRunning", err)
	}
	ids, err := fifoExecIDs(fifoDir)
	if err != nil || len(ids) != 1 {
		t.Errorf("the execs after a refused rm = %q, %v; want the one left behind", ids, err)
	}
	start := time.Now()
	_, err = StopInstance(ctx, c, "vm", time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The stop's own wait for the task is the 1 second timeout, then the
	// SIGKILL.
	took := time.Since(start)
	if took > 5*time.Second {
		t.Errorf("stop took %v with an exec whose output nobody read; want it drained", took)
	}
	_, err = os.Stat(fifoDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the FIFO directory after stop: %v, want it gone", err)
	}

	containerdtest.StartTask(ctx, t, c, "vm2", "sleep", "300")
	fifoDir = ExecFIFODir(c.DefaultNamespace(), "vm2")
	_, err = ExecInstance(ctx, c, "vm2", ExecConfig{Args: []string{"sleep", "300"}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	err = RemoveInstance(ctx, c, "vm2", true, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(fifoDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the FIFO directory after rm --force: %v, want it gone", err)
	}
}

// An exec the shim created but never started, as one is when its hull is
// killed between the two, has no process to kill, and stop and rm delete it.
func TestStopAndRemoveDeleteAnExecNeverStarted(t *testing.T) {
	privateFIFODir(t)
	c := instancesClient(t)
	ctx := context.Background()
	for _, stop := range []bool{true, false} {
		ctr, task := containerdtest.StartTask(ctx, t, c, "vm", "sleep", "300")
		fifoDir := ExecFIFODir(c.DefaultNamespace(), "vm")
		spec, err := ctr.Spec(ctx)
		if err != nil {
			t.Fatal(err)
		}
		proc, err := execProcess(spec.Process, ExecConfig{Args: []string{"true"}})
		if err != nil {
			t.Fatal(err)
		}
		err = os.MkdirAll(fifoDir, 0o700)
		if err != nil {
			t.Fatal(err)
		}
		p, err := task.Exec(ctx, "hull-0000000000000001", proc,
			cio.NewCreator(cio.WithStreams(nil, io.Discard, io.Discard), cio.WithFIFODir(fifoDir)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.IO().Close() })
		if stop {
			_, err = StopInstance(ctx, c, "vm", time.Second, 5*time.Second)
		} else {
			err = RemoveInstance(ctx, c, "vm", true, 5*time.Second, nil)
		}
		if err != nil {
			t.Fatalf("stop %v: %v", stop, err)
		}
		_, err = os.Stat(fifoDir)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the FIFO directory after stop %v: %v, want it gone", stop, err)
		}
		if stop {
			err = RemoveInstance(ctx, c, "vm", false, 5*time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// An exec whose hull deletes it between the shim's answer and the attach
// takes its FIFOs with it, and is gone, not a failure.
func TestEndExecSkipsAnExecWhoseFIFOsWent(t *testing.T) {
	privateFIFODir(t)
	c := instancesClient(t)
	ctx := context.Background()
	_, task := containerdtest.StartTask(ctx, t, c, "vm", "sleep", "300")
	e, err := ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"sleep", "300"}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	fifoDir := ExecFIFODir(c.DefaultNamespace(), "vm")
	err = os.RemoveAll(fifoDir)
	if err != nil {
		t.Fatal(err)
	}
	err = endExec(ctx, task, e.process.ID(), 5*time.Second)
	if err != nil {
		t.Errorf("endExec of an exec with no FIFOs: %v", err)
	}
	err = e.process.Kill(ctx, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
}

// startingTask is a task whose shim does not answer about an exec, as the
// shim does not while it starts one.
type startingTask struct {
	client.Task
}

func (startingTask) LoadProcess(ctx context.Context, _ string, _ cio.Attach) (client.Process, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// An exec still starting is left alone after grace, so it does not hold
// off the end of its instance, and its FIFOs stay for a later stop or rm.
// A stop whose own context ends fails.
func TestEndExecLeavesAnExecStillStarting(t *testing.T) {
	fifoDir := t.TempDir()
	fifo := filepath.Join(fifoDir, "set", "hull-test-stdout")
	err := os.MkdirAll(filepath.Dir(fifo), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(fifo, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = endExecs(context.Background(), startingTask{}, fifoDir, 100*time.Millisecond)
	if err != nil {
		t.Errorf("endExecs with an exec still starting: %v", err)
	}
	took := time.Since(start)
	if took > 5*time.Second {
		t.Errorf("endExecs with an exec still starting took %v, want about the grace", took)
	}
	_, err = os.Stat(fifo)
	if err != nil {
		t.Errorf("the FIFOs of the exec left alone: %v, want them kept", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = endExec(ctx, startingTask{}, "hull-test", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("endExec with its context done = %v, want context.Canceled", err)
	}
}

// leftExec is an exec left behind, attached to FIFOs nothing writes to, so
// the attach's copying ends only when it is cancelled. It exits on SIGKILL
// only if exits is set, and its delete fails with deleteErr.
type leftExec struct {
	client.Process
	io        cio.IO
	exits     bool
	deleteErr error
}

func (p *leftExec) Wait(ctx context.Context) (<-chan client.ExitStatus, error) {
	exited := make(chan client.ExitStatus, 1)
	if p.exits {
		exited <- *client.NewExitStatus(137, time.Now(), nil)
		return exited, nil
	}
	go func() {
		<-ctx.Done()
		exited <- *client.NewExitStatus(client.UnknownExitStatus, time.Time{}, ctx.Err())
	}()
	return exited, nil
}

func (*leftExec) Kill(context.Context, syscall.Signal, ...client.KillOpts) error { return nil }

func (p *leftExec) Delete(context.Context, ...client.ProcessDeleteOpts) (*client.ExitStatus, error) {
	return nil, p.deleteErr
}

func (p *leftExec) IO() cio.IO { return p.io }

// leftTask is a task whose one exec is exec, with its FIFOs in dir.
type leftTask struct {
	client.Task
	dir  string
	exec *leftExec
}

func (t leftTask) LoadProcess(_ context.Context, id string, attach cio.Attach) (client.Process, error) {
	fifos, err := cio.NewFIFOSetInDir(t.dir, id, false)
	if err != nil {
		return nil, err
	}
	t.exec.io, err = attach(fifos)
	if err != nil {
		return nil, err
	}
	return t.exec, nil
}

// An exec that stop or rm cannot kill or delete may still be there. Its
// FIFOs stay for a later stop or rm to find it by, and the copying that
// held them open ends. One the shim no longer has goes with its FIFOs.
func TestEndExecKeepsTheFIFOsOfAnExecItCannotEnd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exec      leftExec
		wantErr   string
		wantFIFOs bool
	}{
		{"still running", leftExec{}, "still running 50ms after SIGKILL", true},
		{"undeletable", leftExec{exits: true, deleteErr: errdefs.ErrUnavailable}, "delete: unavailable", true},
		{"gone", leftExec{exits: true, deleteErr: errdefs.ErrNotFound}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			task := leftTask{dir: dir, exec: &tc.exec}
			err := endExec(context.Background(), task, "hull-test", 50*time.Millisecond)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("endExec = %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("endExec = %v, want an error containing %q", err, tc.wantErr)
			}
			copied := make(chan struct{})
			go func() {
				tc.exec.io.Wait()
				close(copied)
			}()
			select {
			case <-copied:
			case <-time.After(5 * time.Second):
				t.Error("the attach's copying still runs after endExec")
				tc.exec.io.Cancel()
			}
			ids, err := fifoExecIDs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantFIFOs && len(ids) != 1 {
				t.Errorf("the execs with FIFOs after endExec = %q, want the exec's FIFOs kept", ids)
			}
			if !tc.wantFIFOs && len(ids) != 0 {
				t.Errorf("the execs with FIFOs after endExec = %q, want the exec's FIFOs removed", ids)
			}
		})
	}
}
