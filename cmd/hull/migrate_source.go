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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// `hull migrate NAME --to USER@HOST` moves a running hvi instance to another
// host over ssh. This file is the source's half: it drives every step, and
// the destination answers through the hidden verbs in migrate_dest.go.
// docs/migration.md is the protocol, its states and its failure table.

// migrateOptions are the flags of one `hull migrate` run.
type migrateOptions struct {
	to          string
	remote      migrateRemote
	txid        string
	dryRun      bool
	fenceTimout time.Duration
	// out takes the progress lines.
	out io.Writer
}

func migrateCommand() *cli.Command {
	return &cli.Command{
		Name: "migrate",
		Usage: "move a running hvi instance to another host over ssh; the source is fenced " +
			"before the destination may run it, and never runs it again once the move commits",
		ArgsUsage: "NAME | TXID",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "to", Usage: "USER@HOST to move the instance to, over ssh"},
			&cli.StringFlag{Name: "remote-store", Usage: "the destination hull's --store-dir (default: its own default)"},
			&cli.StringFlag{Name: "remote-hull", Usage: "path of hull on the destination (default: hull on its PATH)"},
			&cli.StringFlag{Name: "remote-hvi", Usage: "path of hvi on the destination (default: the one its hull would run)"},
			&cli.StringFlag{Name: "remote-gateway-sock", Usage: "a network gateway on the destination, with the " +
				"source's subnet, for the moved guest's NIC (default: none; a guest that ran on a gateway gets " +
				"hvi's no-egress stub NIC instead)"},
			&cli.StringFlag{Name: "txid", Usage: "resume the move with this txid from where it stopped, or start one with it"},
			&cli.BoolFlag{Name: "dry-run", Usage: "check both hosts and print the plan; change nothing on either"},
			&cli.BoolFlag{Name: "abort", Usage: "abort the move NAME or TXID names, if it has not committed, and run the instance here again"},
			&cli.BoolFlag{Name: "status", Usage: "print the move's record on both hosts and what it means"},
			&cli.IntFlag{Name: "timeout", Value: 120, Usage: "seconds to wait for hvi to write the fenced checkpoint"},
			&cli.BoolFlag{Name: "allow-no-restore", Usage: "testing only: go on although an hvi on either side has no " +
				"restore subcommand. The source is fenced and nothing can run the guest again until both can restore"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 1 {
				return errors.New("name the instance (or, for --abort and --status, the move's txid)")
			}
			s, err := globalStore(cmd)
			if err != nil {
				return err
			}
			arg := cmd.Args().First()
			if cmd.Bool("abort") || cmd.Bool("status") {
				if cmd.Bool("abort") && cmd.Bool("status") {
					return errors.New("--abort and --status do not go together")
				}
				name, err := moveInstanceName(s, arg)
				if err != nil {
					return err
				}
				if cmd.Bool("status") {
					return sourceStatus(ctx, s, name, os.Stdout, nil)
				}
				return sourceAbort(ctx, s, name, os.Stderr, nil)
			}
			opt := migrateOptions{
				to: cmd.String("to"),
				remote: migrateRemote{
					Target: cmd.String("to"), Store: cmd.String("remote-store"), Hull: cmd.String("remote-hull"),
					Hvi: cmd.String("remote-hvi"), GatewaySock: cmd.String("remote-gateway-sock"),
					AllowNoRestore: cmd.Bool("allow-no-restore"),
				},
				txid:        cmd.String("txid"),
				dryRun:      cmd.Bool("dry-run"),
				fenceTimout: time.Duration(cmd.Int("timeout")) * time.Second,
				out:         os.Stderr,
			}
			return sourceMigrate(ctx, s, arg, opt, nil)
		},
	}
}

// moveInstanceName resolves the argument of --abort or --status: a move's
// txid recorded in this store, or an instance name.
func moveInstanceName(s *store.Store, arg string) (string, error) {
	if txidPattern.MatchString(arg) {
		if rec, _, err := findMigrationByTxID(s, arg); err == nil {
			return rec.Name, nil
		}
	}
	if err := store.ValidateInstanceID(arg); err != nil {
		return "", err
	}
	return arg, nil
}

// peerFor is the destination of rec, over ssh unless the caller gave one.
func peerFor(rec *migrationRecord, given movePeer) movePeer {
	if given != nil {
		return given
	}
	return &sshPeer{target: rec.Remote.Target, remote: *rec.Remote}
}

// sourceMigrate is `hull migrate NAME --to TARGET`: a new move, or the move
// --txid names resumed from its recorded state. peer is nil outside tests.
func sourceMigrate(ctx context.Context, s *store.Store, name string, opt migrateOptions, peer movePeer) error {
	if err := store.ValidateInstanceID(name); err != nil {
		return err
	}
	state, err := s.GetInstance(name)
	if err != nil {
		return fmt.Errorf("instance not found: %s", name)
	}
	unlock, err := lockMove(s.InstanceDir(name), 0)
	if errors.Is(err, errMoveBusy) {
		return fmt.Errorf("another `hull migrate` of %s is running", name)
	}
	if err != nil {
		return err
	}
	defer unlock()

	path := s.InstanceMigrationRecord(name)
	rec, err := readMigrationRecord(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		rec = nil
	case err != nil:
		return err
	case rec.Role != roleSource:
		if rec.State != dstRunning {
			return moveRefusal(s, name, "migrate")
		}
		// It arrived here by a move that committed: moving it on is a new
		// move, which replaces that record.
		rec = nil
	case rec.State == migAborted:
		rec = nil
	case opt.txid != "" && opt.txid != rec.TxID:
		return fmt.Errorf("instance %s has move %s on record (%s), not %s", name, rec.TxID, rec.State, opt.txid)
	case opt.txid == "" && rec.State == srcCommitted:
		return moveRefusal(s, name, "migrate")
	case opt.txid == "":
		return fmt.Errorf("instance %s is in move %s to %s, stopped at %s%s. Resume it with "+
			"`hull migrate %s --txid %s`, or end it with `hull migrate --abort %s`", name, rec.TxID,
			rec.Peer, rec.State, describeRecordError(rec), name, rec.TxID, rec.TxID)
	case opt.to != "" && opt.to != rec.Remote.Target:
		return fmt.Errorf("move %s goes to %s, not %s", rec.TxID, rec.Remote.Target, opt.to)
	}

	probed := false
	if rec == nil {
		if rec, err = startMove(ctx, s, state, opt, peer); err != nil || rec == nil {
			return err
		}
		probed = true
	} else {
		_, _ = fmt.Fprintf(opt.out, "Resuming move %s of %s to %s at %s\n", rec.TxID, name, rec.Peer, rec.State)
	}
	m := &moveSource{s: s, name: name, path: path, rec: rec, peer: peerFor(rec, peer), out: opt.out,
		fenceTimeout: opt.fenceTimout, probed: probed}
	return m.run(ctx)
}

// describeRecordError is ", because ..." for a record whose last step
// failed.
func describeRecordError(rec *migrationRecord) string {
	if rec.Error == "" {
		return ""
	}
	return " (" + rec.Error + ")"
}

// startMove checks the instance and the destination and, unless this is a
// dry run, writes the new move's record in preflight. It returns nil and no
// error for a dry run.
func startMove(ctx context.Context, s *store.Store, state *store.InstanceState, opt migrateOptions,
	peer movePeer) (*migrationRecord, error) {
	if opt.to == "" {
		return nil, errors.New("--to USER@HOST is required")
	}
	if err := checkSSHTarget(opt.to); err != nil {
		return nil, err
	}
	txid := opt.txid
	if txid == "" {
		var err error
		if txid, err = newMoveTxID(); err != nil {
			return nil, err
		}
	} else if err := checkTxID(txid); err != nil {
		return nil, err
	}
	spec, err := sourcePreflight(s, state, opt)
	if err != nil {
		return nil, fmt.Errorf("cannot move %s: %w", state.ID, err)
	}
	rec := &migrationRecord{
		TxID: txid, Role: roleSource, Name: state.ID, Epoch: state.OwnerEpoch + 1, State: srcPreflight,
		Peer: opt.to, Remote: &opt.remote, Spec: spec,
	}
	// Probed before the record is written: a destination that cannot take
	// the instance leaves nothing behind on either host to resume or abort.
	probe, err := probeDestination(ctx, peerFor(rec, peer), rec, s, state)
	if err != nil {
		return nil, fmt.Errorf("cannot move %s: %w", state.ID, err)
	}
	if opt.dryRun {
		printMovePlan(opt.out, rec, state, probe)
		return nil, nil
	}
	rec.History = []migrateHistory{{State: srcPreflight, At: time.Now().UTC()}}
	if err := writeMigrationRecord(s.InstanceMigrationRecord(state.ID), rec); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(opt.out, "Move %s of %s to %s, epoch %d\n", txid, state.ID, opt.to, rec.Epoch)
	return rec, nil
}

// sourcePreflight checks that the instance can be moved at all, and returns
// what the destination must build. Nothing has changed anywhere when it
// fails.
func sourcePreflight(s *store.Store, state *store.InstanceState, opt migrateOptions) (*migrateSpec, error) {
	if err := requireRunningHvi(state, "migrate"); err != nil {
		return nil, err
	}
	if err := hviCheckpointable(state.CmdLine); err != nil {
		return nil, err
	}
	if m := state.Migration; m != nil && m.State == store.MigrationIncoming {
		return nil, errors.New("it is arriving by a move that has not committed")
	}
	switch vm := hviVMState(state); vm {
	case "running", "paused":
	default:
		return nil, fmt.Errorf("its VM is %s, and a move takes a running or paused one", vm)
	}
	// A command line restore cannot carry cannot be moved either: the
	// destination brings the instance up with a restore.
	if _, err := hviRestoreArgv(state.CmdLine, "/check", "/check", "/check"); err != nil {
		return nil, err
	}
	if !opt.remote.AllowNoRestore {
		has, err := hviHasRestore(state.CmdLine[0])
		if err != nil {
			return nil, err
		}
		if !has {
			return nil, fmt.Errorf("its hvi (%s) has no restore subcommand, so an aborted move could "+
				"never run it here again. Use an hvi that can restore (or, to test the transfer "+
				"alone, --allow-no-restore)", state.CmdLine[0])
		}
	}
	host, _ := os.Hostname()
	spec := &migrateSpec{SourceHost: host, ImageDigest: state.ImageDigest, MAC: state.MAC, IP: state.IP,
		Hvi: opt.remote.Hvi, Net: "none"}
	argv := state.CmdLine
	if v, ok := flagValue(argv, "--net-mac"); ok {
		spec.MAC = v
	}
	if v, ok := flagValue(argv, "--sandbox-id"); ok {
		spec.SandboxID = v
	}
	_, spec.Agent = flagValue(argv, "--agent-sock")
	_, spec.Events = flagValue(argv, "--events")
	switch {
	case slices.Contains(argv, "--net-gateway"):
		if opt.remote.GatewaySock != "" {
			spec.Net, spec.GatewaySock = "gateway", opt.remote.GatewaySock
		} else {
			spec.Net = "stub"
			_, _ = fmt.Fprintf(opt.out, "warning: %s runs on a network gateway here, and no --remote-gateway-sock "+
				"was given: on the destination its NIC is hvi's stub, which reaches nothing\n", state.ID)
		}
	case slices.Contains(argv, "--net-stub") || slices.Contains(argv, "--net"):
		spec.Net = "stub"
	}
	return spec, nil
}

// probeDestination runs migrate-probe and holds the answer to what the move
// needs.
func probeDestination(ctx context.Context, peer movePeer, rec *migrationRecord, s *store.Store,
	state *store.InstanceState) (*migrateProbe, error) {
	args := []string{"--name", rec.Name}
	if rec.Remote.Hvi != "" {
		args = append(args, "--hvi", rec.Remote.Hvi)
	}
	if rec.Remote.GatewaySock != "" {
		args = append(args, "--gateway-sock", rec.Remote.GatewaySock)
	}
	reply, err := peer.call(ctx, "migrate-probe", args, nil)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", rec.Peer, err)
	}
	p := reply.Probe
	if p == nil {
		return nil, fmt.Errorf("probe %s: the reply has no probe", rec.Peer)
	}
	var why []string
	if p.OS != runtime.GOOS || p.Arch != runtime.GOARCH {
		why = append(why, fmt.Sprintf("it is %s/%s and this host %s/%s: a checkpoint restores only on "+
			"the same kind of host", p.OS, p.Arch, runtime.GOOS, runtime.GOARCH))
	}
	if !p.Hypervisor {
		why = append(why, "it cannot run a VM: "+p.HypervisorDetail)
	}
	switch {
	case p.Hvi == "" || p.HviError != "":
		why = append(why, "its hvi: "+p.HviError)
	case !p.HviControlFlags:
		why = append(why, fmt.Sprintf("its hvi (%s) predates the control socket", p.Hvi))
	case !p.HviRestore && !rec.Remote.AllowNoRestore:
		why = append(why, fmt.Sprintf("its hvi (%s) has no restore subcommand", p.Hvi))
	}
	switch {
	case p.NameTaken && p.NameTxID != rec.TxID && p.NameMove == roleSource+" "+srcCommitted:
		// Moving an instance back to where it ran before: that host keeps
		// the stale copy the first move left, under the same name.
		why = append(why, fmt.Sprintf("it still has the stale copy of %s that move %s left when it moved "+
			"away; `hull rm %s` there removes it", rec.Name, p.NameTxID, rec.Name))
	case p.NameTaken && p.NameTxID != rec.TxID:
		why = append(why, fmt.Sprintf("an instance named %s already exists there", rec.Name))
	}
	if rec.Remote.GatewaySock != "" && !p.GatewayOK {
		why = append(why, fmt.Sprintf("its gateway %s does not answer: %s", rec.Remote.GatewaySock, p.GatewayDetail))
	}
	if need := moveSpaceNeeded(s, state); p.FreeBytes >= 0 && p.FreeBytes < need {
		why = append(why, fmt.Sprintf("its store %s has %s free and the move needs about %s",
			p.StoreDir, humanBytes(p.FreeBytes), humanBytes(need)))
	}
	if len(why) > 0 {
		return p, fmt.Errorf("%s cannot take %s: %s", rec.Peer, rec.Name, strings.Join(why, "; "))
	}
	return p, nil
}

// moveSpaceNeeded estimates what a move writes on the destination: the
// disk's allocated bytes and the guest's whole RAM (the bundle holds at most
// that), plus a tenth.
func moveSpaceNeeded(s *store.Store, state *store.InstanceState) int64 {
	var need int64
	if disk, ok := flagValue(state.CmdLine, "--disk"); ok {
		need += fileAllocated(disk)
	}
	if mib, ok := flagValue(state.CmdLine, "--mem-mib"); ok {
		if n, err := strconv.ParseInt(mib, 10, 64); err == nil {
			need += n << 20
		}
	} else if data, err := os.ReadFile(s.InstanceProfilePath(state.ID)); err == nil {
		var p struct {
			RAMRegions []struct {
				Size int64 `json:"size"`
			} `json:"ram_regions"`
		}
		if json.Unmarshal(data, &p) == nil {
			for _, r := range p.RAMRegions {
				need += r.Size
			}
		}
	}
	return need + need/10
}

// humanBytes renders n for a person.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// printMovePlan is the dry run's answer.
func printMovePlan(w io.Writer, rec *migrationRecord, state *store.InstanceState, p *migrateProbe) {
	_, _ = fmt.Fprintf(w, "Dry run: %s can move to %s\n", state.ID, rec.Peer)
	_, _ = fmt.Fprintf(w, "  destination: hull %s on %s/%s, hvi %s (%s), store %s with %s free\n",
		p.HullVersion, p.OS, p.Arch, p.Hvi, p.HviVersion, p.StoreDir, humanBytes(p.FreeBytes))
	_, _ = fmt.Fprintf(w, "  network: %s", rec.Spec.Net)
	if rec.Spec.GatewaySock != "" {
		_, _ = fmt.Fprintf(w, " at %s", rec.Spec.GatewaySock)
	}
	_, _ = fmt.Fprintf(w, ", MAC %s\n", rec.Spec.MAC)
	_, _ = fmt.Fprintf(w, "  epoch %d -> %d\n", state.OwnerEpoch, rec.Epoch)
	_, _ = fmt.Fprintln(w, "  steps: reserve the name there, fence this VM (it never runs here again unless the "+
		"move is aborted), stream the checkpoint and the disk, verify, restore paused there, commit")
	if !p.HviRestore {
		_, _ = fmt.Fprintln(w, "  warning: the destination's hvi has no restore subcommand; the move will stop "+
			"before the commit with the source fenced")
	}
}

// moveSource drives one move from its recorded state to committed.
type moveSource struct {
	s            *store.Store
	name         string
	path         string
	rec          *migrationRecord
	peer         movePeer
	out          io.Writer
	fenceTimeout time.Duration
	// probed is set when this run has already probed the destination.
	probed bool
}

func (m *moveSource) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(m.out, format+"\n", args...)
}

func (m *moveSource) timing(step string, since time.Time) {
	if m.rec.Timings == nil {
		m.rec.Timings = map[string]int64{}
	}
	m.rec.Timings[step] = time.Since(since).Milliseconds()
}

// run takes the move from its recorded state to committed, one step at a
// time. Each step records its state only once its effect is durable, and
// each finds its effect already there when it is run again, so a run that
// stops anywhere can be run again with the same txid.
func (m *moveSource) run(ctx context.Context) error {
	for {
		var err error
		switch m.rec.State {
		case srcPreflight:
			err = m.reserve(ctx)
		case srcReserved:
			err = m.fence()
		case srcFenced:
			err = m.rec.advance(m.path, srcTransferring)
		case srcTransferring:
			err = m.transfer(ctx)
		case srcTransferred:
			err = m.prepare(ctx)
		case srcCommitting:
			err = m.commit(ctx)
		case srcCommitted:
			return m.finish()
		default:
			return fmt.Errorf("move %s is %s", m.rec.TxID, m.rec.State)
		}
		if err != nil {
			return m.stopped(err)
		}
	}
}

// stopped records why the move stopped and says what the operator can do.
func (m *moveSource) stopped(err error) error {
	err = m.rec.fail(m.path, err)
	fenced := migrateStateOrder(roleSource, m.rec.State) >= migrateStateOrder(roleSource, srcFenced)
	next := fmt.Sprintf("`hull migrate %s --txid %s` resumes it from here", m.name, m.rec.TxID)
	if m.rec.State == srcCommitting {
		return fmt.Errorf("move %s stopped at %s: %w\nThe source stays fenced. The destination may "+
			"have committed; %s and records the commit, or `hull migrate --abort %s` ends the move "+
			"if the destination confirms it is not running", m.rec.TxID, m.rec.State, err, next, m.rec.TxID)
	}
	if fenced {
		return fmt.Errorf("move %s stopped at %s: %w\nThe source stays fenced and will not run "+
			"until the move commits or is aborted. %s; `hull migrate --abort %s` brings the instance "+
			"back here", m.rec.TxID, m.rec.State, err, next, m.rec.TxID)
	}
	return fmt.Errorf("move %s stopped at %s: %w\nThe instance still runs here. %s; `hull migrate "+
		"--abort %s` releases the destination", m.rec.TxID, m.rec.State, err, next, m.rec.TxID)
}

// reserve probes the destination and claims the instance's name there.
func (m *moveSource) reserve(ctx context.Context) error {
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	if !m.probed {
		if _, err := probeDestination(ctx, m.peer, m.rec, m.s, state); err != nil {
			return err
		}
	}
	req, err := json.Marshal(migrateReserveRequest{Epoch: m.rec.Epoch, Spec: *m.rec.Spec})
	if err != nil {
		return err
	}
	if _, err := m.peer.call(ctx, "migrate-receive", []string{"--txid", m.rec.TxID, "--name", m.name, "--reserve"},
		bytes.NewReader(req)); err != nil {
		return fmt.Errorf("reserve %s on %s: %w", m.name, m.rec.Peer, err)
	}
	m.logf("Reserved %s on %s", m.name, m.rec.Peer)
	return m.rec.advance(m.path, srcReserved)
}

// fence takes the checkpoint that stops the guest for good, with the move's
// txid and epoch, and hands it to the move: the instance record says
// migrating, which a restore refuses. A fence already taken (a re-run after
// a crash) is found and adopted.
func (m *moveSource) fence() error {
	started := time.Now()
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	if !fencedFor(state, m.rec.TxID) {
		if err := m.fenceNow(state); err != nil {
			return err
		}
		m.timing("fence", started)
	}
	state, err = m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	state.Migration.State = store.MigrationMigrating
	state.Migration.MoveTxID = m.rec.TxID
	state.Migration.Peer = m.rec.Peer
	if err := m.s.SaveInstance(state); err != nil {
		return err
	}
	disk, ok := flagValue(state.CmdLine, "--disk")
	if !ok {
		return errors.New("the instance has no --disk")
	}
	size, mod, err := fileModNs(disk)
	if err != nil {
		return err
	}
	if want, ok, err := bundleDiskSize(state.Migration.BundleDir); err != nil {
		return err
	} else if ok && want != size {
		return fmt.Errorf("the disk %s is %d bytes, but the fenced checkpoint says %d", disk, size, want)
	}
	m.rec.BundleDir = state.Migration.BundleDir
	m.rec.ManifestSHA256 = state.Migration.ManifestSHA256
	m.rec.Disk = &migrateDisk{Path: disk, Size: size, ModNs: mod}
	m.logf("Fenced %s: checkpoint %s", m.name, m.rec.BundleDir)
	return m.rec.advance(m.path, srcFenced)
}

// fencedFor reports whether the record already holds the fenced checkpoint
// of move txid.
func fencedFor(state *store.InstanceState, txid string) bool {
	mig := state.Migration
	return mig != nil && mig.TxID == txid &&
		(mig.State == store.MigrationFenced || mig.State == store.MigrationMigrating)
}

// fenceNow takes the fenced checkpoint, or adopts one hvi took for this move
// when hull did not live to record it.
func (m *moveSource) fenceNow(state *store.InstanceState) error {
	if err := requireRunningHvi(state, "migrate"); err != nil {
		return fmt.Errorf("%w, and no fenced checkpoint for move %s is recorded", err, m.rec.TxID)
	}
	unlock, err := lockInstanceMigration(m.s, m.name)
	if err != nil {
		return err
	}
	defer unlock()
	root, ok := flagValue(state.CmdLine, "--checkpoint-root")
	if !ok {
		return errors.New("the instance has no checkpoint root")
	}
	dir := filepath.Join(root, m.rec.TxID)
	switch vm := hviVMState(state); vm {
	case store.MigrationFenced:
		// Fenced, and not on record: either this move's fence, whose
		// reply hull did not live to read, or someone else's.
		return m.adoptFence(state, dir)
	case "running", "paused":
		// A capture that failed or was cut short leaves no manifest; its
		// leftovers would make the directory unusable for the retry.
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); errors.Is(err, os.ErrNotExist) {
			emptyDir(dir)
		}
	default:
		return fmt.Errorf("its VM is %s; a move fences a running or paused one", vm)
	}
	m.logf("Fencing %s (checkpoint with then=fence)", m.name)
	ck, err := hviCheckpointLocked(m.s, state, hviThenFence, "", m.rec.TxID, m.rec.Epoch, m.fenceTimeout)
	if err != nil {
		return err
	}
	m.logf("Checkpoint written in %s", describeCheckpointStats(hviThenFence, ck.Stats))
	return nil
}

// adoptFence records the fenced bundle hvi wrote for this move into dir when
// the checkpoint's reply was lost, after hvi has verified it.
func (m *moveSource) adoptFence(state *store.InstanceState, dir string) error {
	sum, err := fileSHA256(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("the VM is fenced, but not by this move (no bundle at %s): `hull migrate --abort %s`",
			dir, m.rec.TxID)
	}
	if _, err := hviCheckpointVerify(state.CmdLine[0], dir); err != nil {
		return err
	}
	state.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: dir, TxID: m.rec.TxID,
		ManifestSHA256: sum}
	if err := m.s.SaveInstance(state); err != nil {
		return err
	}
	m.logf("Adopted the fenced checkpoint %s", dir)
	return nil
}

// checkDiskUnchanged refuses a disk that is not the one fenced.
func (m *moveSource) checkDiskUnchanged() error {
	d := m.rec.Disk
	size, mod, err := fileModNs(d.Path)
	if err != nil {
		return err
	}
	if size != d.Size || mod != d.ModNs {
		return fmt.Errorf("the disk %s changed after the fence (size %d -> %d, mtime %d -> %d): something "+
			"wrote it while the guest was fenced, and the checkpoint no longer matches it", d.Path, d.Size,
			size, d.ModNs, mod)
	}
	return nil
}

// bundleFiles lists what of the bundle a move sends: manifest.json and
// every component it lists, by bare name.
func bundleFiles(dir string) ([]string, error) {
	comps, err := readManifestComponents(dir)
	if err != nil {
		return nil, err
	}
	files := []string{"manifest.json"}
	for _, c := range comps {
		files = append(files, c.Path)
	}
	return files, nil
}

// transfer streams the bundle and the disk to migrate-receive, which ends
// verified on the destination. A destination already verified is not sent
// it again, but the disk is checked as after a stream: unchanged since the
// fence, and the same as what the destination holds by the source's own
// hash.
func (m *moveSource) transfer(ctx context.Context) error {
	if reply, err := m.peer.call(ctx, "migrate-status", []string{"--txid", m.rec.TxID}, nil); err == nil &&
		reply.Record != nil &&
		migrateStateOrder(roleDestination, reply.Record.State) >= migrateStateOrder(roleDestination, dstVerified) {
		if err := m.checkDiskUnchanged(); err != nil {
			return err
		}
		if err := m.sourceDiskSum(); err != nil {
			return err
		}
		if err := m.compareReceived(reply.Record); err != nil {
			return err
		}
		return m.rec.advance(m.path, srcTransferred)
	}
	if err := m.checkDiskUnchanged(); err != nil {
		return err
	}
	files, err := bundleFiles(m.rec.BundleDir)
	if err != nil {
		return err
	}
	comps, err := readManifestComponents(m.rec.BundleDir)
	if err != nil {
		return err
	}
	m.logf("Sending %s to %s", m.name, m.rec.Peer)
	started := time.Now()
	pr, pw := io.Pipe()
	sw := newMigrateStreamWriter(pw)
	var diskSum streamSum
	sent := make(chan error, 1)
	go func() {
		err := func() error {
			if err := sw.header(streamHeader{TxID: m.rec.TxID, ManifestSHA256: m.rec.ManifestSHA256}); err != nil {
				return err
			}
			for _, f := range files {
				sum, err := sw.file("bundle/"+f, filepath.Join(m.rec.BundleDir, f))
				if err != nil {
					return err
				}
				// The source checks its own bundle against the manifest
				// as it reads it: a bundle damaged here is not sent.
				if f == "manifest.json" && sum.SHA256 != m.rec.ManifestSHA256 {
					return fmt.Errorf("manifest.json has sha256 %s, but hvi reported %s at the fence", sum.SHA256, m.rec.ManifestSHA256)
				}
				for _, c := range comps {
					if c.Path == f && (c.SHA256 != sum.SHA256 || c.Size != sum.Size) {
						return fmt.Errorf("the bundle's %s does not match its manifest", f)
					}
				}
			}
			var err error
			if diskSum, err = sw.sparse("disk", m.rec.Disk.Path); err != nil {
				return err
			}
			return sw.finish()
		}()
		_ = pw.CloseWithError(err)
		sent <- err
	}()
	reply, err := m.peer.call(ctx, "migrate-receive", []string{"--txid", m.rec.TxID, "--name", m.name}, pr)
	_ = pr.CloseWithError(errors.New("the destination stopped reading"))
	sendErr := <-sent
	if sendErr == nil {
		// The disk's hash as sent, on record before the reply is looked
		// at: a reply that is lost leaves a re-run to compare the
		// destination's copy with it.
		m.rec.Disk.SHA256 = diskSum.SHA256
		m.rec.Disk.DataBytes = diskSum.DataBytes
		if werr := writeMigrationRecord(m.path, m.rec); werr != nil {
			return werr
		}
	}
	if sendErr != nil && !errors.Is(sendErr, io.ErrClosedPipe) && err == nil {
		err = fmt.Errorf("send: %w", sendErr)
	}
	if err != nil {
		if sendErr != nil && !errors.Is(sendErr, io.ErrClosedPipe) && !strings.Contains(err.Error(), sendErr.Error()) {
			err = fmt.Errorf("%w (sending: %v)", err, sendErr)
		}
		return fmt.Errorf("transfer to %s: %w", m.rec.Peer, err)
	}
	took := time.Since(started)
	if err := m.checkDiskUnchanged(); err != nil {
		return err
	}
	if reply.Record == nil {
		return errors.New("the destination's receive reply has no record")
	}
	if sendErr != nil {
		// The destination answered without reading it all: it had it.
		if err := m.sourceDiskSum(); err != nil {
			return err
		}
	} else {
		m.rec.Transfer = &migrateTransfer{Bytes: sw.written(), Seconds: took.Seconds(), StartedAt: started.UTC(),
			BytesPerSec: int64(float64(sw.written()) / max(took.Seconds(), 1e-3))}
		m.timing("transfer", started)
	}
	if err := m.compareReceived(reply.Record); err != nil {
		return err
	}
	if t := m.rec.Transfer; t != nil {
		m.logf("Sent %s in %.1f s (%s/s); disk %s of %s carried; destination: %s", humanBytes(t.Bytes),
			t.Seconds, humanBytes(t.BytesPerSec), humanBytes(m.rec.Disk.DataBytes), humanBytes(m.rec.Disk.Size),
			reply.Record.Verify)
	}
	return m.rec.advance(m.path, srcTransferred)
}

// compareReceived checks the destination's record of what it received
// against what the source sent.
func (m *moveSource) compareReceived(dst *migrationRecord) error {
	if dst.TxID != m.rec.TxID || dst.Epoch != m.rec.Epoch {
		return fmt.Errorf("the destination recorded move %s epoch %d, not %s epoch %d", dst.TxID, dst.Epoch,
			m.rec.TxID, m.rec.Epoch)
	}
	if dst.ManifestSHA256 != m.rec.ManifestSHA256 {
		return fmt.Errorf("the destination has manifest sha256 %s, the source %s", dst.ManifestSHA256, m.rec.ManifestSHA256)
	}
	if m.rec.Disk.SHA256 == "" {
		return errors.New("the source has no hash of its disk to compare the destination's with")
	}
	if dst.Disk == nil || dst.Disk.Size != m.rec.Disk.Size || dst.Disk.SHA256 != m.rec.Disk.SHA256 {
		return errors.New("the destination's disk is not the one the source sent")
	}
	return nil
}

// sourceDiskSum hashes the source's disk as the stream would carry it, when
// the record has no hash of it yet: the stream was cut before its end, or
// hull did not live to record it.
func (m *moveSource) sourceDiskSum() error {
	if m.rec.Disk.SHA256 != "" {
		return nil
	}
	sum, err := newMigrateStreamWriter(io.Discard).sparse("disk", m.rec.Disk.Path)
	if err != nil {
		return fmt.Errorf("hash the disk: %w", err)
	}
	m.rec.Disk.SHA256 = sum.SHA256
	m.rec.Disk.DataBytes = sum.DataBytes
	return nil
}

// prepare has the destination restore the checkpoint paused. Only once that
// worked does the source record committing: a restore the destination
// refuses is learned while an abort can still bring the instance back here.
func (m *moveSource) prepare(ctx context.Context) error {
	started := time.Now()
	m.logf("Restoring %s paused on %s", m.name, m.rec.Peer)
	if _, err := m.peer.call(ctx, "migrate-prepare", []string{"--txid", m.rec.TxID}, nil); err != nil {
		if refusalCode(err) == codeRestoreRefused {
			return fmt.Errorf("%s could not restore the checkpoint: %w", m.rec.Peer, err)
		}
		return err
	}
	m.timing("prepare", started)
	// The source's commit point. Durable before the commit is sent:
	// from here an abort needs the destination's word that it is not
	// running.
	return m.rec.advance(m.path, srcCommitting)
}

// commit tells the destination to run the VM, and records the commit once
// the destination says it has taken over this move at this epoch. A reply
// that never comes leaves the source fenced in committing; running the move
// again sends the commit again, which the destination answers the same way.
func (m *moveSource) commit(ctx context.Context) error {
	started := time.Now()
	reply, err := m.peer.call(ctx, "migrate-commit",
		[]string{"--txid", m.rec.TxID, "--epoch", strconv.FormatUint(m.rec.Epoch, 10)}, nil)
	if reply == nil || reply.Record == nil || reply.Record.State != dstRunning ||
		reply.Record.TxID != m.rec.TxID || reply.Record.Epoch != m.rec.Epoch {
		if err == nil {
			err = errors.New("the destination's reply does not say it runs this move")
		}
		return fmt.Errorf("commit on %s: %w", m.rec.Peer, err)
	}
	if err != nil {
		m.logf("warning: %s took the move over, but: %v", m.rec.Peer, err)
	}
	m.timing("commit", started)
	return m.rec.advance(m.path, srcCommitted)
}

// finish is what follows the commit on the source, and is safe to run
// again: the instance record marks the checkpoint stale, and the fenced hvi,
// which nothing will run again, is stopped.
func (m *moveSource) finish() error {
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	if state.Migration != nil && state.Migration.State != store.MigrationMigrated {
		state.Migration.State = store.MigrationMigrated
		state.Migration.MoveTxID = m.rec.TxID
		state.Migration.Peer = m.rec.Peer
		if err := m.s.SaveInstance(state); err != nil {
			return err
		}
	}
	if state.Status == "running" && state.PID > 0 && processIsAVMM(state.PID, state) {
		if err := stopInstanceIn(m.s, m.name, 10); err != nil {
			m.logf("warning: stopping the fenced VMM here: %v", err)
		}
	}
	m.logf("Moved %s to %s: it runs there at epoch %d. Its checkpoint here is stale", m.name, m.rec.Peer,
		m.rec.Epoch)
	if t := m.rec.Timings; t != nil {
		m.logf("Timings (ms): fence %d, transfer %d, restore paused %d, commit %d", t["fence"], t["transfer"],
			t["prepare"], t["commit"])
	}
	return nil
}

// sourceAbort is `hull migrate --abort`: before the commit, and only once
// the destination has confirmed that it is not running the VM and will
// refuse the commit, it ends the move and brings the instance back here.
func sourceAbort(ctx context.Context, s *store.Store, name string, w io.Writer, peer movePeer) error {
	unlock, err := lockMove(s.InstanceDir(name), 0)
	if errors.Is(err, errMoveBusy) {
		return fmt.Errorf("a `hull migrate` of %s is running; stop it first", name)
	}
	if err != nil {
		return err
	}
	defer unlock()
	path := s.InstanceMigrationRecord(name)
	rec, err := readMigrationRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("instance %s has no move on record", name)
	}
	if err != nil {
		return err
	}
	if rec.Role != roleSource {
		return fmt.Errorf("instance %s arrived by move %s; abort a move on its source host", name, rec.TxID)
	}
	switch rec.State {
	case srcCommitted:
		return fmt.Errorf("move %s committed: %s runs on %s at epoch %d. Moving it back is a new "+
			"`hull migrate` run on %s", rec.TxID, name, rec.Peer, rec.Epoch, rec.Peer)
	case migAborted:
		_, _ = fmt.Fprintf(w, "Move %s was already aborted\n", rec.TxID)
		return nil
	}
	state, err := s.GetInstance(name)
	if err != nil {
		return err
	}
	fenced := migrateStateOrder(roleSource, rec.State) >= migrateStateOrder(roleSource, srcFenced) ||
		fencedFor(state, rec.TxID)
	// From committing on the commit may have been delivered, so only the
	// destination's record of this very move, aborted, lets the source go.
	committing := rec.State == srcCommitting
	p := peerFor(rec, peer)
	reply, err := p.call(ctx, "migrate-abort", []string{"--txid", rec.TxID}, nil)
	switch {
	case err != nil && refusalCode(err) == codeRunning:
		return fmt.Errorf("the destination %s says move %s committed there and runs %s: it cannot be "+
			"aborted. `hull migrate %s --txid %s` records the commit here", rec.Peer, rec.TxID, name, name, rec.TxID)
	case err != nil && refusalCode(err) == codeUnknownTxID && committing:
		return rec.fail(path, fmt.Errorf("cannot abort move %s: %s has no record of it, but this host has "+
			"sent it the commit, which it may have run before its record was lost. The source stays fenced: "+
			"running %s here could put it on two hosts. Find out where it runs (docs/migration.md, "+
			"\"Operator recovery\")", rec.TxID, rec.Peer, name))
	case err != nil && refusalCode(err) == codeUnknownTxID:
		// No commit was ever sent, so a destination that never heard of
		// the move, or lost its reservation, cannot be running it.
		err = nil
	case err != nil && fenced:
		return rec.fail(path, fmt.Errorf("cannot abort move %s: %w\nWithout the destination's word that it "+
			"is not running %s and will refuse the commit, the source stays fenced: running it here could "+
			"put it on two hosts. Retry when %s answers", rec.TxID, err, name, rec.Peer))
	case err != nil:
		_, _ = fmt.Fprintf(w, "warning: %s did not confirm the abort (%v); it may still hold a reservation for %s, "+
			"which `hull migrate --abort %s` again releases\n", rec.Peer, err, name, rec.TxID)
	}
	if err == nil && fenced && reply.OK && !abortedThere(reply, rec) {
		return rec.fail(path, fmt.Errorf("cannot abort move %s: the reply of %s does not say it aborted "+
			"this move at epoch %d, so the source stays fenced. Retry", rec.TxID, rec.Peer, rec.Epoch))
	}
	if reply != nil && reply.OK && reply.Error != "" {
		_, _ = fmt.Fprintf(w, "warning: %s: %s\n", rec.Peer, reply.Error)
	}
	if err := rec.advance(path, migAborted); err != nil {
		return err
	}
	if !fenced {
		_, _ = fmt.Fprintf(w, "Aborted move %s; %s never stopped running here\n", rec.TxID, name)
		return nil
	}
	// The destination is out of it. The instance is the fenced checkpoint
	// again, and is restored from it here.
	if state.Migration != nil && state.Migration.TxID == rec.TxID {
		state.Migration.State = store.MigrationFenced
		if err := s.SaveInstance(state); err != nil {
			return err
		}
	}
	if state.Status == "running" && state.PID > 0 && processIsAVMM(state.PID, state) {
		if err := stopInstanceIn(s, name, 10); err != nil {
			return fmt.Errorf("aborted move %s, but stopping the fenced VMM failed: %w; `hull stop %s`, "+
				"then `hull restore %s`", rec.TxID, err, name, name)
		}
	}
	_, _ = fmt.Fprintf(w, "Aborted move %s; restoring %s here from its fenced checkpoint\n", rec.TxID, name)
	if err := localRestore(s, name); err != nil {
		return fmt.Errorf("aborted move %s, and %s stays stopped with its fenced checkpoint: %w. `hull "+
			"restore %s` brings it back once that is fixed", rec.TxID, name, err, name)
	}
	return nil
}

// abortedThere reports whether reply is the destination's record of move rec,
// aborted.
func abortedThere(reply *migrateReply, rec *migrationRecord) bool {
	r := reply.Record
	return r != nil && r.State == migAborted && r.TxID == rec.TxID && r.Epoch == rec.Epoch
}

// localRestore runs `hull restore --detach NAME` here, as a child for the
// same reason as on the destination.
var localRestore = func(s *store.Store, name string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	c := exec.Command(self, "--store-dir", s.RootDir(), "restore", "--detach", name)
	c.Stdout = &buf
	c.Stderr = &buf
	if err := c.Run(); err != nil {
		return fmt.Errorf("hull restore: %s", strings.TrimSpace(buf.String()))
	}
	return nil
}

// sourceStatus is `hull migrate --status`: the record here, what it means,
// and what the destination says.
func sourceStatus(ctx context.Context, s *store.Store, name string, w io.Writer, peer movePeer) error {
	rec, err := readMigrationRecord(s.InstanceMigrationRecord(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("instance %s has no move on record", name)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "Move %s of %s: %s, %s %s, epoch %d\n", rec.TxID, rec.Name, rec.State,
		map[string]string{roleSource: "to", roleDestination: "from"}[rec.Role], rec.Peer, rec.Epoch)
	for _, h := range rec.History {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", h.At.Local().Format("2006-01-02 15:04:05.000"), h.State)
	}
	if rec.Error != "" {
		_, _ = fmt.Fprintf(w, "  last error: %s\n", rec.Error)
	}
	if t := rec.Transfer; t != nil {
		_, _ = fmt.Fprintf(w, "  transfer: %s in %.1f s (%s/s)\n", humanBytes(t.Bytes), t.Seconds, humanBytes(t.BytesPerSec))
	}
	_, _ = fmt.Fprintf(w, "  %s\n", explainMoveState(rec))
	if rec.Role != roleSource || rec.Remote == nil {
		return nil
	}
	reply, err := peerFor(rec, peer).call(ctx, "migrate-status", []string{"--txid", rec.TxID}, nil)
	switch {
	case err != nil && refusalCode(err) == codeUnknownTxID:
		_, _ = fmt.Fprintf(w, "  destination %s: has no record of this move\n", rec.Peer)
	case err != nil:
		_, _ = fmt.Fprintf(w, "  destination %s: %v\n", rec.Peer, err)
	default:
		_, _ = fmt.Fprintf(w, "  destination %s: %s", rec.Peer, reply.Record.State)
		if reply.VM != "" {
			_, _ = fmt.Fprintf(w, ", VM %s", reply.VM)
		}
		if reply.Record.Error != "" {
			_, _ = fmt.Fprintf(w, ", last error: %s", reply.Record.Error)
		}
		_, _ = fmt.Fprintln(w)
	}
	return nil
}

// explainMoveState says what a record's state means for the instance.
func explainMoveState(rec *migrationRecord) string {
	if rec.Role == roleDestination {
		switch rec.State {
		case dstRunning:
			return "The move committed: the instance runs here, at this epoch."
		case migAborted:
			return "The move was aborted; nothing of it runs here."
		}
		return "The instance is arriving; only the source's commit runs it here."
	}
	switch rec.State {
	case srcPreflight, srcReserved:
		return "The instance still runs here; nothing was fenced."
	case srcCommitting:
		return "The source is fenced and has let the destination take over. Run the move again to " +
			"learn the commit, or abort it if the destination confirms it is not running."
	case srcCommitted:
		return fmt.Sprintf("The instance runs on %s at epoch %d. Its checkpoint here is stale and nothing "+
			"here may run it; moving it back is a new move from %s.", rec.Peer, rec.Epoch, rec.Peer)
	case migAborted:
		return "The move was aborted; the instance was handed back to this host."
	}
	return "The source is fenced and stays so until the move commits or is aborted."
}
