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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/cni"
	"github.com/brig-sh/hull/internal/containerd"
)

// pinNetns pins a network namespace for the detach, and netnsPinned
// reports whether one is pinned. Variables, so tests run by a user who
// cannot mount can stand in for them.
var (
	pinNetns    = cni.PinNetns
	netnsPinned = cni.Pinned
)

// hookCacheDir is where the hook has libcni keep its results: "" for
// libcni's own. A variable, so tests run by a user who cannot write
// /var/lib/cni can point it elsewhere.
var hookCacheDir = ""

// internalCommand holds the commands hull runs itself, and no user should.
func internalCommand() *cli.Command {
	return &cli.Command{
		Name:     "internal",
		Usage:    "commands hull runs itself",
		Hidden:   true,
		Commands: []*cli.Command{ociHookCommand()},
	}
}

// ociHookCommand is the OCI hook runNetwork puts in an attached instance's
// spec. The runtime runs it with the container's state on stdin: at
// createRuntime, to attach the instance's network namespace to the
// network, and at postStop, to detach it. hull rm runs it as detach, for a
// postStop the runtime never ran; see detachLeft.
func ociHookCommand() *cli.Command {
	return &cli.Command{
		Name:      "oci-hook",
		Usage:     "attach an instance to its network (createRuntime) or detach it (postStop)",
		ArgsUsage: "createRuntime|postStop|detach",
		// A path may hold a comma.
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "namespace", Usage: "containerd namespace of the instance"},
			&cli.StringFlag{Name: "network", Usage: "CNI network name"},
			&cli.StringSliceFlag{Name: "cni-path", Usage: "CNI plugin directory (repeatable)"},
			&cli.StringFlag{Name: "cni-netconfpath", Usage: "CNI network configuration directory"},
			&cli.StringFlag{Name: "dir", Usage: "the instance's network directory"},
			&cli.StringFlag{Name: "rootlesskit-state-dir", Usage: "state directory of a rootless daemon's rootlesskit"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			err := runOCIHook(ctx, cmd)
			if err == nil || cmd.Args().First() == "detach" {
				// hull rm, which runs detach, reads the error from the
				// hook's own output and exit status.
				return err
			}
			// For hull run, stop and rm to report: the runtime's own error
			// does not carry a createRuntime hook's to hull run, and urunc
			// drops the output of a hook that succeeds.
			_ = os.WriteFile(filepath.Join(cmd.String("dir"), containerd.HookErrorFile), []byte(err.Error()+"\n"), 0o644)
			if cmd.Args().First() == "postStop" {
				// The OCI runtime spec has a runtime only warn about a
				// failed postStop hook. urunc 0b1ef07 fails its delete
				// instead, after removing its state, so every later delete
				// fails too and the instance can never be removed. So a
				// failed detach succeeds, and is left for hull to report.
				return nil
			}
			return err
		},
	}
}

func runOCIHook(ctx context.Context, cmd *cli.Command) error {
	// Before stdin is read: the process run again in the namespace reads
	// the state from it.
	err := cni.EnterDetachedNetns(cmd.String("rootlesskit-state-dir"))
	if err != nil {
		return err
	}
	var state specs.State
	err = json.NewDecoder(cmd.Root().Reader).Decode(&state)
	if err != nil {
		return fmt.Errorf("read the container state: %w", err)
	}
	cfg := cni.Config{
		PluginDirs: cmd.StringSlice("cni-path"),
		ConfDir:    cmd.String("cni-netconfpath"),
		Network:    cmd.String("network"),
		CacheDir:   hookCacheDir,
	}
	id := cniID(cmd.String("namespace"), state.ID)
	result := filepath.Join(cmd.String("dir"), networkResultFile)
	pin := filepath.Join(cmd.String("dir"), containerd.NetnsPinFile)

	event := cmd.Args().First()
	switch event {
	case "createRuntime":
		if state.Pid <= 0 {
			return errors.New("the container state names no process, so no network namespace to attach")
		}
		err := pinNetns(fmt.Sprintf("/proc/%d/ns/net", state.Pid), pin)
		if err == nil {
			var res cni.Result
			res, err = cfg.Attach(ctx, id, pin)
			if err == nil {
				err = writeResult(result, res)
				if err != nil {
					// Detach while the namespace is pinned: the pin goes
					// below, and a postStop without it leaves the bridge
					// plugin's masquerade rule behind.
					err = errors.Join(err, cfg.Detach(ctx, id, pin))
				}
			}
			if err != nil {
				err = errors.Join(err, cni.UnpinNetns(pin))
			}
		}
		return err
	case "postStop", "detach":
		netns := ""
		if netnsPinned(pin) {
			netns = pin
		}
		err := cfg.Detach(ctx, id, netns)
		if err != nil && event == "detach" {
			// hull rm fails and keeps the instance, so the next hull rm
			// runs the detach again, from the pin while it holds the
			// namespace.
			return err
		}
		// At postStop the pin and the address go whether or not the detach
		// worked: nothing runs the detach again, a pin would keep the
		// namespace forever, and hull ps would show the address on an
		// instance that no longer runs, which may not even hold it any
		// more.
		err = errors.Join(err, cni.UnpinNetns(pin))
		rerr := os.Remove(result)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return err
	default:
		return fmt.Errorf("unknown hook %q, expected createRuntime, postStop or detach", event)
	}
}

// cniID returns the container ID the hook attaches the container id of the
// containerd namespace namespace under. A containerd identifier starts and
// ends with a letter or digit and never holds two of '.', '_' and '-' in a
// row, so the "--" between the two can be nowhere else: no two containers
// share an ID, as namespace a-b's container c and namespace a's container
// b-c would with one '-'. nerdctl's IDs, <namespace>-<64 hex digits>, never
// hold "--", so none of them is a hull instance's either.
func cniID(namespace, id string) string {
	return namespace + "--" + id
}

// writeResult writes res to path whole, so a hull ps that reads it while
// the hook writes finds the old file or the new one, never part of one.
func writeResult(path string, res cni.Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	err = os.WriteFile(tmp, data, 0o644)
	if err != nil {
		return fmt.Errorf("record the instance's address: %w", err)
	}
	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("record the instance's address: %w", err)
	}
	return nil
}
