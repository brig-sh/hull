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
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"

	"github.com/brig-sh/hull/pkg/store"
)

// Instance is a container in the client's namespace: a hull instance, or a
// container another client such as ctr or nerdctl created there.
type Instance struct {
	// ID is the instance's name; see instanceName.
	ID string
	// ContainerID is the container's ID, which only containerd's own tools
	// take; hull inspect prints it for them.
	ContainerID string
	CreatedAt   time.Time
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
// name. A container whose task cannot be read is still listed, with Err set.
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

// LoadInstance describes the instance id. An error that errdefs.IsNotFound
// accepts means no instance has that name in the client's namespace.
func LoadInstance(ctx context.Context, c *client.Client, id string) (Instance, error) {
	ctr, err := LoadNamed(ctx, c, id)
	if err != nil {
		return Instance{}, err
	}
	return describeInstance(ctx, ctr)
}

// LoadNamed returns the container of the instance name in the client's
// namespace: the one whose LabelName is name. A container with no valid
// LabelName, as one ctr or nerdctl created, is the instance its ID names.
// An error that errdefs.IsNotFound accepts means that no container is
// instance name.
//
// Two containers labeled with one name, as two runs under different locks
// leave when one is killed before it removes its own (see CreateInstance),
// are an error for every command that acts on the name, which names both and
// how to remove one. Instances still lists both, under the name.
func LoadNamed(ctx context.Context, c *client.Client, name string) (client.Container, error) {
	// A name that is not valid is no container's LabelName; see labelName.
	if ValidateName(name) == nil {
		containers, err := named(ctx, c, name)
		if err != nil {
			return nil, err
		}
		if len(containers) > 1 {
			ids := make([]string, 0, len(containers))
			for _, ctr := range containers {
				ids = append(ids, ctr.ID())
			}
			sort.Strings(ids)
			return nil, fmt.Errorf("instance %s is held by %d containers, %s: remove all but one with "+
				"ctr -n %s tasks rm -f <id> and ctr -n %s containers rm <id>",
				name, len(ids), strings.Join(ids, ", "), c.DefaultNamespace(), c.DefaultNamespace())
		}
		if len(containers) == 1 {
			return containers[0], nil
		}
	}
	return loadUnlabeled(ctx, c, name)
}

// loadUnlabeled returns the container of ID id when it has no valid
// LabelName, so it is the instance its ID names. An error that
// errdefs.IsNotFound accepts means there is no such container.
func loadUnlabeled(ctx context.Context, c *client.Client, id string) (client.Container, error) {
	ctr, err := c.LoadContainer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load container %s: %w", id, err)
	}
	// Read the record the load fetched, rather than ask the daemon again.
	info, err := ctr.Info(ctx, client.WithoutRefreshedMetadata)
	if err != nil {
		return nil, fmt.Errorf("container %s: %w", id, err)
	}
	// A container whose ID is the name, but which is labeled as another
	// instance, is that instance and not this one.
	if labelName(info.Labels) != "" {
		return nil, fmt.Errorf("instance %s: %w", id, errdefs.ErrNotFound)
	}
	return ctr, nil
}

// named returns the containers whose LabelName is name.
func named(ctx context.Context, c *client.Client, name string) ([]client.Container, error) {
	containers, err := c.Containers(ctx, fmt.Sprintf("labels.%q==%q", LabelName, name))
	if err != nil {
		return nil, fmt.Errorf("find instance %s: %w", name, err)
	}
	return containers, nil
}

// nameHolders returns the IDs of every container that holds name: those
// labeled with it, and one of that ID with no valid label, as a client
// other than hull creates.
func nameHolders(ctx context.Context, c *client.Client, name string) ([]string, error) {
	containers, err := named(ctx, c, name)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers)+1)
	for _, ctr := range containers {
		ids = append(ids, ctr.ID())
	}
	_, err = loadUnlabeled(ctx, c, name)
	if err == nil {
		ids = append(ids, name)
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, err
	}
	return ids, nil
}

// maxNameLen is the longest name an instance on Linux can have. The name
// is only a label on the container, whose ID is one of its own, but it
// names the instance's log, <name>.log, and a file name is at most 255
// bytes.
const maxNameLen = 255 - len(".log")

// ValidateName checks that name can be an instance's name on Linux: one
// hull's commands take, as store.ValidateInstanceID checks, and at most
// maxNameLen characters, where macOS takes 255. As on macOS and as in
// nerdctl, a name may hold two of '.', '_' and '-' in a row and end in
// one, which a containerd identifier may not.
func ValidateName(name string) error {
	err := store.ValidateInstanceID(name)
	if err != nil {
		return err
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("%w %q: a name is at most %d characters on Linux, so that its log, <name>.log, "+
			"fits in a file name", store.ErrInvalidInstanceID, name, maxNameLen)
	}
	return nil
}

// labelName returns the LabelName in labels, or "" when there is none, or
// it is not a name ValidateName accepts. Any client of the namespace can set
// a label, and a name goes into hull ps's table, which brig parses line by
// line: one holding a tab or a newline could forge a row.
func labelName(labels map[string]string) string {
	name := labels[LabelName]
	if ValidateName(name) != nil {
		return ""
	}
	return name
}

// instanceName returns the name of the instance the container id, with
// labels, is: its LabelName, else its ID.
func instanceName(id string, labels map[string]string) string {
	name := labelName(labels)
	if name == "" {
		return id
	}
	return name
}

// describeInstance returns what it read of ctr even when it also returns an
// error, so Instances can still list the container.
func describeInstance(ctx context.Context, ctr client.Container) (Instance, error) {
	inst := Instance{ID: ctr.ID(), ContainerID: ctr.ID()}
	// The container came from a listing or a load, which fetched its
	// record; asking again would cost a request per container.
	info, err := ctr.Info(ctx, client.WithoutRefreshedMetadata)
	if err != nil {
		return inst, fmt.Errorf("container %s: %w", ctr.ID(), err)
	}
	inst.ID = instanceName(ctr.ID(), info.Labels)
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

// RemoveInstance deletes the container of instance id, its task and its
// snapshot, and the execs a killed hull left in it, with their FIFOs. A task
// that has not stopped is refused with ErrInstanceRunning unless kill is
// set. Then it is sent SIGKILL first, and RemoveInstance waits up to grace
// for it to exit, so the snapshot is never removed from under a VMM that may
// still be writing to it. An error that errdefs.IsNotFound accepts means no
// instance has that name in the client's namespace.
//
// taskGone, when not nil, runs once the task is gone and before the
// container is deleted, so it can still read the container. An error from
// it keeps the container, and RemoveInstance returns the error.
func RemoveInstance(ctx context.Context, c *client.Client, id string, kill bool, grace time.Duration,
	taskGone func(context.Context, client.Container) error) error {
	ctr, err := LoadNamed(ctx, c, id)
	if err != nil {
		return err
	}
	err = deleteTask(ctx, ctr, ExecFIFODir(c.DefaultNamespace(), ctr.ID()), kill, grace)
	if err != nil {
		return err
	}
	if taskGone != nil {
		err = taskGone(ctx, ctr)
		if err != nil {
			return err
		}
	}
	err = ctr.Delete(ctx, client.WithSnapshotCleanup)
	if err != nil {
		return fmt.Errorf("delete container %s: %w", ctr.ID(), err)
	}
	return nil
}

func deleteTask(ctx context.Context, ctr client.Container, fifoDir string, kill bool, grace time.Duration) error {
	task, status, err := loadTask(ctx, ctr)
	if err != nil {
		return err
	}
	running := task != nil && status.Status != client.Stopped
	if running && !kill {
		return ErrInstanceRunning
	}
	err = endExecs(ctx, task, fifoDir, grace)
	if err != nil {
		return fmt.Errorf("end the execs in container %s: %w", ctr.ID(), err)
	}
	if task == nil {
		return nil
	}
	if !running {
		_, err = task.Delete(ctx)
	} else {
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

// StopInstance ends the task of instance id and deletes it, and keeps
// the container and its snapshot. The execs a killed hull left in it are
// killed and deleted first, and their FIFOs removed; see endExecs. A task
// that has not stopped is sent SIGTERM, then SIGKILL if it has not exited
// within timeout, and StopInstance fails if it has not exited within grace
// after that. It reports whether the task was still running. An error that
// errdefs.IsNotFound accepts means no instance has that name in the
// client's namespace.
//
// A task that had already exited is deleted too. Its shim keeps running
// until the task is deleted, and urunc's holds about 18 MB for a VM that is
// gone.
func StopInstance(ctx context.Context, c *client.Client, id string, timeout, grace time.Duration) (bool, error) {
	ctr, err := LoadNamed(ctx, c, id)
	if err != nil {
		return false, err
	}
	task, status, err := loadTask(ctx, ctr)
	if err != nil {
		return false, err
	}
	running := task != nil && status.Status != client.Stopped
	err = endExecs(ctx, task, ExecFIFODir(c.DefaultNamespace(), ctr.ID()), grace)
	if err != nil {
		return running, fmt.Errorf("end the execs in container %s: %w", ctr.ID(), err)
	}
	if task == nil {
		return false, nil
	}
	if running {
		err = endTask(ctx, task, timeout, grace)
		if err != nil {
			return true, fmt.Errorf("task of container %s: %w", ctr.ID(), err)
		}
	}
	_, err = task.Delete(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return running, fmt.Errorf("delete task of container %s: %w", ctr.ID(), err)
	}
	return running, nil
}

func endTask(ctx context.Context, task client.Task, timeout, grace time.Duration) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	exited, err := task.Wait(waitCtx)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	done, err := signalAndWait(ctx, task, exited, syscall.SIGTERM, timeout)
	if done || err != nil {
		return err
	}
	// The SIGKILL goes to every process in the container, as rm --force's
	// does. Killing init ends the rest only in a container with a PID
	// namespace of its own, and one another client created may share the
	// host's. The SIGTERM goes to init alone, which can end the rest itself.
	done, err = signalAndWait(ctx, task, exited, syscall.SIGKILL, grace, client.WithKillAll)
	if done || err != nil {
		return err
	}
	return fmt.Errorf("still running %s after SIGKILL", grace)
}

// signalAndWait sends sig to task and reports whether it exited within
// wait. A task that exited before the signal arrived is not found by the
// kill, which is no error.
func signalAndWait(ctx context.Context, task client.Task, exited <-chan client.ExitStatus, sig syscall.Signal, wait time.Duration, opts ...client.KillOpts) (bool, error) {
	err := task.Kill(ctx, sig, opts...)
	if err != nil && !errdefs.IsNotFound(err) {
		return false, fmt.Errorf("send %v: %w", sig, err)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case status := <-exited:
		// The channel also delivers when ctx ends, with ctx's error, and
		// the task may then be running still.
		err = status.Error()
		// A task another stop or rm deleted after StopInstance read it
		// makes the wait fail with not found. Only an ended task can be
		// deleted, so this one has stopped, and the error must not reach
		// a caller that reads not found as no such container.
		if errdefs.IsNotFound(err) {
			return true, nil
		}
		return true, err
	case <-timer.C:
		return false, nil
	}
}
