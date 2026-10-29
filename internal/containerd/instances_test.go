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
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
)

// instancesClient connects to the test daemon in a namespace of the test's
// own, and deletes the namespace with its images when the test ends.
func instancesClient(t *testing.T) *client.Client {
	t.Helper()
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), addr, containerdtest.NamespaceName("hull-test-instances"))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last in, first out: the containers a test creates are
	// gone before the images, and the client is closed last.
	t.Cleanup(func() { _ = c.Close() })
	containerdtest.DeleteNamespace(t, c)
	return c
}

// newIdleContainer creates a container with no task and no snapshot.
func newIdleContainer(ctx context.Context, t *testing.T, c *client.Client, id string) client.Container {
	t.Helper()
	ctr, err := c.NewContainer(ctx, id, client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })
	return ctr
}

func TestInstancesThroughDaemon(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	newIdleContainer(ctx, t, c, "b-idle")
	newIdleContainer(ctx, t, c, "a-idle")

	instances, err := Instances(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[0].ID != "a-idle" || instances[1].ID != "b-idle" {
		t.Fatalf("instances = %+v, want a-idle and b-idle in that order", instances)
	}
	for _, inst := range instances {
		if inst.Task != nil {
			t.Errorf("%s: task = %+v, want none", inst.ID, inst.Task)
		}
		if inst.CreatedAt.IsZero() {
			t.Errorf("%s: no creation time", inst.ID)
		}
	}

	_, err = LoadInstance(ctx, c, "missing")
	if !errdefs.IsNotFound(err) {
		t.Errorf("LoadInstance(missing) error = %v, want not found", err)
	}
}

// fakeContainer is a container whose task lookup returns taskErr, so a test
// can stand in for a shim that does not answer without a daemon. Calling any
// other method panics on the nil embedded interface.
type fakeContainer struct {
	client.Container
	id      string
	created time.Time
	taskErr error
}

func (f fakeContainer) ID() string { return f.id }

func (f fakeContainer) Info(context.Context, ...client.InfoOpts) (containers.Container, error) {
	return containers.Container{ID: f.id, CreatedAt: f.created}, nil
}

func (f fakeContainer) Task(context.Context, cio.Attach) (client.Task, error) {
	return nil, f.taskErr
}

func TestInstancesListsAContainerWhoseTaskCannotBeRead(t *testing.T) {
	created := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	instances := describeInstances(context.Background(), []client.Container{
		fakeContainer{id: "wedged", created: created, taskErr: context.DeadlineExceeded},
		fakeContainer{id: "idle", created: created, taskErr: errdefs.ErrNotFound},
	})
	if len(instances) != 2 || instances[0].ID != "idle" || instances[1].ID != "wedged" {
		t.Fatalf("instances = %+v, want idle and wedged in that order", instances)
	}
	idle, wedged := instances[0], instances[1]
	if idle.Err != nil || idle.Task != nil {
		t.Errorf("idle = %+v, want no task and no error", idle)
	}
	if !errors.Is(wedged.Err, context.DeadlineExceeded) || wedged.Task != nil || !wedged.CreatedAt.Equal(created) {
		t.Errorf("wedged = %+v, want the deadline error, no task and the creation time", wedged)
	}
}

func TestInstanceTaskStates(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	containerdtest.StartTask(ctx, t, c, "sleeper", "sleep", "300")
	_, exited := containerdtest.StartTask(ctx, t, c, "exited", "sh", "-c", "exit 3")
	exitCh, err := exited.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-exitCh

	inst, err := LoadInstance(ctx, c, "sleeper")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Task == nil || inst.Task.Status != client.Running || inst.Task.Pid == 0 {
		t.Errorf("sleeper task = %+v, want running with a pid", inst.Task)
	}

	inst, err = LoadInstance(ctx, c, "exited")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Task == nil || inst.Task.Status != client.Stopped || inst.Task.ExitStatus != 3 ||
		inst.Task.ExitTime.IsZero() || inst.Task.Pid != 0 {
		t.Errorf("exited task = %+v, want stopped with status 3, an exit time and no pid", inst.Task)
	}
}

func TestRemoveInstanceThroughDaemon(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	newIdleContainer(ctx, t, c, "idle")
	running, _ := containerdtest.StartTask(ctx, t, c, "running", "sleep", "300")
	info, err := running.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = RemoveInstance(ctx, c, "missing", true, time.Second)
	if !errdefs.IsNotFound(err) {
		t.Errorf("removing a missing instance: error = %v, want not found", err)
	}

	err = RemoveInstance(ctx, c, "idle", false, time.Second)
	if err != nil {
		t.Fatalf("removing an instance with no task: %v", err)
	}
	assertNoContainer(ctx, t, c, "idle")

	err = RemoveInstance(ctx, c, "running", false, time.Second)
	if !errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("removing a running instance without kill: error = %v, want ErrInstanceRunning", err)
	}
	_, err = c.LoadContainer(ctx, "running")
	if err != nil {
		t.Fatalf("the refusal removed the container: %v", err)
	}

	err = RemoveInstance(ctx, c, "running", true, 5*time.Second)
	if err != nil {
		t.Fatalf("removing a running instance with kill: %v", err)
	}
	assertNoContainer(ctx, t, c, "running")
	_, err = c.SnapshotService(info.Snapshotter).Stat(ctx, info.SnapshotKey)
	if !errdefs.IsNotFound(err) {
		t.Errorf("snapshot %s: error = %v, want it removed", info.SnapshotKey, err)
	}
}

// A task still alive when the grace ends keeps its container and snapshot.
// No process outlives SIGKILL, so a grace of a nanosecond stands in for one
// that would: the deadline has passed before the task is even waited on.
func TestRemoveInstanceRefusesATaskThatOutlivesTheGrace(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	ctr, _ := containerdtest.StartTask(ctx, t, c, "stubborn", "sleep", "300")
	info, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = RemoveInstance(ctx, c, "stubborn", true, time.Nanosecond)
	if err == nil || errors.Is(err, ErrInstanceRunning) || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("removing past the grace: error = %v, want a refusal to delete", err)
	}
	inst, err := LoadInstance(ctx, c, "stubborn")
	if err != nil {
		t.Fatalf("the refusal removed the container: %v", err)
	}
	if inst.Task == nil || inst.Task.Status != client.Running {
		t.Errorf("task = %+v, want it still running", inst.Task)
	}
	_, err = c.SnapshotService(info.Snapshotter).Stat(ctx, info.SnapshotKey)
	if err != nil {
		t.Errorf("snapshot %s: %v, want it kept", info.SnapshotKey, err)
	}
}

func TestRemoveInstanceWhoseTaskExited(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	_, task := containerdtest.StartTask(ctx, t, c, "exited", "true")
	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-exitCh

	err = RemoveInstance(ctx, c, "exited", false, time.Second)
	if err != nil {
		t.Fatalf("removing an instance whose task exited: %v", err)
	}
	assertNoContainer(ctx, t, c, "exited")
}

// StopInstance deletes the task, and keeps the container and its snapshot
// for hull rm. A container's first process gets no signal it has no handler
// for, so busybox's sleep, run on its own, ignores SIGTERM and needs the
// SIGKILL, and the shell that traps SIGTERM exits on it.
func TestStopInstanceThroughDaemon(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	newIdleContainer(ctx, t, c, "idle")
	polite, _ := containerdtest.StartTask(ctx, t, c, "polite", "sh", "-c", "trap 'exit 0' TERM; sleep 300 & wait")
	containerdtest.StartTask(ctx, t, c, "stubborn", "sleep", "300")
	_, task := containerdtest.StartTask(ctx, t, c, "exited", "true")
	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-exitCh

	_, err = StopInstance(ctx, c, "missing", time.Second, time.Second)
	if !errdefs.IsNotFound(err) {
		t.Errorf("stopping a missing instance: error = %v, want not found", err)
	}

	for _, tc := range []struct {
		id      string
		timeout time.Duration
		running bool
	}{
		{"idle", time.Second, false},
		{"exited", time.Second, false},
		// Exits on SIGTERM, well before the timeout.
		{"polite", time.Minute, true},
		{"stubborn", 100 * time.Millisecond, true},
		{"polite", time.Second, false},
	} {
		start := time.Now()
		running, err := StopInstance(ctx, c, tc.id, tc.timeout, 5*time.Second)
		if err != nil || running != tc.running {
			t.Errorf("stopping %s = %v, %v; want %v, no error", tc.id, running, err, tc.running)
		}
		elapsed := time.Since(start)
		if tc.timeout == time.Minute && elapsed > 30*time.Second {
			t.Errorf("stopping %s took %s; want it ended by SIGTERM, not SIGKILL", tc.id, elapsed)
		}
		inst, err := LoadInstance(ctx, c, tc.id)
		if err != nil || inst.Task != nil {
			t.Errorf("%s after the stop: task = %+v, error = %v; want the container, with no task", tc.id, inst.Task, err)
		}
	}

	info, err := polite.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SnapshotService(info.Snapshotter).Stat(ctx, info.SnapshotKey)
	if err != nil {
		t.Errorf("snapshot %s: %v, want it kept", info.SnapshotKey, err)
	}
}

// A task still alive when the grace after SIGKILL ends keeps its container
// and its task. A grace of a nanosecond ends before the task's exit can
// reach the client, though the kill itself is sent.
func TestStopInstanceFailsWhenTheGraceEnds(t *testing.T) {
	c := instancesClient(t)
	ctx := context.Background()
	containerdtest.StartTask(ctx, t, c, "stubborn", "sleep", "300")

	running, err := StopInstance(ctx, c, "stubborn", 0, time.Nanosecond)
	if !running || err == nil || !strings.Contains(err.Error(), "after SIGKILL") {
		t.Fatalf("stopping past the grace = %v, %v; want a failure after SIGKILL", running, err)
	}
	inst, err := LoadInstance(ctx, c, "stubborn")
	if err != nil || inst.Task == nil {
		t.Errorf("after the failed stop: task = %+v, error = %v; want the task kept", inst.Task, err)
	}
}

// fakeTask is a task whose kill succeeds, so a test can deliver any exit
// status without a daemon. Calling any other method panics on the nil
// embedded interface.
type fakeTask struct {
	client.Task
}

func (fakeTask) Kill(context.Context, syscall.Signal, ...client.KillOpts) error { return nil }

// A task deleted between StopInstance reading it and waiting on it fails
// the wait with not found. It has ended, and the not found must not reach
// hull stop, which would report that no such container exists.
func TestSignalAndWaitTakesADeletedTaskAsEnded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr error
	}{
		{"deleted", errdefs.ErrNotFound, nil},
		{"context ended", context.Canceled, context.Canceled},
	} {
		exited := make(chan client.ExitStatus, 1)
		exited <- *client.NewExitStatus(client.UnknownExitStatus, time.Time{}, tc.err)
		done, err := signalAndWait(context.Background(), fakeTask{}, exited, syscall.SIGTERM, time.Minute)
		if !done || !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: signalAndWait = %v, %v; want true, %v", tc.name, done, err, tc.wantErr)
		}
	}
}

func assertNoContainer(ctx context.Context, t *testing.T, c *client.Client, id string) {
	t.Helper()
	_, err := c.LoadContainer(ctx, id)
	if !errdefs.IsNotFound(err) {
		t.Errorf("container %s: error = %v, want it removed", id, err)
	}
}
