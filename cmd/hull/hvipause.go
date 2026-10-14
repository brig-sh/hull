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
	"fmt"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// isHviInstance reports whether the record's VMM is hvi, by the base name of
// the program it ran, as processIsAVMM matches it.
func isHviInstance(state *store.InstanceState) bool {
	return len(state.CmdLine) > 0 && filepath.Base(state.CmdLine[0]) == "hvi"
}

// requireRunningHvi refuses a verb that needs a live hvi instance with a
// control socket, saying which of the three it is not.
func requireRunningHvi(state *store.InstanceState, verb string) error {
	if !isHviInstance(state) {
		return fmt.Errorf("%s needs an hvi instance; %s runs under %s", verb, state.ID, backendName(state))
	}
	if state.Status != "running" || state.PID <= 0 || !processIsAVMM(state.PID, state) {
		return fmt.Errorf("instance %s is not running", state.ID)
	}
	if state.ControlSock == "" {
		return fmt.Errorf("instance %s was started without a control socket, so it can only be "+
			"stopped; run it again with this version of hull", state.ID)
	}
	return nil
}

// backendName names the VMM a record ran, for refusals.
func backendName(state *store.InstanceState) string {
	if state.Backend != "" {
		return state.Backend
	}
	if len(state.CmdLine) > 0 {
		return filepath.Base(state.CmdLine[0])
	}
	return "an unknown VMM"
}

func pauseCommand() *cli.Command {
	return &cli.Command{
		Name:      "pause",
		Usage:     "pause a running hvi instance: its I/O threads and every vCPU stop",
		ArgsUsage: "NAME",
		Action: func(_ context.Context, cmd *cli.Command) error {
			return hviPauseResume(cmd, "pause")
		},
	}
}

func resumeCommand() *cli.Command {
	return &cli.Command{
		Name:      "resume",
		Usage:     "resume an hvi instance paused by `hull pause`",
		ArgsUsage: "NAME",
		Action: func(_ context.Context, cmd *cli.Command) error {
			return hviPauseResume(cmd, "resume")
		},
	}
}

func hviPauseResume(cmd *cli.Command, op string) error {
	if cmd.Args().Len() == 0 {
		return errors.New("instance ID required")
	}
	id := cmd.Args().First()
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	state, err := s.GetInstance(id)
	if err != nil {
		return fmt.Errorf("instance not found: %s", id)
	}
	if err := requireRunningHvi(state, op); err != nil {
		return err
	}
	reply, err := hviOp(state.ControlSock, op)
	switch {
	case errors.Is(err, errHviNoReply) && op == "pause":
		// hvi keeps working on a pause the client gave up on, so the VM may
		// be paused now. Saying only "timed out" leaves a frozen sandbox
		// nobody knows to resume.
		return fmt.Errorf("%w; the VM may have paused after hull gave up, and `hull resume %s` "+
			"is harmless if it did not", err, id)
	case hviRefusalCode(err) == "fenced":
		return fmt.Errorf("instance %s was fenced by `hull checkpoint --then fence` and will not run "+
			"again in this process; stop it and `hull restore %s` from its checkpoint", id, id)
	case err != nil:
		return err
	}
	if op != "pause" {
		fmt.Printf("Instance %s resumed\n", id)
		return nil
	}
	// pause answers with the state alone; status has the vCPU counts. Best
	// effort: the pause has happened whether or not this second ask works.
	if st, err := hviOp(state.ControlSock, "status"); err == nil && st.Vcpus > 0 {
		fmt.Printf("Instance %s %s (%d of %d vCPUs parked)\n", id, st.State, st.VcpusParked, st.Vcpus)
		return nil
	}
	fmt.Printf("Instance %s %s\n", id, reply.State)
	return nil
}
