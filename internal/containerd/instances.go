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
	"fmt"
	"sort"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
)

// Instance is a container in the client's namespace: a hull instance, or a
// container another client such as ctr or nerdctl created there.
type Instance struct {
	ID        string
	CreatedAt time.Time
	// Labels are the container's labels, among them the ones hull sets on
	// a container it creates: LabelBackend, LabelImageDigest and
	// LabelLogFile.
	Labels map[string]string
	// Task is nil when the container has no task. containerd cannot tell a
	// container that was never started from one whose task was deleted
	// after it stopped.
	Task *Task
	// Err is set when Instances could not read the container's task, as
	// when its shim does not answer. Task is then nil, though the container
	// may still have a task, running or not.
	Err error
}

// Task is the state of an instance's task, as its shim reports it.
type Task struct {
	Status client.ProcessStatus
	Pid    uint32
	// ExitStatus and ExitTime are set only when Status is client.Stopped.
	ExitStatus uint32
	ExitTime   time.Time
}

// Instances describes the containers in the client's namespace, sorted by
// ID. A container whose task cannot be read is still listed, with Err set.
func Instances(ctx context.Context, c *client.Client) ([]Instance, error) {
	containers, err := c.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return describeInstances(ctx, containers), nil
}

func describeInstances(ctx context.Context, containers []client.Container) []Instance {
	instances := make([]Instance, 0, len(containers))
	for _, ctr := range containers {
		// containerd waits a few seconds for a shim that does not answer
		// and then fails the request. Failing the whole listing on that
		// would hide every other container, and brig reads the listing to
		// track all of its sandboxes.
		inst, err := describeInstance(ctx, ctr)
		inst.Err = err
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
	return instances
}

// LoadInstance describes the container id. An error that errdefs.IsNotFound
// accepts means no container has that ID in the client's namespace.
func LoadInstance(ctx context.Context, c *client.Client, id string) (Instance, error) {
	ctr, err := c.LoadContainer(ctx, id)
	if err != nil {
		return Instance{}, fmt.Errorf("load container %s: %w", id, err)
	}
	return describeInstance(ctx, ctr)
}

// describeInstance returns what it read of ctr even when it also returns an
// error, so Instances can still list the container.
func describeInstance(ctx context.Context, ctr client.Container) (Instance, error) {
	inst := Instance{ID: ctr.ID()}
	// The container came from a listing or a load, which fetched its
	// record; asking again would cost a request per container.
	info, err := ctr.Info(ctx, client.WithoutRefreshedMetadata)
	if err != nil {
		return inst, fmt.Errorf("container %s: %w", ctr.ID(), err)
	}
	inst.CreatedAt = info.CreatedAt
	inst.Labels = info.Labels
	inst.Task, err = describeTask(ctx, ctr)
	return inst, err
}

// describeTask returns the state of ctr's task, or nil when it has none.
func describeTask(ctx context.Context, ctr client.Container) (*Task, error) {
	task, status, err := loadTask(ctx, ctr)
	if task == nil || err != nil {
		return nil, err
	}
	t := &Task{Status: status.Status}
	if status.Status == client.Stopped {
		t.ExitStatus = status.ExitStatus
		t.ExitTime = status.ExitTime
	} else {
		t.Pid = task.Pid()
	}
	return t, nil
}

// loadTask returns ctr's task and its status, or a nil task when it has
// none. A task deleted between the two requests counts as none.
func loadTask(ctx context.Context, ctr client.Container) (client.Task, client.Status, error) {
	task, err := ctr.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil, client.Status{}, nil
	}
	if err != nil {
		return nil, client.Status{}, fmt.Errorf("task of container %s: %w", ctr.ID(), err)
	}
	status, err := task.Status(ctx)
	if errdefs.IsNotFound(err) {
		return nil, client.Status{}, nil
	}
	if err != nil {
		return nil, client.Status{}, fmt.Errorf("status of container %s: %w", ctr.ID(), err)
	}
	return task, status, nil
}

// ErrInstanceRunning is RemoveInstance refusing an instance whose task has
// not stopped.
var ErrInstanceRunning = errors.New("instance is running")

// RemoveInstance deletes the container id, its task and its snapshot. A task
// that has not stopped is refused with ErrInstanceRunning unless kill is
// set. Then it is sent SIGKILL first, and RemoveInstance waits up to grace
// for it to exit, so the snapshot is never removed from under a VMM that may
// still be writing to it. An error that errdefs.IsNotFound accepts means no
// container has that ID in the client's namespace.
func RemoveInstance(ctx context.Context, c *client.Client, id string, kill bool, grace time.Duration) error {
	ctr, err := c.LoadContainer(ctx, id)
	if err != nil {
		return fmt.Errorf("load container %s: %w", id, err)
	}
	err = deleteTask(ctx, ctr, kill, grace)
	if err != nil {
		return err
	}
	err = ctr.Delete(ctx, client.WithSnapshotCleanup)
	if err != nil {
		return fmt.Errorf("delete container %s: %w", id, err)
	}
	return nil
}

func deleteTask(ctx context.Context, ctr client.Container, kill bool, grace time.Duration) error {
	task, status, err := loadTask(ctx, ctr)
	if task == nil || err != nil {
		return err
	}
	if status.Status == client.Stopped {
		_, err = task.Delete(ctx)
	} else {
		if !kill {
			return ErrInstanceRunning
		}
		killCtx, cancel := context.WithTimeout(ctx, grace)
		_, err = task.Delete(killCtx, client.WithProcessKill)
		timedOut := errors.Is(killCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil && timedOut {
			return fmt.Errorf("task of container %s is still running %s after SIGKILL; "+
				"refusing to delete it while it may still be writing to its snapshot", ctr.ID(), grace)
		}
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete task of container %s: %w", ctr.ID(), err)
	}
	return nil
}
