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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/brig-sh/hull/pkg/store"
)

func TestInstanceRecord(t *testing.T) {
	created := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	exited := created.Add(time.Minute)
	cases := []struct {
		name   string
		task   *containerd.Task
		status string
		pid    int
		exit   *int
	}{
		{"no task", nil, "stopped", 0, nil},
		{"created", &containerd.Task{Status: client.Created, Pid: 41}, store.StatusStarting, 41, nil},
		{"running", &containerd.Task{Status: client.Running, Pid: 42}, "running", 42, nil},
		{"paused", &containerd.Task{Status: client.Paused, Pid: 43}, "paused", 43, nil},
		{"stopped", &containerd.Task{Status: client.Stopped, ExitStatus: 3, ExitTime: exited}, "stopped", 0, ptr(3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := instanceRecord(containerd.Instance{ID: "vm", ContainerID: "c0ffee", CreatedAt: created, Task: tc.task})
			if rec.ID != "vm" || rec.ContainerID != "c0ffee" || !rec.StartTime.Equal(created) {
				t.Errorf("id %q, container ID %q, start time %v; want vm, c0ffee, %v",
					rec.ID, rec.ContainerID, rec.StartTime, created)
			}
			if rec.StartTime.Location() != time.Local {
				t.Errorf("start time in %v, want local", rec.StartTime.Location())
			}
			if rec.Status != tc.status || rec.PID != tc.pid {
				t.Errorf("status %q, pid %d; want %q, %d", rec.Status, rec.PID, tc.status, tc.pid)
			}
			switch {
			case tc.exit == nil && rec.ExitCode != nil:
				t.Errorf("exit code %d, want none", *rec.ExitCode)
			case tc.exit != nil && (rec.ExitCode == nil || *rec.ExitCode != *tc.exit):
				t.Errorf("exit code %v, want %d", rec.ExitCode, *tc.exit)
			case tc.exit != nil && !rec.ExitedAt.Equal(exited):
				t.Errorf("exited at %v, want %v", rec.ExitedAt, exited)
			case tc.exit != nil && rec.ExitedAt.Location() != time.Local:
				t.Errorf("exit time in %v, want local", rec.ExitedAt.Location())
			}
		})
	}
}

// A task that could not be read may still be running, so it must not read
// as stopped, and it has no pid to signal.
func TestInstanceRecordOfAnUnreadableTask(t *testing.T) {
	created := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	rec := instanceRecord(containerd.Instance{ID: "vm", CreatedAt: created, Err: errors.New("shim does not answer")})
	if rec.Status != store.StatusUnreadable || rec.PID != 0 || rec.ExitCode != nil || !rec.StartTime.Equal(created) {
		t.Errorf("record = %+v, want unreadable with the creation time, no pid and no exit code", rec)
	}
}

func ptr[T any](v T) *T { return &v }

// instanceFixture is a namespace of the test's own on the test daemon, to
// create containers in and run hull against.
type instanceFixture struct {
	t    *testing.T
	addr string
	ns   string
	c    *client.Client
}

func newInstanceFixture(t *testing.T) *instanceFixture {
	t.Helper()
	addr := containerdtest.DaemonAddress(t)
	ns := containerdtest.NamespaceName("hull-test-ps")
	c, err := containerd.Connect(context.Background(), addr, ns)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last in, first out: the containers a test creates are
	// gone before the images and the namespace, and the client is closed
	// last.
	t.Cleanup(func() { _ = c.Close() })
	containerdtest.DeleteNamespace(t, c)
	return &instanceFixture{t: t, addr: addr, ns: ns, c: c}
}

// run runs hull against the fixture's daemon in namespace ns and returns
// what it printed.
func (f *instanceFixture) run(ns string, args ...string) (string, error) {
	f.t.Helper()
	out, _, err := f.runStderr(ns, args...)
	return out, err
}

// runStderr is run, and also returns what hull printed to stderr.
func (f *instanceFixture) runStderr(ns string, args ...string) (stdout, stderr string, err error) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	app := newApp()
	app.Writer = &out
	app.ErrWriter = &errOut
	argv := append([]string{"hull", "--containerd-address", f.addr, "--namespace", ns}, args...)
	err = app.Run(context.Background(), argv)
	return out.String(), errOut.String(), err
}

// psRow returns the fields of the `hull ps` row for id, split the way brig
// splits them, or nil when there is none.
func psRow(table, id string) []string {
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == id {
			return f
		}
	}
	return nil
}

func TestInstanceCommandsAgainstDaemon(t *testing.T) {
	f := newInstanceFixture(t)
	ctx := context.Background()
	idle, err := f.c.NewContainer(ctx, "idle", client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idle.Delete(ctx) })
	containerdtest.StartTask(ctx, t, f.c, "running", "sleep", "300")

	table, err := f.run(f.ns, "ps")
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if !strings.HasPrefix(table, "ID  ") {
		t.Errorf("ps printed no header:\n%s", table)
	}
	// brig's Running reads the first two fields of a row.
	row := psRow(table, "running")
	if len(row) < 4 || row[1] != "running" || row[2] != "-" {
		t.Errorf("ps row for running = %q, want status running, no exit code and a pid", row)
	}
	row = psRow(table, "idle")
	if len(row) < 2 || row[1] != "stopped" {
		t.Errorf("ps row for idle = %q, want status stopped", row)
	}
	// brig's List asks for ps -a first.
	for _, flag := range []string{"-a", "--all"} {
		all, err := f.run(f.ns, "ps", flag)
		if err != nil || all != table {
			t.Errorf("ps %s = %q, %v; want the table ps prints:\n%s", flag, all, err, table)
		}
	}

	other, err := f.run(f.ns+"-other", "ps")
	if err != nil || strings.TrimSpace(other) != "No instances found" {
		t.Errorf("ps in another namespace = %q, %v; want no instances", other, err)
	}

	out, err := f.run(f.ns, "inspect", "running")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var state store.InstanceState
	err = json.Unmarshal([]byte(out), &state)
	if err != nil {
		t.Fatalf("inspect printed %q: %v", out, err)
	}
	if state.ID != "running" || state.Status != "running" || state.PID == 0 || state.StartTime.IsZero() {
		t.Errorf("inspect = %+v, want running with a pid and a start time", state)
	}

	web, err := f.c.NewContainer(ctx, "ctr-web", client.WithSpec(&oci.Spec{}),
		client.WithContainerLabels(map[string]string{containerd.LabelName: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = web.Delete(ctx) })
	out, err = f.run(f.ns, "inspect", "web")
	if err != nil {
		t.Fatalf("inspect web: %v", err)
	}
	var raw map[string]any
	err = json.Unmarshal([]byte(out), &raw)
	if err != nil || raw["id"] != "web" || raw["containerId"] != "ctr-web" {
		t.Errorf("inspect web printed %s (%v), want id web and containerId ctr-web", out, err)
	}
	out, err = f.run(f.ns, "rm", "web")
	if err != nil || out != "Instance web removed\n" {
		t.Errorf("rm web = %q, %v", out, err)
	}

	_, err = f.run(f.ns, "inspect", "missing")
	if err == nil || err.Error() != "instance not found: missing" {
		t.Errorf("inspect missing: error = %v, want instance not found: missing", err)
	}
	_, err = f.run(f.ns, "rm", "missing")
	if err == nil || err.Error() != "instance not found: missing" {
		t.Errorf("rm missing: error = %v, want instance not found: missing", err)
	}

	_, err = f.run(f.ns, "rm", "running")
	if err == nil || err.Error() != "instance is running, use --force to stop it first" {
		t.Errorf("rm running: error = %v, want a refusal", err)
	}
	out, err = f.run(f.ns, "rm", "--force", "running")
	if err != nil || out != "Instance running removed\n" {
		t.Errorf("rm --force running = %q, %v", out, err)
	}
	out, err = f.run(f.ns, "rm", "idle")
	if err != nil || out != "Instance idle removed\n" {
		t.Errorf("rm idle = %q, %v", out, err)
	}

	table, err = f.run(f.ns, "ps")
	if err != nil || strings.TrimSpace(table) != "No instances found" {
		t.Errorf("ps after rm = %q, %v; want no instances", table, err)
	}
}

func TestInstanceCommandsRefuseBadNames(t *testing.T) {
	for _, verb := range []string{"inspect", "logs", "rm", "stop"} {
		var out bytes.Buffer
		app := newApp()
		app.Writer = &out
		// The name is refused before any daemon is dialled, so the address
		// names none.
		err := app.Run(context.Background(), []string{"hull", "--containerd-address", "/nonexistent.sock", verb, "../x"})
		if err == nil || !strings.Contains(err.Error(), "invalid instance name") {
			t.Errorf("%s ../x: error = %v, want invalid instance name", verb, err)
		}
		err = app.Run(context.Background(), []string{"hull", verb})
		if err == nil || err.Error() != "instance ID required" {
			t.Errorf("%s with no name: error = %v, want instance ID required", verb, err)
		}
	}
}

// Every verb fails on a name two containers carry, with the error that names
// both, and says the name once.
func TestInstanceCommandsReportADuplicateName(t *testing.T) {
	f := newInstanceFixture(t)
	ctx := context.Background()
	for _, id := range []string{"dupa", "dupb"} {
		ctr, err := f.c.NewContainer(ctx, id, client.WithSpec(&oci.Spec{}),
			client.WithContainerLabels(map[string]string{containerd.LabelName: "dup"}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ctr.Delete(ctx) })
	}
	for _, verb := range []string{"inspect", "logs", "rm", "stop"} {
		_, err := f.run(f.ns, verb, "dup")
		if err == nil || !strings.Contains(err.Error(), "instance dup is held by 2 containers, dupa, dupb:") ||
			strings.Count(err.Error(), "instance dup") != 1 {
			t.Errorf("%s dup: error = %v, want the duplicate named once", verb, err)
		}
	}
}

// rm removes the log and the network directory at the paths run gives the
// name, and not a file the container's log-file label names: any client of
// the namespace can set that label.
func TestRemoveDeletesOnlyTheInstanceLog(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	ctx := context.Background()
	decoy := filepath.Join(t.TempDir(), "keep")
	err := os.WriteFile(decoy, []byte("not a log"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := f.c.NewContainer(ctx, "vm", client.WithSpec(&oci.Spec{}),
		client.WithContainerLabels(map[string]string{containerd.LabelLogFile: decoy}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })
	logFile, err := instanceLogFile(f.ns, "vm")
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(logFile, []byte("console\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	netDir, err := instanceNetDir(f.ns, "vm")
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(netDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(netDir, "resolv.conf"), []byte("nameserver 192.0.2.1\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(netDir, containerd.HookErrorFile), []byte("lease store unreadable\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, err := f.runStderr(f.ns, "rm", "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "could not detach instance vm: lease store unreadable") {
		t.Errorf("rm's stderr = %q, want the failed detach reported", stderr)
	}
	_, err = os.Stat(logFile)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log %s: error = %v, want it removed", logFile, err)
	}
	_, err = os.Stat(netDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("network directory %s: error = %v, want it removed", netDir, err)
	}
	_, err = os.Stat(decoy)
	if err != nil {
		t.Errorf("%s, named by the label: %v, want it kept", decoy, err)
	}
}

// stop reports a detach that failed when the task was deleted, which the
// hook cannot report itself.
func TestStopReportsAFailedDetach(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	containerdtest.StartTask(context.Background(), t, f.c, "vm", "sleep", "300")
	netDir, err := instanceNetDir(f.ns, "vm")
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(netDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(netDir, containerd.HookErrorFile), []byte("lease store unreadable\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// sleep, as the task's first process, ignores the SIGTERM.
	out, stderr, err := f.runStderr(f.ns, "stop", "--timeout", "1", "vm")
	if err != nil || out != "Instance vm stopped\n" {
		t.Errorf("stop = %q, %v", out, err)
	}
	if stderr != "warning: the network hook could not detach instance vm: lease store unreadable\n" {
		t.Errorf("stop's stderr = %q, want the failed detach reported", stderr)
	}
}

// newAttachedInstance creates instance name in f's namespace as a container
// whose spec names the network hook, and its network directory, with the
// hook's record of an attachment when attached is set. The task is gone, as
// after a host reboot, so the runtime never runs the hook.
func newAttachedInstance(t *testing.T, f *instanceFixture, name string, attached bool) (client.Container, string) {
	t.Helper()
	ctx := context.Background()
	netDir, err := instanceNetDir(f.ns, name)
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(netDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		err = os.WriteFile(filepath.Join(netDir, networkResultFile), []byte(`{"ip":"10.44.0.9"}`), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	hookArgs := []string{"/gone/hull", "internal", "oci-hook", "--dir", netDir, "postStop"}
	spec := &oci.Spec{Version: specs.Version, Hooks: &specs.Hooks{
		Poststop: []specs.Hook{{Path: "/gone/hull", Args: hookArgs, Env: []string{"PATH=/usr/bin:/bin", "HOOK_TEST=1"}}},
	}}
	ctr, err := f.c.NewContainer(ctx, "ctr-"+name, client.WithSpec(spec),
		client.WithContainerLabels(map[string]string{containerd.LabelName: name}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })
	return ctr, netDir
}

// fakeHook stands in for the hull binary rm runs the hook with: a script
// that records its arguments, its environment and its stdin in a directory
// it returns, and exits with code.
func fakeHook(t *testing.T, code int) string {
	t.Helper()
	out := t.TempDir()
	script := filepath.Join(t.TempDir(), "hull")
	body := "#!/bin/sh\necho \"$@\" > " + out + "/args\nenv > " + out + "/env\ncat > " + out + "/state\n" +
		"echo hook output\nexit " + strconv.Itoa(code) + "\n"
	err := os.WriteFile(script, []byte(body), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	old := hookExecutable
	hookExecutable = func() (string, error) { return script, nil }
	t.Cleanup(func() { hookExecutable = old })
	return out
}

// rm runs the postStop hook of an instance whose task went without running
// it, as detach, with the spec's other arguments, its environment and the
// container's state, from the hull binary rm is: the spec's may be gone.
func TestRemoveDetachesWhatTheHookNeverDid(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	out := fakeHook(t, 0)
	ctr, netDir := newAttachedInstance(t, f, "web", true)

	_, stderr, err := f.runStderr(f.ns, "rm", "web")
	if err != nil || stderr != "" {
		t.Fatalf("rm web: %v, stderr %q", err, stderr)
	}
	args, err := os.ReadFile(filepath.Join(out, "args"))
	if err != nil || strings.TrimSpace(string(args)) != "internal oci-hook --dir "+netDir+" detach" {
		t.Errorf("hook arguments = %q, %v; want the spec's, after the executable, with detach for postStop", args, err)
	}
	env, err := os.ReadFile(filepath.Join(out, "env"))
	if err != nil || !strings.Contains(string(env), "HOOK_TEST=1") {
		t.Errorf("hook environment = %q, %v; want the spec's", env, err)
	}
	var state specs.State
	data, err := os.ReadFile(filepath.Join(out, "state"))
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err != nil || state.ID != ctr.ID() || state.Status != specs.StateStopped {
		t.Errorf("hook state = %s, %v; want container %s, stopped", data, err, ctr.ID())
	}
	_, err = os.Stat(netDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("network directory: error = %v, want it removed", err)
	}
}

// rm runs no hook for an instance its hook already detached. When the
// detach it runs fails, rm fails and keeps the instance, so the next rm
// tries again, and rm --force removes it with a warning.
func TestRemoveDetachesOnlyWhatIsLeft(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	out := fakeHook(t, 1)
	newAttachedInstance(t, f, "detached", false)
	_, stderr, err := f.runStderr(f.ns, "rm", "detached")
	if err != nil || stderr != "" {
		t.Fatalf("rm detached: %v, stderr %q", err, stderr)
	}
	_, err = os.Stat(filepath.Join(out, "args"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hook ran for a detached instance: %v", err)
	}

	ctr, netDir := newAttachedInstance(t, f, "web", true)
	_, err = f.run(f.ns, "rm", "web")
	if err == nil || !strings.Contains(err.Error(), "could not detach it from its network, so it is kept") ||
		!strings.Contains(err.Error(), "hook output") {
		t.Errorf("rm web with a failing detach: error = %v, want the failed hook reported with its output", err)
	}
	_, err = f.c.LoadContainer(context.Background(), ctr.ID())
	if err != nil {
		t.Errorf("container after a failed detach: %v, want it kept", err)
	}
	_, err = os.Stat(filepath.Join(netDir, networkResultFile))
	if err != nil {
		t.Errorf("the hook's record after a failed detach: %v, want it kept", err)
	}

	_, stderr, err = f.runStderr(f.ns, "rm", "--force", "web")
	if err != nil {
		t.Fatalf("rm --force web: %v", err)
	}
	if !strings.Contains(stderr, "warning: could not detach instance web from its network: ") ||
		!strings.Contains(stderr, "hook output") {
		t.Errorf("rm --force's stderr = %q, want the failed hook reported with its output", stderr)
	}
	_, err = f.c.LoadContainer(context.Background(), ctr.ID())
	if !errdefs.IsNotFound(err) {
		t.Errorf("container after rm --force: error = %v, want it removed", err)
	}
	_, err = os.Stat(netDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("network directory after rm --force: error = %v, want it removed", err)
	}
}

// rm runs no hook of its own when the task it deletes runs its postStop
// hook, which removes the hook's record of the attachment.
func TestRemoveLeavesTheDetachToTheRuntime(t *testing.T) {
	privateLogDirs(t)
	f := newInstanceFixture(t)
	out := fakeHook(t, 0)
	netDir, err := instanceNetDir(f.ns, "web")
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(netDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(netDir, networkResultFile), []byte(`{"ip":"10.44.0.9"}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// runc runs both at the task's delete: the first stands in for the
	// network hook's detach, and the second names the hook for rm to find.
	hooks := func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Hooks = &specs.Hooks{Poststop: []specs.Hook{
			{Path: "/bin/sh", Args: []string{"sh", "-c", "rm -f " + filepath.Join(netDir, networkResultFile)}},
			{Path: "/bin/true", Args: []string{"/gone/hull", "internal", "oci-hook", "--dir", netDir, "postStop"}},
		}}
		return nil
	}
	containerdtest.StartTaskWithSpec(context.Background(), t, f.c, "web", []oci.SpecOpts{hooks}, "sleep", "300")

	_, stderr, err := f.runStderr(f.ns, "rm", "--force", "web")
	if err != nil || stderr != "" {
		t.Fatalf("rm --force web: %v, stderr %q", err, stderr)
	}
	_, err = os.Stat(filepath.Join(out, "args"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rm ran the hook after the runtime had: %v", err)
	}
}
