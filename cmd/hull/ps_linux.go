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

	"github.com/containerd/containerd/v2/client"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

func psCommand() *cli.Command {
	return &cli.Command{
		Name:  "ps",
		Usage: "list running instances",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return listInstances(ctx, cmd)
		},
	}
}

// listInstances prints the containers in hull's containerd namespace, in
// the same table as on macOS.
func listInstances(ctx context.Context, cmd *cli.Command) error {
	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	instances, err := containerd.Instances(ctx, c)
	if err != nil {
		return err
	}
	records := make([]*store.InstanceState, 0, len(instances))
	for _, inst := range instances {
		records = append(records, instanceRecord(inst))
	}
	return writeInstanceTable(cmd.Writer, records)
}

// instanceRecord turns a container into the record the macOS store keeps,
// so ps and inspect print the same fields with the same meaning. StartTime
// is when the container was created. The image digest, backend and log file
// come from the labels hull run sets, so a container another client created
// has none of them.
//
// containerd's client returns every timestamp in UTC, and macOS records them
// in local time. They are converted to local time here, or the ps table,
// which prints a time with no zone, would show a creation time hours off.
//
// A container with no task reads as stopped: there is no VMM to signal or
// wait for, whether it was never started or its task was deleted. A task
// that is created but not started reads as starting, the macOS word for an
// instance whose VMM exists and has not booted yet. Any other status
// containerd reports, such as paused, is printed under containerd's name.
//
// A container whose task could not be read reads as unreadable, as a macOS
// record that does not parse does. It must not read as stopped, since its
// task may still be running, and it carries no pid to signal.
func instanceRecord(inst containerd.Instance) *store.InstanceState {
	rec := &store.InstanceState{
		ID:          inst.ID,
		Status:      "stopped",
		StartTime:   inst.CreatedAt.Local(),
		ImageDigest: inst.Labels[containerd.LabelImageDigest],
		Backend:     inst.Labels[containerd.LabelBackend],
		LogFile:     inst.Labels[containerd.LabelLogFile],
	}
	if inst.Err != nil {
		rec.Status = store.StatusUnreadable
		return rec
	}
	if inst.Task == nil {
		return rec
	}
	switch inst.Task.Status {
	case client.Stopped:
		code := int(inst.Task.ExitStatus)
		rec.ExitCode = &code
		rec.ExitedAt = inst.Task.ExitTime.Local()
		return rec
	case client.Created:
		rec.Status = store.StatusStarting
	case client.Running:
		rec.Status = "running"
	default:
		rec.Status = string(inst.Task.Status)
	}
	rec.PID = int(inst.Task.Pid)
	return rec
}
