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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"

	"github.com/brig-sh/hull/pkg/store"
)

func restoreCommand() *cli.Command {
	return &cli.Command{
		Name: "restore",
		Usage: "restore a stopped instance from its checkpoint: Vz from its saved state, " +
			"hvi from the bundle of a fenced checkpoint",
		ArgsUsage: "NAME",
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
				Name: "gateway-sock",
				Usage: "Vz: re-join the user-mode network gateway at this control socket (required if " +
					"the instance ran on the gateway); hvi re-joins the gateway it ran on by itself",
			},
			&cli.BoolFlag{
				Name:  "paused",
				Usage: "hvi: leave the restored guest paused; `hull resume` starts it",
			},
			&cli.IntFlag{
				Name:  "timeout",
				Value: 120,
				Usage: "hvi: seconds to wait for hvi to rebuild the VM from the bundle",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return errors.New("instance ID required")
			}
			s, err := globalStore(cmd)
			if err != nil {
				return err
			}
			id := cmd.Args().First()
			state, err := s.GetInstance(id)
			if err != nil {
				return fmt.Errorf("instance not found: %s", id)
			}
			if !isHviInstance(state) {
				if cmd.IsSet("paused") || cmd.IsSet("timeout") {
					return errors.New("--paused and --timeout apply to hvi instances only")
				}
				return vzRestoreInstance(ctx, cmd)
			}
			if cmd.IsSet("gateway-sock") {
				return errors.New("an hvi instance re-joins the gateway socket it ran with " +
					"(--net-gateway on its command line); --gateway-sock is for Vz instances")
			}
			return hviRestoreInstance(cmd, s, state, hviRestoreOptions{
				detach:  cmd.Bool("detach"),
				paused:  cmd.Bool("paused"),
				timeout: time.Duration(cmd.Int("timeout")) * time.Second,
			})
		},
	}
}

type hviRestoreOptions struct {
	detach  bool
	paused  bool
	timeout time.Duration
}

// hviRestoreArgv turns the command line an hvi instance last ran with into
// the `hvi restore` command line that brings it back from the bundle in from.
//
// The bundle holds the guest's RAM, so it already has the kernel, initramfs
// and command line in it, and its profile fixes the memory size and vCPU
// count: those flags are dropped. What the bundle cannot hold are the host
// resources the devices were attached to, and those are kept: the disk, the
// gateway socket (so a restore on the same host re-joins the same gateway),
// the MAC the gateway's leases are keyed by, the agent socket, the sandbox id
// and the event ledger. A bare --net is the deprecated spelling of
// --net-stub and is passed under its new name, since restore takes only that.
//
// An allowlist, not a list of flags to remove: a flag this function has never
// heard of could carry a host resource restore would silently go without, so
// it is refused by name instead. The control socket and profile paths are the
// caller's, placed last, which is what makes the mapping idempotent: the
// command line a restore records is a valid input to the next one.
func hviRestoreArgv(cmdLine []string, from, controlSock, profilePath string) ([]string, error) {
	if len(cmdLine) < 2 {
		return nil, errors.New("the recorded hvi command line is empty")
	}
	if sub := cmdLine[1]; sub != "boot" && sub != "restore" {
		return nil, fmt.Errorf("the recorded hvi command line runs %q, not boot or restore", sub)
	}
	keep := map[string]bool{
		"--disk": true, "--net-gateway": true, "--net-mac": true,
		"--agent-sock": true, "--sandbox-id": true, "--events": true,
	}
	drop := map[string]bool{
		"--kernel": true, "--initramfs": true, "--cmdline": true, "--mem-mib": true,
		"--cpus": true, "--fs-uid": true, "--fs-gid": true, "--from": true,
		"--control-sock": true, "--profile-out": true,
	}
	out := []string{cmdLine[0], "restore", "--from", from}
	sawDisk := false
	for i := 2; i < len(cmdLine); i++ {
		flag := cmdLine[i]
		switch {
		case flag == "--net" || flag == "--net-stub":
			out = append(out, "--net-stub")
		case keep[flag] || drop[flag]:
			if i+1 >= len(cmdLine) {
				return nil, fmt.Errorf("the recorded hvi command line ends at %s, which takes a value", flag)
			}
			if keep[flag] {
				out = append(out, flag, cmdLine[i+1])
				sawDisk = sawDisk || flag == "--disk"
			}
			i++
		case flag == "--share-ro" || flag == "--share-rw":
			return nil, errors.New("the instance shared a host directory, which no checkpoint carries")
		default:
			return nil, fmt.Errorf("the recorded hvi command line has %s, which hull does not know "+
				"how to carry into a restore", flag)
		}
	}
	if !sawDisk {
		return nil, errors.New("the recorded hvi command line has no --disk; a restore needs the " +
			"block rootfs the checkpoint was taken with")
	}
	return append(out, "--control-sock", controlSock, "--profile-out", profilePath), nil
}

// flagValue returns the value after the first occurrence of flag in argv.
func flagValue(argv []string, flag string) (string, bool) {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1], true
		}
	}
	return "", false
}

// hviHasRestore reports whether the hvi at binary has a restore subcommand.
//
// Asked of the binary the instance ran, not of PATH: that is the hvi whose
// bundle format the checkpoint is in, and the one restore will run. hvi
// answers an unknown subcommand with a fixed sentence (src/main.rs), so its
// absence is told apart from any other failure, such as restore complaining
// that it was given no --from, which means it is there.
func hviHasRestore(binary string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	probe := exec.CommandContext(ctx, binary, "restore")
	probe.Stdout = &out
	probe.Stderr = &out
	err := probe.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("%s restore did not answer within 5s", binary)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return false, fmt.Errorf("run %s: %w", binary, err)
	}
	return !strings.Contains(out.String(), `unknown subcommand "restore"`), nil
}

// restorableMigration reports whether the record's last checkpoint can be
// restored onto the instance's disk.
//
// The disk is not in the bundle; the restore reopens the live one. That is
// only consistent if the guest never wrote to it after the capture:
//   - fenced: the source never ran again. The case a restore is for.
//   - restoring or restored-paused: a restore that did not get as far as
//     running the guest, so the disk is still the one the bundle saw.
//
// checkpointed (then continue) means the guest kept running and writing, and
// restored means a restored guest ran: the disk has moved on from the RAM in
// the bundle, and a guest given the two together sees a filesystem that
// disagrees with its own caches. That is refused, as Vz restore refuses a
// checkpoint whose disk clone is missing.
func restorableMigration(m *store.MigrationState) error {
	if m == nil || m.BundleDir == "" {
		return errors.New("it has no hvi checkpoint; take one with `hull checkpoint --then fence`")
	}
	switch m.State {
	case store.MigrationFenced, store.MigrationRestoring, store.MigrationRestoredPaused:
		return nil
	case store.MigrationCheckpointed:
		return errors.New("its checkpoint was taken with --then continue, and the guest has " +
			"written to its disk since, which the bundle does not hold. Take one with " +
			"`hull checkpoint --then fence` to restore from")
	case store.MigrationRestored:
		return errors.New("its checkpoint was already restored and the restored guest ran, so " +
			"its disk has moved on from the bundle")
	}
	return fmt.Errorf("its checkpoint is in an unknown state %q", m.State)
}

func hviRestoreInstance(cmd *cli.Command, s *store.Store, state *store.InstanceState, opt hviRestoreOptions) error {
	id := state.ID
	if state.Status == "running" && state.PID > 0 && processIsAVMM(state.PID, state) {
		return fmt.Errorf("instance %s is still running; stop it first (the checkpoint is kept)", id)
	}
	// As for Vz: "starting" with no pid may hide a live VMM holding the disk,
	// and a second one on the same disk is the corruption restore must not
	// cause.
	if state.Status == store.StatusStarting && state.PID <= 0 {
		if reapOrphanVMM(s, state) {
			log.Warnf("instance %s had a VMM running that was never recorded; killed it "+
				"before restoring", id)
		}
	}
	if err := restorableMigration(state.Migration); err != nil {
		return fmt.Errorf("cannot restore %s: %w", id, err)
	}
	bundle := state.Migration.BundleDir
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("cannot restore %s: its bundle %s is gone", id, bundle)
	}
	// checkpoint writes latest.json before the record, so a failure between
	// the two leaves latest.json naming a newer checkpoint than the record
	// does. Restoring the record's would rewind past a checkpoint whose fate
	// (continued or fenced) the record never learned; refuse and say which.
	if latest, err := readHviCheckpointRecord(instanceCheckpointDir(s, id)); err == nil &&
		latest.TxID != state.Migration.TxID {
		return fmt.Errorf("cannot restore %s: its record names checkpoint %s in %s, but latest.json "+
			"names %s in %s, a later one the record never took in. Take a new checkpoint with "+
			"--then fence", id, state.Migration.TxID, bundle, latest.TxID, latest.Dir)
	}
	if err := hviCheckpointable(state.CmdLine); err != nil {
		return err
	}
	has, err := hviHasRestore(state.CmdLine[0])
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("cannot restore %s: the hvi it ran (%s) has no restore subcommand yet. "+
			"The bundle in %s is kept; restore it with an hvi that has one", id, state.CmdLine[0], bundle)
	}

	controlSock := s.InstanceControlSocket(id)
	profilePath := s.InstanceProfilePath(id)
	if err := checkUnixSocketPath("control", controlSock); err != nil {
		return err
	}
	argv, err := hviRestoreArgv(state.CmdLine, bundle, controlSock, profilePath)
	if err != nil {
		return fmt.Errorf("cannot restore %s: %w", id, err)
	}
	// hvi relays the guest's NIC to the gateway socket by path, so a restore
	// on the same host re-joins the gateway the instance left, with the same
	// MAC and so the same lease. A gateway that is not there would leave the
	// guest with a NIC that reaches nothing: refuse instead.
	if gw, ok := flagValue(argv, "--net-gateway"); ok {
		if info, err := os.Stat(gw); err != nil || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("cannot restore %s: it ran on the network gateway at %s, which is not "+
				"running; start `hull network-gateway` there first", id, gw)
		}
	}

	state.ControlSock = controlSock
	state.ProfilePath = profilePath
	state.Migration.State = store.MigrationRestoring
	txid := state.Migration.TxID

	result := make(chan string, 1)
	gone := make(chan struct{})
	go func() {
		result <- awaitRestoredAndResume(s, id, controlSock, opt, gone)
	}()
	// The IP the record holds stays the guest's: it resumes with its network
	// state in memory and the same MAC. So no rediscovery, as for Vz.
	started, launchErr := launchVMM(cmd, s, state, argv, nil, hypervisors.HviVmm, opt.detach, "none", "")
	// In the foreground launchVMM returns when the VMM has exited, and on a
	// failed start there never was one. Either way nothing will answer on
	// the socket, and the wait must end now rather than at its deadline.
	if !opt.detach || !started {
		close(gone)
	}

	// launchVMM's own saves carry the migration state as it was at launch.
	// Write the outcome of the wait onto the newest record, keeping
	// everything launchVMM recorded in between.
	final := <-result
	if fresh, err := s.GetInstance(id); err == nil && fresh.Migration != nil && fresh.Migration.TxID == txid {
		if strings.HasPrefix(final, "error: ") {
			fresh.Migration.State = store.MigrationRestoring
		} else {
			fresh.Migration.State = final
		}
		_ = s.SaveInstance(fresh)
	}
	if launchErr != nil {
		return launchErr
	}
	if reason, failed := strings.CutPrefix(final, "error: "); failed {
		return fmt.Errorf("restore of %s: %s", id, reason)
	}
	return nil
}

// awaitRestoredAndResume waits until hvi reports restored-paused on the
// control socket, then resumes the guest unless opt.paused. It returns the
// migration state reached, or "error: <reason>".
//
// It runs beside launchVMM, which in the foreground holds the terminal until
// the VMM exits, so nothing after that call could ever send the resume. Until
// hvi binds the socket, a dial finds nothing or the source's stale socket and
// is refused; both mean "not yet". A VMM that died, or gone closing, ends the
// wait at once rather than at the deadline.
func awaitRestoredAndResume(s *store.Store, id, sock string, opt hviRestoreOptions, gone <-chan struct{}) string {
	const exited = "error: hvi exited before the guest was restored; its reason is in " +
		"`hull logs` (detached) or on the terminal"
	deadline := time.Now().Add(opt.timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Sprintf("error: hvi did not report restored-paused within %s. If it is "+
				"still running, `hull stop %s` ends it; the bundle is kept", opt.timeout, id)
		}
		select {
		case <-gone:
			return exited
		default:
		}
		if rec, err := s.GetInstance(id); err == nil && rec.PID > 0 {
			if syscall.Kill(rec.PID, 0) != nil {
				return exited
			}
		}
		reply, err := hviOp(sock, "status")
		if err == nil && reply.State == store.MigrationRestoredPaused {
			break
		}
		if err == nil && (reply.State == "running" || reply.State == "stopping") {
			return fmt.Sprintf("error: hvi reported %s before the guest was resumed", reply.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if opt.paused {
		recordMigrationState(s, id, store.MigrationRestoredPaused)
		fmt.Fprintf(os.Stderr, "Instance %s restored and paused; `hull resume %s` starts it\n", id, id)
		return store.MigrationRestoredPaused
	}
	if _, err := hviOp(sock, "resume"); err != nil {
		return "error: resume after restore: " + err.Error()
	}
	recordMigrationState(s, id, store.MigrationRestored)
	fmt.Fprintf(os.Stderr, "Instance %s restored and running\n", id)
	return store.MigrationRestored
}

// recordMigrationState sets the migration state on the newest record, while
// launchVMM may be holding the record it started from. Only once the pid is
// recorded, so this write cannot put back the pid-less record launchVMM had
// just replaced.
func recordMigrationState(s *store.Store, id, migrationState string) {
	fresh, err := s.GetInstance(id)
	if err != nil || fresh.Migration == nil || fresh.PID <= 0 {
		return
	}
	fresh.Migration.State = migrationState
	_ = s.SaveInstance(fresh)
}
