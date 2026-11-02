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
	"io"
	"os"
	"strings"
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
	// containerd's default FIFO directory is root's, and the tests may
	// run as anyone.
	cfg.FIFODir = t.TempDir()
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

func TestExecInstanceThroughDaemon(t *testing.T) {
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
		Args:    []string{"cat"},
		Stdin:   stdinR,
		Stdout:  &late,
		Stderr:  io.Discard,
		FIFODir: t.TempDir(),
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
	e, err = ExecInstance(ctx, c, "vm", ExecConfig{Args: []string{"true"}, FIFODir: t.TempDir()})
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
		{name: "root uses containerd's default", euid: 0, runtimeDir: "/run/user/0", want: ""},
		{name: "user uses XDG_RUNTIME_DIR", euid: 1000, runtimeDir: "/tmp/rt", want: "/tmp/rt/hull/fifo"},
		{name: "user without XDG_RUNTIME_DIR", euid: 1000, want: "/run/user/1000/hull/fifo"},
		{name: "user with a relative XDG_RUNTIME_DIR", euid: 1000, runtimeDir: "rt", want: "/run/user/1000/hull/fifo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeIDs(t, tc.euid, 1000)
			t.Setenv("XDG_RUNTIME_DIR", tc.runtimeDir)
			got := ExecFIFODir()
			if got != tc.want {
				t.Fatalf("ExecFIFODir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// undeletable is a process the shim can no longer delete, as when the shim
// or the task is gone.
type undeletable struct {
	client.Process
	io cio.IO
}

func (p undeletable) Delete(context.Context, ...client.ProcessDeleteOpts) (*client.ExitStatus, error) {
	return nil, errdefs.ErrNotFound
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
	err = deleteExec(context.Background(), undeletable{io: pio})
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
	e := &Exec{process: undeletable{io: pio}, exited: exited}
	code, err := e.Wait(context.Background())
	if code != 3 || !errors.Is(err, ErrExecNotDeleted) || !errors.Is(err, errdefs.ErrNotFound) {
		t.Errorf("Wait = %d, %v; want 3 and an error that is ErrExecNotDeleted and the delete's", code, err)
	}
}
