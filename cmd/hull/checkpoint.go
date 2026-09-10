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

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	ckptError     = "error"
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
	if !processIsAVMM(state.PID, state) {
		return errors.New("checkpoint requires the Vz backend (vz-runner is not the instance's VMM)")
	}
	if !slices.Contains(state.CmdLine, "--state-dir") {
		return fmt.Errorf("instance %s was started without a checkpoint state dir; re-run it with this version of hull", instanceID)
	}
	if err := requireBlockRootfs(state.CmdLine); err != nil {
		return err
	}

	ckptDir := instanceCheckpointDir(s, instanceID)
	started := time.Now()

	// vz-runner pauses, saves, clones the disk, resumes, and writes the
	// manifest last — its fresh mtime is the completion signal.
	if err := syscall.Kill(state.PID, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("failed to signal vz-runner (pid %d): %w", state.PID, err)
	}

	timeout := time.Duration(cmd.Int("timeout")) * time.Second
	return waitForCheckpoint(ckptDir, instanceID, state.PID, started, timeout)
}

// waitForCheckpoint watches ckptDir until the checkpoint that began at started
// finishes, until the runner records why it could not take one, until the
// runner exits, or until timeout runs out.
func waitForCheckpoint(ckptDir, instanceID string, pid int, started time.Time, timeout time.Duration) error {
	deadline := started.Add(timeout)
	for time.Now().Before(deadline) {
		done, err := checkpointCompleted(ckptDir, started)
		if err != nil {
			// Not "checkpoint failed", which is the runner's own reason a few
			// lines below. This one is hull refusing to call an incomplete
			// artifact set a checkpoint, and the two read differently to
			// whoever has to act on them.
			return fmt.Errorf("checkpoint incomplete: %w", err)
		}
		if done {
			stateSize := int64(0)
			if st, err := os.Stat(filepath.Join(ckptDir, ckptStateFile)); err == nil {
				stateSize = st.Size()
			}
			// The clone's mtime is no use as a freshness test: `cp -c` on APFS
			// carries the source file's timestamps over, so a clone taken now
			// from a disk the guest last wrote to an hour ago is an hour old.
			// The manifest naming the clone is what says it belongs to this
			// checkpoint, and checkpointCompleted has just established that.
			diskNote := ""
			if st, err := os.Stat(filepath.Join(ckptDir, ckptDiskFile)); err == nil {
				diskNote = fmt.Sprintf(", disk clone %.1f MB", float64(st.Size())/(1024*1024))
			}
			fmt.Printf("Checkpoint of %s saved in %s (%s: %.1f MB%s)\n",
				instanceID, time.Since(started).Round(time.Millisecond),
				ckptStateFile, float64(stateSize)/(1024*1024), diskNote)
			return nil
		}
		// A checkpoint the runner has already given up on. It writes the
		// reason beside the state file, so report that instead of waiting out
		// a timeout for a manifest that is not coming: a save refused in
		// 200ms used to surface a minute later as "did not complete", which
		// says nothing and reads like the machine is slow.
		if reason, ok := freshCheckpointFailure(ckptDir, started); ok {
			return fmt.Errorf("checkpoint failed: %s", reason)
		}
		// The runner stays alive whether or not it managed the checkpoint, so
		// its exit is a separate failure. Where its reason lands depends on
		// how the instance was started, and saying "the instance log"
		// unconditionally sent people looking for a file that does not exist:
		// launchVMM only opens the log in the detached branch, and a
		// foreground run gives the VMM the terminal instead.
		if err := syscall.Kill(pid, 0); err != nil {
			return errors.New("vz-runner exited while checkpointing; the reason is on the guest console (`hull logs` for a detached run, the terminal for a foreground one)")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("checkpoint did not complete within %ds; the reason, if the runner gave one, is on the guest console (`hull logs` for a detached run, the terminal for a foreground one)", int(timeout.Seconds()))
}

// checkpointManifest is latest.json, the file vz-runner writes last
// (vz-runner/Sources/main.swift:1059-1067). It names the artifacts the
// checkpoint is made of. diskImage is the empty string when the disk clone
// failed. Fields the CLI does not act on (savedAt, durationMs) are ignored.
type checkpointManifest struct {
	StateFile string `json:"stateFile"`
	DiskImage string `json:"diskImage"`
}

// checkpointCompleted reports whether ckptDir holds a finished checkpoint from
// the attempt that began at started.
//
// docs/checkpoint-restore.md:49 defines the marker: "latest.json | manifest,
// written last — its mtime marks checkpoint completion". A fresh manifest
// therefore says the runner is done, but not that it succeeded: it publishes
// one whenever the machine state was saved, whether or not the disk clone
// worked (main.swift:1057 gates the write on saveError == nil alone). So
// completion is the manifest *and* the artifacts it names.
//
// The three answers are distinct:
//   - (false, nil): nothing from this attempt yet, keep waiting.
//   - (false, err): the attempt finished and did not produce a whole
//     checkpoint. The manifest is written last, so nothing more is coming and
//     waiting out the timeout would only delay the same bad news.
//   - (true, nil): every named artifact is on disk.
func checkpointCompleted(ckptDir string, started time.Time) (bool, error) {
	info, err := os.Stat(filepath.Join(ckptDir, ckptManifest))
	if err != nil || info.ModTime().Before(started) {
		return false, nil
	}
	m, err := readCheckpointManifest(ckptDir)
	if err != nil {
		// The runner writes the manifest non-atomically (main.swift:1066,
		// Data.write with no .atomic option), so a manifest that does not
		// parse may be one being written right now. Keep waiting; the timeout
		// is the backstop for one that never becomes readable.
		return false, nil
	}
	if err := verifyCheckpointArtifacts(ckptDir, m); err != nil {
		return false, err
	}
	return true, nil
}

// readCheckpointManifest reads and parses latest.json out of ckptDir.
func readCheckpointManifest(ckptDir string) (checkpointManifest, error) {
	var m checkpointManifest
	data, err := os.ReadFile(filepath.Join(ckptDir, ckptManifest))
	if err != nil {
		return m, fmt.Errorf("no %s in %s, so no checkpoint finished there", ckptManifest, ckptDir)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s in %s does not read as a manifest: %w", ckptManifest, ckptDir, err)
	}
	return m, nil
}

// verifyCheckpointArtifacts checks that every artifact the manifest names is
// on disk with contents in it.
//
// A manifest naming no disk image is the runner reporting a failed clone:
// diskCloned drives that field (main.swift:1061) and stays false when `cp -c`
// exits nonzero (main.swift:1052-1055). The runner deletes the previous clone
// before it makes the new one (main.swift:1049), so this is not a checkpoint
// that fell back on an older disk. It is a machine state with no rootfs to go
// with it, and the pair has to rewind together for the guest to survive
// (docs/checkpoint-restore.md:51-53).
func verifyCheckpointArtifacts(ckptDir string, m checkpointManifest) error {
	if err := checkpointArtifact(ckptDir, "machine state", m.StateFile); err != nil {
		return err
	}
	if m.DiskImage == "" {
		return errors.New("the machine state was saved but the disk was not cloned, so there is no rootfs to rewind with it")
	}
	return checkpointArtifact(ckptDir, "disk clone", m.DiskImage)
}

// checkpointArtifact reports whether one artifact the manifest names is there
// to be used. The name has to be a file in the checkpoint directory itself: a
// name that walks out of it describes something other than this checkpoint.
func checkpointArtifact(ckptDir, role, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s: %s names no file for it", role, ckptManifest)
	case name == "." || name == ".." || filepath.Base(name) != name:
		return fmt.Errorf("%s: %s names %q, which is not a file in %s", role, ckptManifest, name, ckptDir)
	}
	info, err := os.Stat(filepath.Join(ckptDir, name))
	if err != nil {
		return fmt.Errorf("%s: %s names %s but it is not in %s", role, ckptManifest, name, ckptDir)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s: %s is empty", role, name)
	}
	return nil
}

// requireRestorableCheckpoint refuses a checkpoint that is not all there.
//
// The runner restores the machine state onto whatever rootfs it finds: with no
// clone in the checkpoint it prints "no disk image in checkpoint, keeping
// current rootfs" and carries on (main.swift:1091-1094). The guest then
// resumes holding memory from the checkpoint moment against a disk that has
// moved on since — inodes, journal and free lists all disagree with what the
// guest believes. Reading the manifest here is what makes the difference
// visible: vm.vzstate on its own says a state was saved, not that a whole
// checkpoint was.
func requireRestorableCheckpoint(ckptDir string) error {
	m, err := readCheckpointManifest(ckptDir)
	if err != nil {
		return err
	}
	return verifyCheckpointArtifacts(ckptDir, m)
}

// freshCheckpointFailure reads the reason vz-runner records when it cannot
// take a checkpoint.
//
// Only a marker written after this attempt began counts. The checkpoint
// directory outlives a single attempt, so an older file describes an older
// one, and reporting that would turn a slow checkpoint into a phantom
// failure. This is the same mtime rule the manifest is read under.
func freshCheckpointFailure(ckptDir string, started time.Time) (string, bool) {
	info, err := os.Stat(filepath.Join(ckptDir, ckptError))
	if err != nil || info.ModTime().Before(started) {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(ckptDir, ckptError))
	if err != nil {
		return "", false
	}
	reason := strings.TrimSpace(string(data))
	if reason == "" {
		return "", false
	}
	return reason, true
}

func restoreCommand() *cli.Command {
	return &cli.Command{
		Name:  "restore",
		Usage: "restore a stopped Vz instance from its checkpoint",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "run in background",
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
	if state.Status == "running" && state.PID > 0 && processIsAVMM(state.PID, state) {
		return fmt.Errorf("instance %s is still running; stop it first (the checkpoint is kept)", instanceID)
	}
	// The check above needs a pid, and "starting" is the state that has none.
	// It is not the same as stopped: there may be a live VMM holding this
	// instance's block rootfs, and restoring on top of it would put a second
	// one on the same disk. Reap it first rather than refusing, so a restore is
	// still possible after a run died in that window.
	if state.Status == store.StatusStarting && state.PID <= 0 {
		if reapOrphanVMM(s, state) {
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
	if err := requireNoDescriptorShare(state.CmdLine); err != nil {
		return err
	}
	// After requireBlockRootfs, because that is what makes a manifest naming
	// no disk image unambiguous: the runner only skips the clone when the
	// instance has no block rootfs, and this instance has one.
	if err := requireRestorableCheckpoint(ckptDir); err != nil {
		return fmt.Errorf("the checkpoint for %s cannot be restored: %w", instanceID, err)
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
// requireNoDescriptorShare rejects restoring an instance whose share was named
// by a descriptor the caller held open.
//
// Restore replays the recorded command line, and for such a share that line
// carries an identity path (/.vol/<device>/<inode>). The descriptor that made
// the identity trustworthy died with the process that was given it, so what
// the path names is no longer pinned: the directory can be gone, and on a
// volume that reuses inode numbers the path can name a different one. Handing
// the guest a directory nobody vouched for is the substitution --shared-dir-fd
// exists to prevent, so restore says so and the caller starts the instance
// again with a fresh descriptor. The gateway socket, another inherited
// descriptor, is refused the same way a few lines below.
func requireNoDescriptorShare(cmdLine []string) error {
	for _, arg := range cmdLine {
		if strings.HasPrefix(arg, "/.vol/") {
			return errors.New("this instance shared a directory by descriptor (--shared-dir-fd), which does not " +
				"survive the process it was passed to: run it again with a fresh descriptor rather than restoring it")
		}
	}
	return nil
}

func requireBlockRootfs(cmdLine []string) error {
	if !slices.Contains(cmdLine, "--rootfs") {
		return errors.New("checkpoint/restore needs a block rootfs: start the instance with `run --rootfs-type block` (a virtiofs root goes stale across restore)")
	}
	return nil
}

// vzRunnerProcess reports whether pid's argv looks like vz-runner, guarding
// checkpoint/restore against pid reuse and non-Vz backends.
