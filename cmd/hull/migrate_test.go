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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// The whole move, both sides in one process: it commits, the source is
// fenced before the destination may run, the destination takes the epoch,
// and the source's checkpoint ends up stale with its VMM stopped.
func TestMoveCommits(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	if err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer); err != nil {
		t.Fatal(err)
	}
	src := f.srcRecord(t)
	if src.State != srcCommitted || src.Epoch != 1 {
		t.Fatalf("source record %s epoch %d", src.State, src.Epoch)
	}
	var states []string
	for _, h := range src.History {
		states = append(states, h.State)
	}
	if want := []string{srcPreflight, srcReserved, srcFenced, srcTransferring, srcTransferred, srcCommitting,
		srcCommitted}; !slices.Equal(states, want) {
		t.Errorf("source went %v, want %v", states, want)
	}
	if src.Transfer == nil || src.Transfer.Bytes == 0 || src.Disk.SHA256 == "" ||
		src.Disk.DataBytes == 0 || src.Disk.DataBytes >= fixtureDiskSize {
		t.Errorf("transfer %+v disk %+v", src.Transfer, src.Disk)
	}
	dst := f.dstRecord(t, src.TxID)
	if dst.State != dstRunning || dst.Epoch != 1 || dst.Disk.SHA256 != src.Disk.SHA256 {
		t.Errorf("destination record %+v", dst)
	}
	// The fence: a checkpoint with then=fence, the move's txid and the
	// epoch it hands over. The source was never resumed.
	var fence map[string]any
	for _, r := range f.vm.stub.seen() {
		if r["op"] == "checkpoint" {
			fence = r
		}
		if r["op"] == "resume" {
			t.Error("the source was resumed")
		}
	}
	if fence == nil || fence["then"] != "fence" || fence["txid"] != src.TxID || fence["epoch"] != float64(1) {
		t.Errorf("fence request %v", fence)
	}
	if !f.vm.stopped {
		t.Error("the source's fenced VMM was not stopped after the commit")
	}
	srcState := mustInstance(t, f.src, f.name)
	if srcState.Migration.State != store.MigrationMigrated || srcState.Migration.MoveTxID != src.TxID {
		t.Errorf("source instance migration %+v", srcState.Migration)
	}
	if err := restorableMigration(srcState.Migration); err == nil {
		t.Error("the stale source checkpoint is restorable")
	}
	dstState := mustInstance(t, f.dst, f.name)
	if dstState.OwnerEpoch != 1 || dstState.MAC != "52:54:00:12:34:56" || dstState.Migration.State != store.MigrationRestored {
		t.Errorf("destination instance epoch %d mac %s migration %+v", dstState.OwnerEpoch, dstState.MAC, dstState.Migration)
	}
	// The destination's command line is its own.
	for _, want := range []string{f.dst.InstanceAgentSocket(f.name), dst.BundleDir, "--net-mac"} {
		if !slices.Contains(dstState.CmdLine, want) {
			t.Errorf("destination command line %v has no %s", dstState.CmdLine, want)
		}
	}
	if disk, _ := flagValue(dstState.CmdLine, "--disk"); !strings.HasPrefix(disk, f.dst.RootDir()) {
		t.Errorf("destination disk %s is not in its store", disk)
	}
	assertSameContent(t, f.disk, dst.Disk.Path)
	// Moving again from the stale source is refused; a plain `hull migrate`
	// re-run with the txid is a no-op.
	if err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer); err == nil {
		t.Error("a new move of the stale source was allowed")
	}
	opt := f.options(false)
	opt.txid = src.TxID
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Errorf("re-running a committed move: %v", err)
	}
}

// The commit's reply is lost: the destination runs, the source stays
// fenced in committing and is never resumed, and running the move again
// sends the commit again and records it.
func TestMoveLostCommitAcknowledgement(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	peer.drop["migrate-commit"] = 1
	err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer)
	if err == nil || !strings.Contains(err.Error(), "stays fenced") {
		t.Fatalf("err = %v", err)
	}
	src := f.srcRecord(t)
	if src.State != srcCommitting {
		t.Fatalf("source is %s, want committing", src.State)
	}
	if dst := f.dstRecord(t, src.TxID); dst.State != dstRunning {
		t.Fatalf("destination is %s, want running", dst.State)
	}
	if slices.Contains(f.vm.ops(), "resume") {
		t.Fatal("the source was resumed")
	}
	// An abort now must be refused: the destination says it runs.
	if err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer); err == nil ||
		!strings.Contains(err.Error(), "cannot be aborted") {
		t.Errorf("abort after a lost acknowledgement: %v", err)
	}
	if f.srcRecord(t).State != srcCommitting {
		t.Error("the refused abort changed the source record")
	}
	// A re-run without --txid says how to resume.
	if err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer); err == nil ||
		!strings.Contains(err.Error(), "--txid "+src.TxID) {
		t.Errorf("re-run without the txid: %v", err)
	}
	opt := f.options(false)
	opt.txid = src.TxID
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatal(err)
	}
	if got := f.srcRecord(t).State; got != srcCommitted {
		t.Errorf("after the retry the source is %s", got)
	}
	if peer.called("migrate-commit") != 2 {
		t.Errorf("commit sent %d times, want 2", peer.called("migrate-commit"))
	}
}

// The destination's hvi cannot restore: the move stops before the commit,
// with the source fenced; an abort, once the destination confirms, frees
// the destination and restores the instance here.
func TestMoveRestoreRefusedThenAbort(t *testing.T) {
	f := newMoveFixture(t, false)
	peer := newLocalPeer(f.dst)
	restored := 0
	localRestore = func(_ *store.Store, name string) error {
		restored++
		st := mustInstance(t, f.src, name)
		return restorableMigration(st.Migration)
	}
	err := sourceMigrate(context.Background(), f.src, f.name, f.options(true), peer)
	if err == nil || !strings.Contains(err.Error(), "could not restore") {
		t.Fatalf("err = %v", err)
	}
	src := f.srcRecord(t)
	if src.State != srcTransferred {
		t.Fatalf("source is %s, want transferred", src.State)
	}
	dst := f.dstRecord(t, src.TxID)
	if dst.State != dstVerified || !strings.Contains(dst.Error, "no restore subcommand") {
		t.Fatalf("destination %s, error %q", dst.State, dst.Error)
	}
	// A plain restore of the source is refused while the move holds it.
	if err := restorableMigration(mustInstance(t, f.src, f.name).Migration); err == nil {
		t.Error("the source's checkpoint is restorable while a move holds it")
	}
	if err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer); err != nil {
		t.Fatal(err)
	}
	if got := f.srcRecord(t).State; got != migAborted {
		t.Errorf("source is %s", got)
	}
	if restored != 1 {
		t.Errorf("the source was restored %d times, want 1", restored)
	}
	if !f.vm.stopped {
		t.Error("the fenced VMM was not stopped before the restore")
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); !os.IsNotExist(err) {
		t.Errorf("the destination kept the instance: %v", err)
	}
	if rec := f.dstRecord(t, src.TxID); rec.State != migAborted {
		t.Errorf("tombstone %s", rec.State)
	}
	// A commit that arrives late is refused.
	if r := destCommit(f.dst, src.TxID, src.Epoch); r.OK || r.Code != codeAborted {
		t.Errorf("late commit: %+v", r)
	}
	// The aborted move's record does not hold the instance: a new move is
	// refused only because the fixture's VMM is stopped now (the stub
	// restore here does not start one), not by the old move.
	if err := sourceMigrate(context.Background(), f.src, f.name, f.options(true), newLocalPeer(shortStore(t))); err == nil {
		t.Error("expected the fixture's stopped VMM to refuse a new move")
	} else if strings.Contains(err.Error(), "in move") {
		t.Errorf("an aborted move blocks a new one: %v", err)
	}
}

// Without the destination's answer an abort after the fence refuses to
// bring the source back.
func TestMoveAbortWithoutAnAnswerKeepsTheSourceFenced(t *testing.T) {
	f := newMoveFixture(t, false)
	peer := newLocalPeer(f.dst)
	_ = sourceMigrate(context.Background(), f.src, f.name, f.options(true), peer)
	restored := 0
	localRestore = func(*store.Store, string) error { restored++; return nil }
	peer.down = true
	err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer)
	if err == nil || !strings.Contains(err.Error(), "stays fenced") {
		t.Fatalf("err = %v", err)
	}
	if restored != 0 || f.vm.stopped {
		t.Error("the source was touched without the destination's word")
	}
	if got := f.srcRecord(t); got.State != srcTransferred || got.Error == "" {
		t.Errorf("source record %s %q", got.State, got.Error)
	}
}

// A destination that answers running refuses the abort, whatever the
// source's record says.
func TestMoveAbortRefusedByARunningDestination(t *testing.T) {
	f := newMoveFixture(t, false)
	peer := newLocalPeer(f.dst)
	_ = sourceMigrate(context.Background(), f.src, f.name, f.options(true), peer)
	peer.answer["migrate-abort"] = &migrateReply{OK: false, Code: codeRunning, Error: "it runs here"}
	restored := 0
	localRestore = func(*store.Store, string) error { restored++; return nil }
	if err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer); err == nil ||
		!strings.Contains(err.Error(), "cannot be aborted") {
		t.Fatalf("err = %v", err)
	}
	if restored != 0 || f.vm.stopped || f.srcRecord(t).State == migAborted {
		t.Error("the abort went ahead against a running destination")
	}
}

// After the commit, an abort is refused.
func TestMoveAbortAfterCommit(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	if err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer); err != nil {
		t.Fatal(err)
	}
	err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer)
	if err == nil || !strings.Contains(err.Error(), "new `hull migrate`") {
		t.Errorf("err = %v", err)
	}
	if peer.called("migrate-abort") != 0 {
		t.Error("the destination was asked to abort a committed move")
	}
	// The destination refuses it too.
	if r := destAbort(f.dst, f.srcRecord(t).TxID); r.OK || r.Code != codeRunning {
		t.Errorf("destination abort of a running move: %+v", r)
	}
}

// An abort before the fence leaves the source running and frees the
// destination's reservation.
func TestMoveAbortBeforeTheFence(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	// The move stops right after the reservation: the capture fails, and
	// hvi runs the guest on.
	f.vm.mu.Lock()
	f.vm.failCheckpoint = true
	f.vm.mu.Unlock()
	err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer)
	if err == nil || !strings.Contains(err.Error(), "still runs here") {
		t.Fatalf("err = %v", err)
	}
	src := f.srcRecord(t)
	if src.State != srcReserved {
		t.Fatalf("source %s", src.State)
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); err != nil {
		t.Fatalf("no reservation on the destination: %v", err)
	}
	if err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); !os.IsNotExist(err) {
		t.Errorf("the reservation is still there: %v", err)
	}
	if f.vm.stopped {
		t.Error("an abort before the fence stopped the source")
	}
}

// A transfer cut off midway leaves the source in transferring; the re-run
// sends again and goes on. The destination's restore is never run twice.
func TestMoveTransferRetry(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	// The reserve goes through; the stream's connection fails.
	failing := &failVerbPeer{localPeer: peer, verb: "migrate-receive", times: 1, skipFirst: 1}
	err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), failing)
	if err == nil {
		t.Fatal("the cut transfer succeeded")
	}
	src := f.srcRecord(t)
	if src.State != srcTransferring {
		t.Fatalf("source %s", src.State)
	}
	opt := f.options(false)
	opt.txid = src.TxID
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatal(err)
	}
	if f.srcRecord(t).State != srcCommitted || f.restores != 1 {
		t.Errorf("source %s, %d restores", f.srcRecord(t).State, f.restores)
	}
}

// failVerbPeer fails one verb a number of times without running it, after
// letting skipFirst calls of it through.
type failVerbPeer struct {
	*localPeer
	verb      string
	times     int
	skipFirst int
}

func (p *failVerbPeer) call(ctx context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error) {
	if verb == p.verb {
		if p.skipFirst > 0 {
			p.skipFirst--
		} else if p.times > 0 {
			p.times--
			p.down = true
			defer func() { p.down = false }()
		}
	}
	return p.localPeer.call(ctx, verb, args, stdin)
}

// A disk written after the fence is not sent.
func TestMoveRefusesADiskChangedAfterTheFence(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	failing := &failVerbPeer{localPeer: peer, verb: "migrate-receive", times: 1, skipFirst: 1}
	_ = sourceMigrate(context.Background(), f.src, f.name, f.options(false), failing)
	if f.srcRecord(t).State != srcTransferring {
		t.Fatalf("source %s", f.srcRecord(t).State)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.disk, later, later); err != nil {
		t.Fatal(err)
	}
	opt := f.options(false)
	opt.txid = f.srcRecord(t).TxID
	err := sourceMigrate(context.Background(), f.src, f.name, opt, peer)
	if err == nil || !strings.Contains(err.Error(), "changed after the fence") {
		t.Errorf("err = %v", err)
	}
}

// What a move holds is out of reach of the plain verbs on both hosts.
func TestMoveGuards(t *testing.T) {
	f := newMoveFixture(t, false)
	peer := newLocalPeer(f.dst)
	_ = sourceMigrate(context.Background(), f.src, f.name, f.options(true), peer)
	// Source: checkpoint, resume, rm.
	if err := hviCheckpointInstance(f.src, mustInstance(t, f.src, f.name), hviThenContinue, "", time.Second); err == nil ||
		!strings.Contains(err.Error(), "being moved") {
		t.Errorf("checkpoint of a source in a move: %v", err)
	}
	if err := refuseDuringMove(f.src, f.name, "resume"); err == nil {
		t.Error("resume of a source in a move was allowed")
	}
	if err := removeInstanceIn(f.src, f.name, true); err == nil || !strings.Contains(err.Error(), "being moved") {
		t.Errorf("rm of a source in a move: %v", err)
	}
	// Destination: the received instance restores only through its move,
	// and only paused.
	st := mustInstance(t, f.dst, f.name)
	if err := checkMoveRestore(f.dst, st, hviRestoreOptions{}); err == nil {
		t.Error("a plain restore of an incoming instance was allowed")
	}
	txid := f.srcRecord(t).TxID
	if err := checkMoveRestore(f.dst, st, hviRestoreOptions{move: txid}); err == nil {
		t.Error("a move's restore without --paused was allowed")
	}
	if err := checkMoveRestore(f.dst, st, hviRestoreOptions{move: strings.Repeat("e", 32), paused: true}); err == nil {
		t.Error("another move's restore was allowed")
	}
	if err := checkMoveRestore(f.dst, st, hviRestoreOptions{move: txid, paused: true}); err != nil {
		t.Errorf("the move's own restore: %v", err)
	}
	if err := refuseDuringMove(f.dst, f.name, "resume"); err == nil || !strings.Contains(err.Error(), "arriving") {
		t.Errorf("resume of an incoming instance: %v", err)
	}
}

// A dry run changes nothing on either side.
func TestMoveDryRun(t *testing.T) {
	f := newMoveFixture(t, true)
	peer := newLocalPeer(f.dst)
	opt := f.options(false)
	opt.dryRun = true
	var out bytes.Buffer
	opt.out = &out
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Dry run") {
		t.Errorf("output: %s", out.String())
	}
	if _, err := os.Stat(f.src.InstanceMigrationRecord(f.name)); !os.IsNotExist(err) {
		t.Error("a dry run wrote a record")
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); !os.IsNotExist(err) {
		t.Error("a dry run reserved the name")
	}
	if len(f.vm.ops()) > 0 && slices.Contains(f.vm.ops(), "checkpoint") {
		t.Error("a dry run fenced the source")
	}
	if peer.called("migrate-probe") != 1 || len(peer.calls) != 1 {
		t.Errorf("calls %v", peer.calls)
	}
}

// Preflight refuses what a move cannot carry before anything changes.
func TestMovePreflightRefusals(t *testing.T) {
	f := newMoveFixture(t, false)
	peer := newLocalPeer(f.dst)
	// No restore on this hvi, and no --allow-no-restore.
	err := sourceMigrate(context.Background(), f.src, f.name, f.options(false), peer)
	if err == nil || !strings.Contains(err.Error(), "no restore subcommand") {
		t.Errorf("err = %v", err)
	}
	if len(peer.calls) != 0 {
		t.Errorf("the destination was called: %v", peer.calls)
	}
	// A name that is taken there.
	if _, err := f.dst.CreateInstance(f.name); err != nil {
		t.Fatal(err)
	}
	err = sourceMigrate(context.Background(), f.src, f.name, f.options(true), peer)
	if err == nil || !strings.Contains(err.Error(), "already exists there") {
		t.Errorf("err = %v", err)
	}
	if slices.Contains(f.vm.ops(), "checkpoint") {
		t.Error("the source was fenced for a destination that refused")
	}
	// A bad target.
	opt := f.options(true)
	opt.to = "-oProxyCommand=touch /tmp/x"
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err == nil {
		t.Error("an option as a target was accepted")
	}
}

// The whole move over the ssh transport, with this test binary standing in
// for ssh and for the destination's hull: the remote command line goes
// through a shell, the destination's verbs run in another process against
// the second store, and its restore is a real `hull restore --move` child,
// which the fake hvi (no restore subcommand) refuses. That is today's
// outcome with a real hvi too, until hvi has restore.
func TestMoveOverFakeSSH(t *testing.T) {
	f := newMoveFixture(t, false)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The destination's store is used as it is: on macOS hull would
	// otherwise mount a case-sensitive image over a temporary directory.
	if err := os.WriteFile(filepath.Join(f.dst.RootDir(), ".case-sensitive-apfs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(migrateSSHEnv, self)
	t.Setenv(fakeSSHEnv, "1")
	t.Setenv("DO_NOT_TRACK", "1")
	opt := f.options(true)
	opt.remote.Hull = self
	opt.remote.Store = f.dst.RootDir()
	var progress bytes.Buffer
	opt.out = &progress

	err = sourceMigrate(context.Background(), f.src, f.name, opt, nil)
	// The refusal is the real `hull restore`'s, from the other process.
	if err == nil || !strings.Contains(err.Error(), "hull restore vm1: cannot restore vm1: the hvi it ran") {
		t.Fatalf("err = %v\n%s", err, progress.String())
	}
	src := f.srcRecord(t)
	if src.State != srcTransferred || src.Transfer == nil || src.Disk.SHA256 == "" {
		t.Fatalf("source %s %+v", src.State, src.Transfer)
	}
	dst := f.dstRecord(t, src.TxID)
	if dst.State != dstVerified || dst.Verify != "ok: test bundle" || dst.Disk.SHA256 != src.Disk.SHA256 {
		t.Fatalf("destination %+v", dst)
	}
	assertSameContent(t, f.disk, dst.Disk.Path)
	for _, name := range []string{"manifest.json", "ram.bin", "disk.json"} {
		assertSameContent(t, filepath.Join(src.BundleDir, name), filepath.Join(dst.BundleDir, name))
	}
	if st := mustInstance(t, f.dst, f.name); st.Migration.State != store.MigrationIncoming || st.Status != "stopped" {
		t.Errorf("destination instance %s %+v", st.Status, st.Migration)
	}

	var status bytes.Buffer
	if err := sourceStatus(context.Background(), f.src, f.name, &status, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "destination pm@dest: verified") {
		t.Errorf("status:\n%s", status.String())
	}

	if err := sourceAbort(context.Background(), f.src, f.name, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.srcRecord(t).State; got != migAborted {
		t.Errorf("source %s", got)
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); !os.IsNotExist(err) {
		t.Errorf("the destination kept the instance: %v", err)
	}
	if rec := f.dstRecord(t, src.TxID); rec.State != migAborted {
		t.Errorf("destination tombstone %s", rec.State)
	}
}

// Moving back is a new move from where the instance runs now, to the host
// it left, once the stale copy there is removed; the owner epoch goes on
// counting.
func TestMoveThereAndBack(t *testing.T) {
	f := newMoveFixture(t, true)
	ctx := context.Background()
	if err := sourceMigrate(ctx, f.src, f.name, f.options(false), newLocalPeer(f.dst)); err != nil {
		t.Fatal(err)
	}
	if got := mustInstance(t, f.dst, f.name).OwnerEpoch; got != 1 {
		t.Fatalf("epoch after the first move %d", got)
	}
	back := f.options(false)
	back.to, back.remote.Target = "pm@src", "pm@src"
	err := sourceMigrate(ctx, f.dst, f.name, back, newLocalPeer(f.src))
	if err == nil || !strings.Contains(err.Error(), "`hull rm vm1` there") {
		t.Fatalf("moving back over the stale copy: %v", err)
	}
	if err := removeInstanceIn(f.src, f.name, false); err != nil {
		t.Fatalf("rm of the stale copy: %v", err)
	}
	if err := sourceMigrate(ctx, f.dst, f.name, back, newLocalPeer(f.src)); err != nil {
		t.Fatal(err)
	}
	home := mustInstance(t, f.src, f.name)
	if home.OwnerEpoch != 2 || home.Migration.State != store.MigrationRestored {
		t.Errorf("back home: epoch %d, migration %+v", home.OwnerEpoch, home.Migration)
	}
	away, err := readMigrationRecord(f.dst.InstanceMigrationRecord(f.name))
	if err != nil || away.Role != roleSource || away.State != srcCommitted || away.Epoch != 2 {
		t.Errorf("the second move's source record: %+v %v", away, err)
	}
}
