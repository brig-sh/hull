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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
			&cli.StringFlag{
				Name:   "move",
				Hidden: true,
				Usage: "hvi: the txid of the move (`hull migrate`) whose received checkpoint this " +
					"restores; with --paused, which a move's restore always is",
			},
			&cli.BoolFlag{
				Name: "adopt-latest",
				Usage: "hvi: restore from the fenced checkpoint latest.json names when the record " +
					"missed it (a checkpoint whose record update failed)",
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
				if cmd.IsSet("paused") || cmd.IsSet("timeout") || cmd.IsSet("adopt-latest") || cmd.IsSet("move") {
					return errors.New("--paused, --timeout and --adopt-latest apply to hvi instances only")
				}
				return vzRestoreInstance(ctx, cmd)
			}
			if cmd.IsSet("gateway-sock") {
				return errors.New("an hvi instance re-joins the gateway socket it ran with " +
					"(--net-gateway on its command line); --gateway-sock is for Vz instances")
			}
			return hviRestoreInstance(cmd, s, state, hviRestoreOptions{
				detach:      cmd.Bool("detach"),
				paused:      cmd.Bool("paused"),
				timeout:     time.Duration(cmd.Int("timeout")) * time.Second,
				adoptLatest: cmd.Bool("adopt-latest"),
				move:        cmd.String("move"),
			})
		},
	}
}

type hviRestoreOptions struct {
	detach      bool
	paused      bool
	timeout     time.Duration
	adoptLatest bool
	// move is the txid of the move whose received checkpoint this restores
	// on its destination. Only that move's own restore may bring up an
	// incoming instance, and only paused.
	move string
}

// hviRestoreArgv turns the command line an hvi instance last ran with into
// the `hvi restore` command line that brings it back from the bundle in from.
//
// The bundle holds the guest's RAM, so it already has the kernel, initramfs
// and command line in it, and its profile fixes the memory size, the vCPU
// count and the machine profile (--profile): those flags are dropped. What
// the bundle cannot hold are the host resources the devices were attached
// to, and those are kept: the disk, the gateway socket (so a restore on the
// same host re-joins the same gateway), the MAC the gateway's leases are
// keyed by, the agent socket, the sandbox id, the event ledger, and the
// checkpoint root, without which the restored guest could never be
// checkpointed again. A bare --net is the deprecated spelling of
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
		"--checkpoint-root": true,
	}
	drop := map[string]bool{
		"--kernel": true, "--initramfs": true, "--cmdline": true, "--mem-mib": true,
		"--cpus": true, "--fs-uid": true, "--fs-gid": true, "--from": true, hviFromStreamFlag: true,
		"--control-sock": true, "--profile-out": true,
		// The base digest and the blocks written over it describe the
		// boot; a restored disk has moved on from it, and the bundle's
		// disk.json carries what hvi recorded. A move's restore names its
		// base again itself (hviRestoreInstance).
		hviDiskBaseFlag: true, hviDiskWrittenFlag: true,
		// The machine profile is in the bundle's own profile.json, which
		// hvi restore checks against the host; it takes no --profile.
		"--profile": true,
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

// Files a streamed move keeps in its spool directory on the source.
const (
	// spoolStream is hvi's migrate stream as hvi wrote it: what `hvi
	// restore --from-stream` reads, should an abort bring the guest back
	// here.
	spoolStream = "stream"
	// spoolSealed holds hvi's reply once the stream is complete and synced.
	// A spool without it may still be whole: see verifySpool.
	spoolSealed = "sealed"
)

// isStreamRestore reports whether cmdLine is an hvi restore from a stream:
// what a streamed move records for the instance it brings in.
func isStreamRestore(cmdLine []string) bool {
	return len(cmdLine) > 1 && cmdLine[1] == "restore" && slices.Contains(cmdLine, hviFromStreamFlag)
}

// restoreStreamSource is where a restore reads a streamed checkpoint from,
// or nil for a bundle directory: standard input for a streamed move's own
// restore on its destination, which `migrate-receive --stream` feeds, and
// the spooled stream for a source that takes back a streamed move it
// aborted.
func restoreStreamSource(state *store.InstanceState, opt hviRestoreOptions) (*os.File, error) {
	m := state.Migration
	if opt.move != "" && isStreamRestore(state.CmdLine) && m.State == store.MigrationIncoming {
		if !opt.detach {
			return nil, errors.New("a streamed move's restore is detached")
		}
		return os.Stdin, nil
	}
	spool := filepath.Join(m.BundleDir, spoolStream)
	if _, err := os.Stat(spool); err != nil {
		return nil, nil
	}
	// A spool hull did not live to seal is sealed here if it checks out
	// whole, frame by frame against its manifest.
	if err := openSpool(m.BundleDir); err != nil {
		return nil, err
	}
	if !opt.detach {
		return nil, errors.New("a checkpoint kept as a stream restores detached (--detach)")
	}
	return os.Open(spool)
}

// streamRestoreArgv turns `restore --from DIR ...` into `restore
// --from-stream - ...`.
func streamRestoreArgv(argv []string) []string {
	out := slices.Clone(argv)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--from" {
			out[i], out[i+1] = hviFromStreamFlag, hviFromStreamStdin
			break
		}
	}
	return out
}

// moveRestoreBase adds the base to the restore argv of a streamed move's
// destination (incoming, reading the stream on stdin): its --disk is a copy
// of the base the stream's disk rounds go onto, and told the base, hvi
// refuses a stream from another base, or one without disk rounds, either of
// which would leave the bare base as the guest's disk. Every other restore
// is left as it is: a directory restore takes no base, and an abort's
// restore from the spool writes the disk rounds back onto the source's own
// disk, which already holds them.
func moveRestoreBase(argv []string, state *store.InstanceState, incoming bool) []string {
	if !incoming || state.Base == nil || checkDiskDigest(state.Base.SHA256) != nil {
		return argv
	}
	return append(argv, hviDiskBaseFlag, state.Base.SHA256)
}

// hviRestoreTakesStream reports whether the hvi at binary restores from a
// stream (`restore --from-stream`), which a streamed move hands it on stdin.
// Asked as the other probes are: hvi's restore parser answers an argument it
// does not know with `unknown restore arg "--from-stream"` (src/main.rs).
func hviRestoreTakesStream(binary string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	probe := exec.CommandContext(ctx, binary, "restore", hviFromStreamFlag)
	probe.Stdout = &out
	probe.Stderr = &out
	err := probe.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("%s restore did not answer a probe within 5s", binary)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return false, fmt.Errorf("run %s: %w", binary, err)
	}
	text := out.String()
	return !strings.Contains(text, fmt.Sprintf("unknown restore arg %q", hviFromStreamFlag)) &&
		!strings.Contains(text, `unknown subcommand "restore"`), nil
}

// hviRestoreTakesStreamBase reports whether the hvi at binary restores a
// stream's disk rounds onto a copy of the base it names (`restore
// --disk-base-sha256`), as a stream-v3 move has it. An hvi from before
// the disk rounds in its migrate stream answers `unknown restore arg
// "--disk-base-sha256"`; one that knows it says the flag needs a value.
func hviRestoreTakesStreamBase(binary string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	probe := exec.CommandContext(ctx, binary, "restore", hviDiskBaseFlag)
	probe.Stdout = &out
	probe.Stderr = &out
	err := probe.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("%s restore did not answer a probe within 5s", binary)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return false, fmt.Errorf("run %s: %w", binary, err)
	}
	text := out.String()
	return !strings.Contains(text, fmt.Sprintf("unknown restore arg %q", hviDiskBaseFlag)) &&
		!strings.Contains(text, `unknown subcommand "restore"`), nil
}

// hviFromStreamFlag is hvi restore's argument for a checkpoint read from a
// stream, and hviFromStreamStdin its value for standard input.
const (
	hviFromStreamFlag  = "--from-stream"
	hviFromStreamStdin = "-"
)

// reconcileLatestCheckpoint compares the record's checkpoint with the one
// <instance>/checkpoint/latest.json names, and adopts the latter into the
// record when asked and safe.
//
// checkpoint writes latest.json before the record, so a failure between the
// two leaves latest.json naming a later checkpoint than the record does.
// Restoring the record's would rewind past a checkpoint whose fate the
// record never learned, so a mismatch is refused unless adopt is set. Even
// then only a fenced one is adopted, since the guest never ran after it,
// and only a bundle below the instance's checkpoint root whose manifest.json
// still hashes to the sha256 latest.json recorded from hvi's reply.
//
// A latest.json that is there and does not parse is refused rather than
// skipped: it is the file that would say a later checkpoint exists, and it
// sits in the directory hvi may write to.
func reconcileLatestCheckpoint(s *store.Store, state *store.InstanceState, adopt bool) error {
	latest, err := readHviCheckpointRecord(instanceCheckpointDir(s, state.ID))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w; hull cannot tell whether a later checkpoint was taken", err)
	}
	recorded := ""
	if state.Migration != nil {
		recorded = state.Migration.TxID
	}
	if latest.TxID == recorded {
		return nil
	}
	if latest.Then != hviThenFence {
		return fmt.Errorf("latest.json names checkpoint %s in %s, a later one the record never "+
			"took in, taken with --then %s: the guest ran on after it, so neither it nor the "+
			"record's can be restored", latest.TxID, latest.Dir, latest.Then)
	}
	if !adopt {
		return fmt.Errorf("its record names checkpoint %q, but latest.json names %s in %s, a later "+
			"one the record never took in. It was fenced, so it can be restored: `hull restore "+
			"--adopt-latest %s` checks its manifest and records it first", recorded, latest.TxID,
			latest.Dir, state.ID)
	}
	root, ok := flagValue(state.CmdLine, "--checkpoint-root")
	if !ok {
		return errors.New("the instance ran without a checkpoint root, so latest.json cannot name a bundle of its own")
	}
	dir, err := bundleDirBelow(root, latest.Dir)
	if err != nil {
		return fmt.Errorf("latest.json: %w", err)
	}
	if latest.ManifestSHA256 == "" {
		return errors.New("latest.json records no manifest sha256 to check its bundle against")
	}
	if err := checkBundleManifest(dir, latest.ManifestSHA256); err != nil {
		return err
	}
	state.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: dir,
		TxID: latest.TxID, ManifestSHA256: latest.ManifestSHA256}
	if err := s.SaveInstance(state); err != nil {
		return fmt.Errorf("record the adopted checkpoint: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Adopted checkpoint %s in %s from latest.json\n", latest.TxID, dir)
	return nil
}

// checkBundleManifest checks that dir/manifest.json hashes to want, the
// sha256 hvi reported when it sealed the bundle. hvi hashes the file's bytes
// as written. The manifest lists every other part with its sha256, which hvi
// checks on restore, so this one hash binds the whole bundle to the
// checkpoint hull recorded, and a bundle rewritten since, by anything that
// can write the checkpoint root, is refused. "" (a record from before hull
// kept the hash) checks nothing.
func checkBundleManifest(dir, want string) error {
	if want == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("bundle %s has no readable manifest: %w", dir, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("bundle %s: manifest.json has sha256 %s, but hvi reported %s when it "+
			"sealed the checkpoint; the bundle was changed since", dir, got, want)
	}
	return nil
}

// restorableMigration reports whether the record's last checkpoint can be
// restored onto the instance's disk.
//
// The disk is not in the bundle; the restore reopens the live one. That is
// only consistent if the guest never wrote to it after the capture:
//   - fenced: the source never ran again. The case a restore is for.
//   - restoring or restored-paused: a restore that did not get as far as
//     running the guest, so the disk is still the one the bundle saw. Every
//     op that may run that guest (the restore's own resume, `hull resume`,
//     any `hull checkpoint`) records restored before it is sent,
//     and setRestoreState never moves a record back, so these two stay true.
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
	case store.MigrationMigrating:
		return fmt.Errorf("its fenced checkpoint belongs to move %s to %s, which decides what becomes "+
			"of it: `hull migrate --abort %s` brings the instance back here", m.MoveTxID, m.Peer, m.MoveTxID)
	case store.MigrationMigrated:
		return fmt.Errorf("it moved to %s (move %s) and runs there; its checkpoint here is stale. "+
			"Moving it back is a new `hull migrate` run on %s", m.Peer, m.MoveTxID, m.Peer)
	case store.MigrationIncoming:
		return fmt.Errorf("it is arriving from %s by move %s, and only that move restores it", m.Peer, m.MoveTxID)
	}
	return fmt.Errorf("its checkpoint is in an unknown state %q", m.State)
}

func hviRestoreInstance(cmd *cli.Command, s *store.Store, state *store.InstanceState, opt hviRestoreOptions) error {
	id := state.ID
	// Held until the wait below ends, not until launchVMM returns: in the
	// foreground that is the restored guest's whole life, and a checkpoint
	// of it must not be refused for as long as it runs.
	unlock, err := lockInstanceMigration(s, id)
	if err != nil {
		return err
	}
	var unlockOnce sync.Once
	release := func() { unlockOnce.Do(unlock) }
	defer release()
	// Read again under the lock: a checkpoint that finished meanwhile
	// changed the record.
	if state, err = s.GetInstance(id); err != nil {
		return fmt.Errorf("instance not found: %s", id)
	}
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
	if err := checkMoveRestore(s, state, opt); err != nil {
		return fmt.Errorf("cannot restore %s: %w", id, err)
	}
	if opt.move == "" {
		if err := reconcileLatestCheckpoint(s, state, opt.adoptLatest); err != nil {
			return fmt.Errorf("cannot restore %s: %w", id, err)
		}
		if err := restorableMigration(state.Migration); err != nil {
			return fmt.Errorf("cannot restore %s: %w", id, err)
		}
	}
	bundle := state.Migration.BundleDir
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("cannot restore %s: its bundle %s is gone", id, bundle)
	}
	stream, err := restoreStreamSource(state, opt)
	if err != nil {
		return fmt.Errorf("cannot restore %s: %w", id, err)
	}
	if stream != nil && stream != os.Stdin {
		defer func() { _ = stream.Close() }()
	}
	// A move's streamed restore starts before its manifest has arrived;
	// hvi checks every component of the stream against it as it reads.
	if stream != os.Stdin {
		if err := checkBundleManifest(bundle, state.Migration.ManifestSHA256); err != nil {
			return fmt.Errorf("cannot restore %s: %w", id, err)
		}
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
	if stream != nil {
		argv = streamRestoreArgv(argv)
	}
	argv = moveRestoreBase(argv, state, stream == os.Stdin)
	// hvi relays the guest's NIC to the gateway socket by path, so a restore
	// on the same host re-joins the gateway the instance left, with the same
	// MAC and so the same lease. A gateway that is not there would leave the
	// guest with a NIC that reaches nothing, since hvi only warns and falls
	// back to its no-egress stub: refuse instead. Dialled, as run's
	// pre-flight does, not just looked at: a gateway that died leaves its
	// socket file behind.
	if gw, ok := flagValue(argv, "--net-gateway"); ok {
		conn, err := net.DialTimeout("unix", gw, 2*time.Second)
		if err != nil {
			return fmt.Errorf("cannot restore %s: it ran on the network gateway at %s, which is not "+
				"answering (%w); start `hull network-gateway` there first", id, gw, err)
		}
		_ = conn.Close()
	}

	state.ControlSock = controlSock
	state.ProfilePath = profilePath
	state.Migration.State = store.MigrationRestoring
	txid := state.Migration.TxID

	type awaited struct {
		state string
		err   error
	}
	result := make(chan awaited, 1)
	gone := make(chan struct{})
	since := time.Now()
	go func() {
		st, err := awaitRestoredAndResume(s, id, txid, controlSock, opt, since, gone)
		release()
		result <- awaited{st, err}
	}()
	// The IP the record holds stays the guest's: it resumes with its network
	// state in memory and the same MAC. So no rediscovery, as for Vz.
	started, launchErr := launchVMM(cmd, s, state, argv, nil, hypervisors.HviVmm, opt.detach, "none", "", stream)
	// In the foreground launchVMM returns when the VMM has exited, and on a
	// failed start there never was one. Either way nothing will answer on
	// the socket, and the wait must end now rather than at its deadline.
	if !opt.detach || !started {
		close(gone)
	}

	// The wait records each state as it is reached; this is the backstop
	// for one it reached and could not record, written through the same
	// one-way setter, so it cannot undo a `hull resume` given meanwhile.
	final := <-result
	if final.state != "" {
		setRestoreState(s, id, txid, final.state)
	}
	if launchErr != nil {
		return launchErr
	}
	if final.err != nil {
		return fmt.Errorf("restore of %s: %w", id, final.err)
	}
	return nil
}

// awaitRestoredAndResume waits until hvi reports restored-paused on the
// control socket, then resumes the guest unless opt.paused. It returns the
// migration state reached, "" for none past restoring, and why it stopped
// short if it did.
//
// It runs beside launchVMM, which in the foreground holds the terminal until
// the VMM exits, so nothing after that call could ever send the resume. Until
// hvi binds the socket, a dial finds nothing or the source's stale socket and
// is refused; both mean "not yet". A VMM that died, or gone closing, ends the
// wait at once rather than at the deadline. since is when the restore began.
//
// The state it returns errs toward "the guest ran": once a resume has been
// sent, the guest may be writing to the disk, and a record that still said
// restored-paused would let a second restore put the bundle's RAM over it.
func awaitRestoredAndResume(s *store.Store, id, txid, sock string, opt hviRestoreOptions,
	since time.Time, gone <-chan struct{}) (string, error) {
	exited := errors.New("hvi exited before the guest was restored; its reason is in " +
		"`hull logs` (detached) or on the terminal")
	deadline := time.Now().Add(opt.timeout)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("hvi did not report restored-paused within %s. If it is "+
				"still running, `hull stop %s` ends it; the bundle is kept", opt.timeout, id)
		}
		select {
		case <-gone:
			return "", exited
		default:
		}
		// Only a pid recorded by this restore says anything about its VMM.
		// Until launchVMM's first save the record is the old one, and a
		// source that died without `hull stop` (a crash, a reboot) left its
		// dead pid there; reading that as "hvi exited" would fail a restore
		// that is going fine. launchVMM stamps StartTime on that save. Until
		// it has saved, the state is not written either: a write from a
		// record read before that save would put back the pid-less one.
		ours := false
		if rec, err := s.GetInstance(id); err == nil && rec.PID > 0 && !rec.StartTime.Before(since) {
			if restoreVMMExited(rec.PID, opt.detach) {
				return "", hviExitedError(exited, rec.LogFile)
			}
			ours = true
		}
		reply, err := hviOp(sock, "status")
		if err == nil && reply.State == store.MigrationRestoredPaused && ours {
			break
		}
		if err == nil && (reply.State == "running" || reply.State == "stopping") {
			return store.MigrationRestored, fmt.Errorf("hvi reported %s before hull resumed the "+
				"guest; it is recorded as having run", reply.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if opt.paused {
		setRestoreState(s, id, txid, store.MigrationRestoredPaused)
		fmt.Fprintf(os.Stderr, "Instance %s restored and paused; `hull resume %s` starts it\n", id, id)
		return store.MigrationRestoredPaused, nil
	}
	// Recorded before the resume is sent: after it, the guest may be
	// running whatever the reply says, and a crash of hull between the two
	// must not leave a record that allows another restore.
	setRestoreState(s, id, txid, store.MigrationRestored)
	if _, err := hviOp(sock, "resume"); err != nil {
		if errors.Is(err, errHviNoReply) {
			return store.MigrationRestored, fmt.Errorf("resume after restore: %w; the guest may "+
				"be running. `hull resume %s` again is harmless if it is", err, id)
		}
		return store.MigrationRestored, fmt.Errorf("resume after restore: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Instance %s restored and running\n", id)
	return store.MigrationRestored, nil
}

// restoreVMMExited reports whether the VMM a restore started has exited.
//
// A detached restore starts hvi as this process's child and never waits on
// it, so an hvi that exits at once (one that refuses the bundle, or has no
// restore for this backend) stays a zombie while the wait runs, and a
// zombie answers signal 0 like a live process. The wait then ran to its
// deadline, two minutes by default, for a VMM that was gone in a
// millisecond. So a detached restore asks wait4 without blocking first,
// which also reaps the zombie. In the foreground launchVMM waits on the
// child itself and closes gone, and reaping it here would take its exit
// status from that wait, so only signal 0 is asked there.
func restoreVMMExited(pid int, detached bool) bool {
	if detached {
		var ws syscall.WaitStatus
		switch wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); {
		case wpid == pid:
			return true
		case err == nil:
			return false
		}
		// ECHILD: not this process's child after all; fall through.
	}
	return syscall.Kill(pid, 0) != nil
}

// hviExitedError adds the last line of the instance log, which is where a
// detached hvi says why it stopped, to the "hvi exited" error.
func hviExitedError(exited error, logFile string) error {
	if logFile == "" {
		return exited
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		return exited
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return exited
	}
	if len(last) > 400 {
		last = last[:400] + "..."
	}
	return fmt.Errorf("%w; it said: %s", exited, last)
}

// setRestoreState records how far the restore of checkpoint txid got.
//
// One way only: restoring, then restored-paused, then restored, and never
// back. Several hull processes write this field while one restore runs (the
// restore itself, its own backstop write, a `hull resume` from another
// terminal), and the last writer is not always the one that knows most. A
// guest recorded as having run must stay so, or a second restore would put
// the bundle's RAM over a disk that guest has written since. A record that
// has moved on to another checkpoint, or out of a restore, is left alone.
func setRestoreState(s *store.Store, id, txid, to string) {
	fresh, err := s.GetInstance(id)
	if err != nil || fresh.Migration == nil || fresh.Migration.TxID != txid {
		return
	}
	rank := map[string]int{
		store.MigrationRestoring:      1,
		store.MigrationRestoredPaused: 2,
		store.MigrationRestored:       3,
	}
	from := rank[fresh.Migration.State]
	if from == 0 || rank[to] <= from {
		return
	}
	fresh.Migration.State = to
	_ = s.SaveInstance(fresh)
}

// markRestoredGuestRunning records that a restored guest is about to run,
// before the op that may run it is sent: `hull resume`, or any checkpoint,
// which resumes it after "continue" or a failed capture. From then on the
// disk may move on from the bundle, so a restore from it is refused. Sent
// first, because the reply may never come and the op may still take
// effect. A record not in a restore is left alone.
func markRestoredGuestRunning(s *store.Store, id string) error {
	fresh, err := s.GetInstance(id)
	if err != nil {
		return err
	}
	m := fresh.Migration
	if m == nil || (m.State != store.MigrationRestoring && m.State != store.MigrationRestoredPaused) {
		return nil
	}
	m.State = store.MigrationRestored
	if err := s.SaveInstance(fresh); err != nil {
		return fmt.Errorf("record that the restored guest of %s runs: %w", id, err)
	}
	return nil
}

// keepRecordedMigration replaces state.Migration with the record's on disk,
// for a caller about to save a record it has held while the VMM ran.
//
// Other hull processes write Migration while a VMM runs: `hull checkpoint`
// records a fenced bundle, `hull resume` that a restored guest ran. In the
// foreground launchVMM holds the record it started from for the VMM's whole
// life, and saving it at exit as it was would put back the migration state
// from launch: a fenced checkpoint forgotten, so it cannot be restored, or a
// restored guest that ran recorded as one that never did, so it can be
// restored again over a disk it has written.
func keepRecordedMigration(s *store.Store, state *store.InstanceState) {
	if fresh, err := s.GetInstance(state.ID); err == nil {
		state.Migration = fresh.Migration
	}
}

// checkMoveRestore holds a restore to the rules of a move. Without --move it
// refuses an instance a move has in hand (refuseDuringMove). With it, the
// restore must be that move's own, on its destination, paused, of the
// received checkpoint, before the commit: the move's restore step.
func checkMoveRestore(s *store.Store, state *store.InstanceState, opt hviRestoreOptions) error {
	if opt.move == "" {
		return refuseDuringMove(s, state.ID, "restore")
	}
	if !opt.paused {
		return errors.New("a move's restore is always --paused; only its commit resumes the guest")
	}
	rec, err := readMigrationRecord(s.InstanceMigrationRecord(state.ID))
	if err != nil {
		return fmt.Errorf("move %s: %w", opt.move, err)
	}
	m := state.Migration
	switch {
	case rec.TxID != opt.move || rec.Role != roleDestination:
		return fmt.Errorf("the instance is not the destination of move %s", opt.move)
	case rec.State == dstReceiving && isStreamRestore(state.CmdLine):
		// A streamed move restores while the stream arrives; its commit
		// still waits for the stream to be verified.
	case rec.State != dstVerified && rec.State != dstRestoredPaused:
		return fmt.Errorf("move %s is %s here, and only a verified one is restored", opt.move, rec.State)
	case m == nil || m.MoveTxID != opt.move || m.BundleDir == "":
		return fmt.Errorf("the instance record holds no checkpoint of move %s", opt.move)
	case m.State != store.MigrationIncoming && m.State != store.MigrationRestoring &&
		m.State != store.MigrationRestoredPaused:
		return fmt.Errorf("the checkpoint of move %s is %s, so it may have run already", opt.move, m.State)
	}
	return nil
}
