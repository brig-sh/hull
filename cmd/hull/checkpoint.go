// Copyright (c) 2023-2026, Nubificus LTD
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

//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// Checkpoint artifact names inside <instance>/checkpoint/ — must match
// vz-runner's layout (vz-runner/Sources/main.swift).
const (
	ckptStateFile = "vm.vzstate"
	ckptDiskFile  = "rootfs.img"
	ckptManifest  = "latest.json"
)

func instanceCheckpointDir(s *store.Store, id string) string {
	return filepath.Join(s.InstanceDir(id), "checkpoint")
}

func checkpointCommand() *cli.Command {
	return &cli.Command{
		Name:  "checkpoint",
		Usage: "checkpoint a running Vz instance (pause, save VM + disk state, resume)",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "timeout",
				Value: 60,
				Usage: "seconds to wait for the checkpoint to complete",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return checkpointInstance(ctx, cmd)
		},
	}
}

func checkpointInstance(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}
	if state.Status != "running" || state.PID <= 0 {
		return fmt.Errorf("instance %s is not running", instanceID)
	}
	if !vzRunnerProcess(state.PID) {
		return errors.New("checkpoint requires the Vz backend (vz-runner is not the instance's VMM)")
	}
	if !slices.Contains(state.CmdLine, "--state-dir") {
		return fmt.Errorf("instance %s was started without a checkpoint state dir; re-run it with this version of hull", instanceID)
	}
	if err := requireBlockRootfs(state.CmdLine); err != nil {
		return err
	}

	ckptDir := instanceCheckpointDir(s, instanceID)
	manifest := filepath.Join(ckptDir, ckptManifest)
	started := time.Now()

	// vz-runner pauses, saves, clones the disk, resumes, and writes the
	// manifest last — its fresh mtime is the completion signal.
	if err := syscall.Kill(state.PID, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("failed to signal vz-runner (pid %d): %w", state.PID, err)
	}

	deadline := started.Add(time.Duration(cmd.Int("timeout")) * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(manifest); err == nil && !info.ModTime().Before(started) {
			stateSize := int64(0)
			if st, err := os.Stat(filepath.Join(ckptDir, ckptStateFile)); err == nil {
				stateSize = st.Size()
			}
			diskNote := ""
			if st, err := os.Stat(filepath.Join(ckptDir, ckptDiskFile)); err == nil && !st.ModTime().Before(started) {
				diskNote = fmt.Sprintf(", disk clone %.1f MB", float64(st.Size())/(1024*1024))
			}
			fmt.Printf("Checkpoint of %s saved in %s (%s: %.1f MB%s)\n",
				instanceID, time.Since(started).Round(time.Millisecond),
				ckptStateFile, float64(stateSize)/(1024*1024), diskNote)
			return nil
		}
		// The runner logs the reason when a checkpoint is refused or fails;
		// it stays alive either way, so all we can do here is time out.
		if err := syscall.Kill(state.PID, 0); err != nil {
			return errors.New("vz-runner exited while checkpointing; see the instance log")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("checkpoint did not complete within %ds; see the instance log", cmd.Int("timeout"))
}

func restoreCommand() *cli.Command {
	return &cli.Command{
		Name:  "restore",
		Usage: "restore a stopped Vz instance from its checkpoint",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "detach,d",
				Usage: "run in background",
			},
			&cli.IntFlag{
				Name:  "stop-grace",
				Value: 10,
				Usage: "seconds vz-runner waits for the guest to answer a stop request before forcing",
			},
			&cli.BoolFlag{
				Name:  "wait-ip",
				Usage: "with --detach and NAT networking, wait for the DHCP lease and record the IP before returning",
			},
			&cli.StringFlag{
				Name:  "gateway-sock",
				Usage: "re-join the user-mode network gateway at this control socket (required if the instance ran on the gateway)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return restoreInstance(ctx, cmd)
		},
	}
}

func restoreInstance(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}
	instanceID := args.First()
	detach := cmd.Bool("detach")
	gatewaySock := cmd.String("gateway-sock")

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}
	if state.Status == "running" && state.PID > 0 && vzRunnerProcess(state.PID) {
		return fmt.Errorf("instance %s is still running; stop it first (the checkpoint is kept)", instanceID)
	}
	// The check above needs a pid, and "starting" is the state that has none.
	// It is not the same as stopped: there may be a live VMM holding this
	// instance's block rootfs, and restoring on top of it would put a second
	// one on the same disk. Reap it first rather than refusing, so a restore is
	// still possible after a run died in that window.
	if state.Status == store.StatusStarting && state.PID <= 0 {
		if reapOrphanVMM(state) {
			log.Warnf("instance %s had a VMM running that was never recorded; killed it "+
				"before restoring", instanceID)
		}
	}
	ckptDir := instanceCheckpointDir(s, instanceID)
	if _, err := os.Stat(filepath.Join(ckptDir, ckptStateFile)); err != nil {
		return fmt.Errorf("no checkpoint found for %s (run `hull checkpoint %s` while it is running)", instanceID, instanceID)
	}
	if len(state.CmdLine) == 0 || !strings.Contains(state.CmdLine[0], "vz-runner") {
		return errors.New("restore requires the Vz backend")
	}
	if err := requireBlockRootfs(state.CmdLine); err != nil {
		return err
	}

	cmdArgs := slices.Clone(state.CmdLine)
	if !slices.Contains(cmdArgs, "--restore") {
		cmdArgs = append(cmdArgs, "--restore")
	}

	// The gateway datagram socket is an inherited fd; it does not survive the
	// original process, so gateway instances must re-join.
	var gatewayFiles []*os.File
	if slices.Contains(cmdArgs, "--net-fd") {
		if gatewaySock == "" {
			return errors.New("this instance ran on the network gateway; pass --gateway-sock to re-join it")
		}
		dataF, ctlConn, err := joinGateway(gatewaySock)
		if err != nil {
			return err
		}
		defer func() { _ = dataF.Close() }()
		ctlF, err := ctlConn.File()
		if err != nil {
			return fmt.Errorf("failed to dup gateway control connection: %w", err)
		}
		_ = ctlConn.Close()
		defer func() { _ = ctlF.Close() }()
		gatewayFiles = []*os.File{dataF, ctlF}
	}

	// The guest resumes with its previous network state in memory, so the
	// recorded IP stays valid (same MAC, same lease/static address) — skip
	// rediscovery by reporting no NAT networking to the launcher.
	_, err = launchVMM(cmd, s, state, cmdArgs, gatewayFiles, hypervisors.VzVmm, detach, "none", "")
	return err
}

// requireBlockRootfs rejects checkpointing for instances whose rootfs is a
// virtiofs directory share: Virtualization.framework does not rehydrate the
// guest's FUSE state in a new VMM process, so every inode goes stale at
// restore and the guest effectively dies. A block rootfs (--rootfs-type
// block) is cloned at checkpoint and restored consistently.
func requireBlockRootfs(cmdLine []string) error {
	if !slices.Contains(cmdLine, "--rootfs") {
		return errors.New("checkpoint/restore needs a block rootfs: start the instance with `run --rootfs-type block` (a virtiofs root goes stale across restore)")
	}
	return nil
}

// vzRunnerProcess reports whether pid's argv looks like vz-runner, guarding
// checkpoint/restore against pid reuse and non-Vz backends.
func vzRunnerProcess(pid int) bool {
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "vz-runner")
}
