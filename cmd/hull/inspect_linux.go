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
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

func inspectCommand() *cli.Command {
	return &cli.Command{
		Name:  "inspect",
		Usage: "inspect instance details",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return inspectInstance(ctx, cmd)
		},
	}
}

// inspectInstance prints the record instanceRecord builds for one
// container, in the JSON form macOS prints. The fields containerd has no
// counterpart for, such as logFile, are empty. cmdLine is the monitor's
// command line, which hull run records when it sees the monitor start.
func inspectInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()
	err := store.ValidateInstanceID(instanceID)
	if err != nil {
		return instanceReadError(instanceID, err)
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	inst, err := containerd.LoadInstance(ctx, c, instanceID)
	if errdefs.IsNotFound(err) {
		// instanceReadError reports only this error as "instance not
		// found", the words brig matches to decide an instance is gone.
		return instanceReadError(instanceID, store.ErrInstanceNotFound)
	}
	if err != nil {
		// LoadInstance's errors already name the instance or its
		// container; instanceReadError would put the name in front again.
		return err
	}
	rec := instanceRecord(inst)
	namespace := containerd.ResolveNamespace(cmd.String("namespace"))
	withNetworkAddress(rec, namespace)
	// The path hull run computes for this name, not one a label names, as
	// rm finds the log.
	logFile, err := instanceLogPath(namespace, instanceID)
	if err == nil {
		rec.CmdLine, err = readCmdLine(cmdLineFileOf(logFile))
	}
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrWriter, "warning: cannot read the command line of %s: %v\n", instanceID, err)
	}
	if rec.MAC == "" && rec.Network == gatewayMode {
		rec.MAC = flagValue(rec.CmdLine, "--net-mac")
	}
	return printJSON(cmd.Writer, rec)
}

// flagValue returns the value args give flag, as --flag value or
// --flag=value, and empty when they do not give it. urunc picks the MAC of a
// guest on a gateway and gives it to hvi as --net-mac, so the monitor's
// recorded arguments are where hull learns it.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		v, ok := strings.CutPrefix(a, flag+"=")
		if ok {
			return v
		}
	}
	return ""
}
