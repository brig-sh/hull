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

package main

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"
)

const appUsage = "container CLI for hvi sandboxes on Linux/KVM"

// platformCommands is the Linux verb set: what it takes to run an hvi sandbox
// on KVM, and what brig calls to drive one (run, exec, ps, inspect, logs,
// stop, rm, assets dir, network-gateway), plus pause, resume, checkpoint and
// restore over hvi's control socket. checkpoint and restore are the same
// verbs as on macOS; there they also take Vz instances, here only hvi ones.
//
// Left out, because it drives something Linux does not have:
//   - store: detaches the APFS sparse image the macOS store is mounted from.
//
// compose is listed but refuses; see linuxCompose.
func platformCommands() []*cli.Command {
	return append([]*cli.Command{
		pullCommand(),
		runCommand(),
		execCommand(),
		psCommand(),
		stopCommand(),
		pauseCommand(),
		resumeCommand(),
		checkpointCommand(),
		restoreCommand(),
		rmCommand(),
		logsCommand(),
		inspectCommand(),
		imagesCommand(),
		rmiCommand(),
		pruneCommand(),
		assetsCommand(),
		linuxCompose(),
		networkGatewayCommand(),
		telemetryCommand(),
		versionCommand(),
	}, migrateRemoteCommands()...)
}

// errComposeMacOnly is what every compose subcommand answers on Linux.
var errComposeMacOnly = errors.New("hull compose runs on macOS only: its services default to vz " +
	"or qemu, and its health checks, restart policy and shared gateway have only been " +
	"exercised there. On Linux, run each sandbox with `hull run --hypervisor hvi`")

// linuxCompose is the compose command with a Before hook that refuses it.
//
// Refused rather than left out, for two reasons. Left out, `hull compose up`
// answers "No help topic for 'compose'", which reads as a broken install
// rather than a platform limit. And the compose code stays compiled on Linux
// either way -- the gateway hosts compose's service supervisor, so the two
// cannot be separated without splitting network-gateway -- and keeping its
// entry point reachable keeps it type-checked and linted like any other code
// on this platform, instead of rotting as a pile of unused functions.
func linuxCompose() *cli.Command {
	c := composeCommand()
	c.Hidden = true
	c.Before = func(ctx context.Context, _ *cli.Command) (context.Context, error) {
		return ctx, errComposeMacOnly
	}
	return c
}
