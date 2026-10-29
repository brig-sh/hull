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
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"

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
			rec := instanceRecord(containerd.Instance{ID: "vm", CreatedAt: created, Task: tc.task})
			if rec.ID != "vm" || !rec.StartTime.Equal(created) {
				t.Errorf("id %q, start time %v; want vm, %v", rec.ID, rec.StartTime, created)
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
	var out bytes.Buffer
	app := newApp()
	app.Writer = &out
	argv := append([]string{"hull", "--containerd-address", f.addr, "--namespace", ns}, args...)
	err := app.Run(context.Background(), argv)
	return out.String(), err
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

// rm removes the log at the path run gives the name, and not a file the
// container's log-file label names: any client of the namespace can set
// that label.
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

	_, err = f.run(f.ns, "rm", "vm")
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(logFile)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log %s: error = %v, want it removed", logFile, err)
	}
	_, err = os.Stat(decoy)
	if err != nil {
		t.Errorf("%s, named by the label: %v, want it kept", decoy, err)
	}
}
