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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"

	"github.com/brig-sh/hull/pkg/store"
)

// The destination's half of a streamed move: `migrate-receive --stream`
// reads the stream on stdin, applies the disk blocks to a copy of the base
// that becomes the instance's disk, and relays hvi's components to an `hvi
// restore --from-stream` it starts for the instance. RAM rounds go to hvi
// as they arrive, so no RAM lands on this host's disk; the machine's state
// and the manifest are held until the stream's trailer has been checked
// and the last disk round is on disk, so hvi rebuilds the machine only from
// a stream hull has verified whole, over a disk that is complete. The move
// is verified, and the guest restored paused, when both hull and hvi have
// checked everything; only the source's commit resumes it.

// streamRestoreTimeout bounds a streamed restore: it lasts as long as the
// stream, pre-copy rounds included.
const streamRestoreTimeout = time.Hour

// streamRestore is the hvi restore a streamed receive feeds.
type streamRestore struct {
	in   io.WriteCloser
	wait func() error
	kill func()
}

// destStreamRestore starts the restore of the incoming instance name for
// move txid, reading hvi's stream from what is written to its in. A
// variable so the tests can stand in for hvi.
var destStreamRestore = startDestStreamRestore

// startDestStreamRestore runs `hull restore --detach --paused --move TXID
// NAME` as a child, as a move's restore always runs, with a pipe as its
// stdin, which the hvi it starts inherits. wait returns once hvi reports the
// guest restored paused, or why it did not.
func startDestStreamRestore(s *store.Store, name, txid string) (*streamRestore, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamRestoreTimeout)
	var out bytes.Buffer
	c := exec.CommandContext(ctx, self, "--store-dir", s.RootDir(), "restore", "--detach", "--paused",
		"--timeout", fmt.Sprint(int(streamRestoreTimeout.Seconds())), "--move", txid, name)
	c.Stdin, c.Stdout, c.Stderr = pr, &out, &out
	if err := c.Start(); err != nil {
		cancel()
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	_ = pr.Close()
	return &streamRestore{
		in: pw,
		wait: func() error {
			defer cancel()
			if err := c.Wait(); err != nil {
				text := strings.TrimSpace(out.String())
				if i := strings.LastIndex(text, "error: "); i >= 0 {
					text = text[i+len("error: "):]
				}
				return fmt.Errorf("hull restore %s: %s", name, text)
			}
			return nil
		},
		kill: func() {
			_ = pw.Close()
			if c.Process != nil {
				_ = c.Process.Kill()
			}
			_ = c.Wait()
			cancel()
			stopIncomingVMM(s, name)
		},
	}, nil
}

// stopIncomingVMM kills the hvi a failed streamed receive started, if its
// record names one.
func stopIncomingVMM(s *store.Store, name string) {
	state, err := s.GetInstance(name)
	if err != nil || state.PID <= 0 || !processIsAVMM(state.PID, state) {
		return
	}
	_ = syscall.Kill(state.PID, syscall.SIGKILL)
	for i := 0; i < 50 && syscall.Kill(state.PID, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	state.Status, state.PID = "stopped", 0
	_ = s.SaveInstance(state)
}

// streamReceiver is the handler a streamed receive reads the stream with.
type streamReceiver struct {
	s        *store.Store
	rec      *migrationRecord
	instDir  string
	root     string
	hvi      string
	diskPath string
	diskFile *os.File
	size     int64
	base     string

	restore *streamRestore
	// stage: 0 before profile.json, 1 after it, 2 in the RAM rounds, 3 in
	// the machine's state.
	stage int
	// pending is the component being read, when it is kept whole.
	pending *bytesBuffer
	// held are the state components, framed for hvi, held until the
	// trailer; files the small components kept beside the record.
	held      [][]byte
	heldBytes int64
	files     map[string][]byte
	manifest  []byte
	zero      []byte
	// restoreErr is why hvi's restore failed, once it did.
	restoreErr error
	// profileLine is hvi's header line of profile.json, held with it.
	profileLine []byte
	// unsynced is what was written to the disk since the last background
	// sync was asked for. syncing waits for the syncs in the background;
	// syncMu guards whether one runs, whether another is asked for, and
	// the first error one met.
	unsynced    int64
	syncing     sync.WaitGroup
	syncMu      sync.Mutex
	syncRunning bool
	syncAgain   bool
	syncErr     error
	// filesDone reports the documents written beside the record.
	filesDone chan error
	// applied is each disk round's batches as applied, by round.
	applied map[int]*diskRoundDigest
	// baseChecked reports the check of the base's digest, which runs while
	// the stream arrives.
	baseChecked chan error
}

// maybeSyncDisk asks for a sync of the disk in the background once
// spoolSyncEvery bytes were written to it since the last: the sync the
// guest waits for, after the stream, then has only the last round left.
func (r *streamReceiver) maybeSyncDisk(n int64) {
	r.unsynced += n
	if r.unsynced < spoolSyncEvery {
		return
	}
	r.syncDisk()
}

// syncDisk syncs the disk in the background. One asked for while another
// runs runs after it, so every write made before the call is covered once
// syncing is done.
func (r *streamReceiver) syncDisk() {
	r.unsynced = 0
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	if r.syncRunning {
		r.syncAgain = true
		return
	}
	r.syncRunning = true
	r.syncing.Add(1)
	f := r.diskFile
	go func() {
		defer r.syncing.Done()
		for {
			err := syncFile(f)
			r.syncMu.Lock()
			if err != nil && r.syncErr == nil {
				r.syncErr = err
			}
			if !r.syncAgain {
				r.syncRunning = false
				r.syncMu.Unlock()
				return
			}
			r.syncAgain = false
			r.syncMu.Unlock()
		}
	}()
}

// toHvi writes data to hvi's stdin. A write that fails means hvi stopped
// reading, which it does when it refuses the restore: its reason is the
// one to report.
func (r *streamReceiver) toHvi(what string, data []byte) error {
	if _, err := r.restore.in.Write(data); err != nil {
		_ = r.restore.in.Close()
		if werr := r.restore.wait(); werr != nil {
			r.restoreErr = werr
			return werr
		}
		return fmt.Errorf("hand %s to hvi: %w", what, err)
	}
	return nil
}

func (r *streamReceiver) header(h moveStreamHeader) error {
	switch {
	case h.TxID != r.rec.TxID:
		return fmt.Errorf("the stream is for move %s, not %s", h.TxID, r.rec.TxID)
	case h.Name != r.rec.Name:
		return fmt.Errorf("the stream is for %s, not %s", h.Name, r.rec.Name)
	case h.Epoch != r.rec.Epoch:
		return fmt.Errorf("the stream hands over epoch %d, not %d", h.Epoch, r.rec.Epoch)
	case !baseHeld(r.s, h.BaseSHA256):
		return &noBaseError{sum: h.BaseSHA256}
	}
	r.base = h.BaseSHA256
	r.size = h.DiskSize
	// The base is trusted by its name only until this check, which reads
	// it while the stream's rounds arrive and is waited for before hvi
	// gets the machine.
	r.baseChecked = make(chan error, 1)
	go func(path, want string) {
		got, _, err := diskDigestFile(path)
		if err == nil && got != want {
			err = fmt.Errorf("its blocks hash to %s", got)
		}
		r.baseChecked <- err
	}(r.s.BasePath(h.BaseSHA256), h.BaseSHA256)
	// A fresh copy of the base on every attempt: whatever an earlier one
	// applied is gone with it.
	if err := os.MkdirAll(filepath.Dir(r.diskPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(r.diskPath)
	if _, err := copyDisk(r.s.BasePath(h.BaseSHA256), r.diskPath); err != nil {
		return fmt.Errorf("copy the base: %w", err)
	}
	if err := os.Chmod(r.diskPath, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(r.diskPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	r.diskFile = f
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != h.DiskSize {
		return fmt.Errorf("the base is %d bytes and the guest's disk %d", info.Size(), h.DiskSize)
	}
	r.zero = make([]byte, diskBlockSize)
	return nil
}

// checkBase waits for the check of the base's digest. A base whose blocks
// do not hash to its name is set aside, so the source sends it again, as
// for a base this host never held.
func (r *streamReceiver) checkBase() error {
	if r.baseChecked == nil {
		return errors.New("the stream named no base")
	}
	err := <-r.baseChecked
	r.baseChecked = nil
	if err == nil {
		r.rec.Stream.step("base checked")
		return nil
	}
	path := r.s.BasePath(r.base)
	aside := fmt.Sprintf("%s.bad-%d", path, time.Now().UnixNano())
	if rerr := os.Rename(path, aside); rerr != nil {
		return fmt.Errorf("base %s is corrupt (%v), and setting it aside failed: %w", r.base, err, rerr)
	}
	log.Warnf("Base %s is corrupt (%v); set aside as %s", r.base, err, aside)
	return &noBaseError{sum: r.base}
}

// checkDiskRounds holds the disk rounds the destination applied to the
// trailer's account of them: every round, with as many blocks and batches,
// and the same digest of its batches' digests. For the rounds from hvi's
// dirty blocks it then holds that account to what hvi itself recorded in
// disk.json, which the manifest covers: each round hvi read went out
// whole, as hvi streamed it, and the final round, read after the fence,
// is the one after the last listed, at the generation hvi captured the
// disk at, so no write of the guest was left out. A replay sends the disk
// by comparison, which disk.json does not describe.
func checkDiskRounds(replay bool, t *streamDiskRounds, applied map[int]*diskRoundDigest, diskJSON []byte) error {
	if t == nil {
		if len(applied) > 0 {
			return errors.New("the stream's trailer gives no account of its disk rounds")
		}
		return nil
	}
	sent := map[int]streamDiskRound{}
	for _, want := range t.Rounds {
		if _, dup := sent[want.Round]; dup {
			return fmt.Errorf("the trailer lists disk round %d twice", want.Round)
		}
		sent[want.Round] = want
		got := (&diskRoundDigest{}).done()
		if a := applied[want.Round]; a != nil {
			got = a.done()
			if got.Final != want.Final {
				return fmt.Errorf("disk round %d arrived final %v, the trailer says %v", want.Round, got.Final, want.Final)
			}
		}
		if got.Blocks != want.Blocks || got.Batches != want.Batches || got.SentSHA256 != want.SentSHA256 {
			return fmt.Errorf("disk round %d: %d blocks in %d batches arrived (digest %s); the source sent %d "+
				"in %d (digest %s)", want.Round, got.Blocks, got.Batches, got.SentSHA256, want.Blocks,
				want.Batches, want.SentSHA256)
		}
	}
	for round := range applied {
		if _, ok := sent[round]; !ok {
			return fmt.Errorf("disk round %d arrived, and the trailer does not list it", round)
		}
	}
	var final *streamDiskRound
	fromHvi := false
	for i, r := range t.Rounds {
		fromHvi = fromHvi || r.From == "hvi"
		if r.Final {
			if final != nil {
				return errors.New("the trailer lists two final disk rounds")
			}
			final = &t.Rounds[i]
		}
	}
	if replay || !fromHvi {
		return nil
	}
	var doc struct {
		Generation uint64 `json:"generation"`
		Rounds     []struct {
			Round        int     `json:"round"`
			Generation   uint64  `json:"generation"`
			Blocks       int64   `json:"blocks"`
			BitmapSHA256 string  `json:"bitmap_sha256"`
			SentSHA256   *string `json:"sent_sha256"`
		} `json:"rounds"`
	}
	if diskJSON == nil {
		return errors.New("the disk rounds came from hvi, and its stream has no disk.json to hold them to")
	}
	if err := json.Unmarshal(diskJSON, &doc); err != nil {
		return fmt.Errorf("hvi's disk.json: %w", err)
	}
	last := -1
	listed := map[int]bool{}
	for _, l := range doc.Rounds {
		listed[l.Round] = true
		last = max(last, l.Round)
		s, ok := sent[l.Round]
		switch {
		case !ok:
			return fmt.Errorf("hvi read disk round %d, and the stream does not carry it", l.Round)
		case s.From != "hvi":
			continue
		case l.SentSHA256 == nil || *l.SentSHA256 != s.SentSHA256:
			return fmt.Errorf("hvi recorded disk round %d as sent with digest %v; the stream carried %s",
				l.Round, l.SentSHA256, s.SentSHA256)
		case l.Generation != s.Generation || l.Blocks != s.Blocks ||
			(l.BitmapSHA256 != "" && l.BitmapSHA256 != s.BitmapSHA256):
			return fmt.Errorf("hvi recorded disk round %d at generation %d with %d blocks; the source sent it "+
				"as read at %d with %d", l.Round, l.Generation, l.Blocks, s.Generation, s.Blocks)
		}
	}
	for _, s := range t.Rounds {
		if s.From == "hvi" && !s.Final && !listed[s.Round] {
			return fmt.Errorf("disk round %d is not one hvi recorded before the fence", s.Round)
		}
	}
	switch {
	case final == nil || final.From != "hvi":
		return errors.New("the stream has no final disk round from hvi: the guest's last writes would be missing")
	case final.Round != last+1:
		return fmt.Errorf("the final disk round is %d; hvi's last recorded round was %d", final.Round, last)
	case final.Generation != doc.Generation:
		return fmt.Errorf("the final disk round was read at generation %d, and hvi captured the disk at %d: "+
			"a write of the guest is missing", final.Generation, doc.Generation)
	}
	return nil
}

// noBaseError is a stream whose base this store does not hold.
type noBaseError struct{ sum string }

func (e *noBaseError) Error() string {
	return fmt.Sprintf("this host does not hold base %s: `hull migrate-prepare` sends it", e.sum)
}

func (r *streamReceiver) disk(d diskBatchHeader, raw []byte) error {
	if r.applied == nil {
		r.applied = map[int]*diskRoundDigest{}
	}
	round := r.applied[d.Round]
	if round == nil {
		round = &diskRoundDigest{round: streamDiskRound{Round: d.Round, Final: d.Final}}
		r.applied[d.Round] = round
	} else if round.round.Final != d.Final {
		return fmt.Errorf("disk round %d is sent as both final and not", d.Round)
	}
	round.add(d.Count, d.SHA256)
	cur := make([]byte, diskBlockSize)
	for i := int64(0); i < d.Count; i++ {
		index := d.First + i
		lo := i * diskBlockSize
		block := raw[lo:min(lo+diskBlockSize, int64(len(raw)))]
		off := index * diskBlockSize
		if isZero(block) {
			// A zero block over one that is zero already (a hole of the
			// base) is left alone, so the disk stays sparse.
			n, err := r.diskFile.ReadAt(cur[:len(block)], off)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if isZero(cur[:n]) {
				continue
			}
		}
		if _, err := r.diskFile.WriteAt(block, off); err != nil {
			return fmt.Errorf("write disk block %d: %w", index, err)
		}
		r.maybeSyncDisk(int64(len(block)))
	}
	return nil
}

func (r *streamReceiver) component(line []byte, hdr hviComponentHeader, body io.Reader) error {
	name := hdr.Component
	switch {
	case r.stage == 0:
		if name != "profile.json" {
			return fmt.Errorf("hvi's stream starts with %s, not profile.json", name)
		}
		if hdr.Size > 1<<20 {
			return fmt.Errorf("profile.json is %d bytes", hdr.Size)
		}
		r.profileLine = append([]byte{}, line...)
		r.pending = &bytesBuffer{}
		_, err := io.Copy(r.pending, body)
		return err
	case isRAMRound(name):
		if r.stage == 3 {
			return fmt.Errorf("hvi's %s comes after the machine's state", name)
		}
		r.stage = 2
		if err := r.toHvi(name, append(append([]byte{}, line...), '\n')); err != nil {
			return err
		}
		buf := make([]byte, 1<<20)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				if werr := r.toHvi(name, buf[:n]); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	r.stage = 3
	if r.heldBytes+hdr.Size > spooledStateMax {
		return fmt.Errorf("hvi's %s of %d bytes is not a RAM round and too large to hold", name, hdr.Size)
	}
	r.heldBytes += hdr.Size
	r.pending = &bytesBuffer{}
	_, err := io.Copy(r.pending, body)
	if err == nil {
		r.held = append(r.held, append(append(append([]byte{}, line...), '\n'), r.pending.b...))
	}
	return err
}

func (r *streamReceiver) componentVerified(hdr hviComponentHeader) error {
	name := hdr.Component
	if isRAMRound(name) {
		return nil
	}
	if r.manifest != nil {
		return fmt.Errorf("hvi's %s comes after its manifest", name)
	}
	r.files[name] = append(r.files[name], r.pending.b...)
	switch name {
	case "manifest.json":
		r.manifest = r.pending.b
	case "profile.json":
		if r.stage != 0 {
			return errors.New("hvi's stream carries profile.json twice")
		}
		if err := checkStreamProfile(r.pending.b, r.rec.Spec.Profile); err != nil {
			return err
		}
		if err := writeStreamIncomingInstance(r.s, r.rec, r.hvi, r.root, r.diskPath, r.base); err != nil {
			return err
		}
		restore, err := destStreamRestore(r.s, r.rec.Name, r.rec.TxID)
		if err != nil {
			return fmt.Errorf("start hvi restore: %w", err)
		}
		r.restore = restore
		r.stage = 1
		// The disk rounds that run with the guest running are all in: sync
		// them while the RAM rounds arrive, so the sync after the stream
		// has only the last round to write.
		if r.diskFile != nil {
			r.syncDisk()
		}
		if err := r.toHvi(name, append(append(r.profileLine, '\n'), r.pending.b...)); err != nil {
			return err
		}
	}
	r.pending = nil
	return nil
}

// checkStreamProfile refuses a stream whose machine profile is not the one
// the move's spec names, as checkBundleProfile does for a bundle.
func checkStreamProfile(data []byte, want string) error {
	dir, err := os.MkdirTemp("", "hull-profile-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), data, 0o600); err != nil {
		return err
	}
	return checkBundleProfile(dir, want)
}

// finish hands hvi the state it held, once the stream is verified whole
// and the last disk round is written, and waits for hvi to report the
// guest restored paused. hvi reads the disk through the page cache, so
// the sync that makes the last round durable runs while hvi rebuilds the
// machine rather than before it; finish returns once both are done. The
// documents kept beside the record are written meanwhile too.
func (r *streamReceiver) finish() error {
	if r.restore == nil || r.manifest == nil {
		return errors.New("the stream carried no hvi checkpoint")
	}
	r.rec.Stream.step("trailer read")
	r.syncDisk()
	r.filesDone = make(chan error, 1)
	go func() {
		for name, data := range r.files {
			if err := writeFileSynced(filepath.Join(r.rec.BundleDir, name), data); err != nil {
				r.filesDone <- err
				return
			}
		}
		r.filesDone <- nil
	}()
	for _, c := range r.held {
		if err := r.toHvi("the machine's state", c); err != nil {
			return err
		}
	}
	if err := r.restore.in.Close(); err != nil {
		return err
	}
	if err := r.restore.wait(); err != nil {
		r.restoreErr = err
		return err
	}
	r.rec.Stream.step("hvi restored the guest paused")
	r.syncing.Wait()
	r.rec.Stream.step("disk synced")
	if r.syncErr != nil {
		return fmt.Errorf("sync the disk: %w", r.syncErr)
	}
	err := r.diskFile.Close()
	r.diskFile = nil
	if err != nil {
		return err
	}
	if err := <-r.filesDone; err != nil {
		return fmt.Errorf("keep hvi's documents: %w", err)
	}
	r.filesDone = nil
	return nil
}

// abandon ends a receive that failed: hvi's input is cut, and the restore
// and any hvi it started are stopped.
func (r *streamReceiver) abandon() {
	if r.baseChecked != nil {
		<-r.baseChecked
	}
	r.syncing.Wait()
	if r.filesDone != nil {
		<-r.filesDone
	}
	if r.diskFile != nil {
		_ = r.diskFile.Close()
	}
	if r.restore != nil {
		r.restore.kill()
	}
}

// destStreamCmdLine is the command line a streamed move records for the
// instance it brings in: a restore from a stream, with the destination's
// own disk, sockets and checkpoint root.
func destStreamCmdLine(s *store.Store, rec *migrationRecord, hvi, root, disk string) []string {
	argv := destCmdLine(s, rec, hvi, root)
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "--from":
			argv[i], argv[i+1] = hviFromStreamFlag, hviFromStreamStdin
		case "--disk":
			argv[i+1] = disk
		}
	}
	return argv
}

// writeStreamIncomingInstance writes the incoming instance's state.json for
// a streamed move: stopped, its checkpoint the move's, which only the
// move's restore may bring up.
func writeStreamIncomingInstance(s *store.Store, rec *migrationRecord, hvi, root, disk, base string) error {
	rec.BundleDir = filepath.Join(root, rec.TxID)
	if err := os.MkdirAll(rec.BundleDir, 0o700); err != nil {
		return err
	}
	rec.Disk = &migrateDisk{Path: disk}
	spec := rec.Spec
	state := &store.InstanceState{
		ID:          rec.Name,
		ImageDigest: spec.ImageDigest,
		Status:      "stopped",
		LogFile:     s.InstanceLogFile(rec.Name),
		CmdLine:     destStreamCmdLine(s, rec, hvi, root, disk),
		BundleDir:   s.InstanceBundleDir(rec.Name),
		MAC:         spec.MAC,
		IP:          spec.IP,
		Backend:     string(hypervisors.HviVmm),
		Profile:     spec.Profile,
		StartTime:   time.Now(),
		Migration: &store.MigrationState{State: store.MigrationIncoming, BundleDir: rec.BundleDir,
			TxID: rec.TxID, MoveTxID: rec.TxID, Peer: rec.Peer},
		Base: &store.BaseState{SHA256: base},
	}
	return s.SaveInstance(state)
}

// destReceiveStream is `migrate-receive --stream`.
func destReceiveStream(s *store.Store, txid, name string, in io.Reader) *migrateReply {
	rec, path, unlock, no := destRecord(s, txid)
	if no != nil {
		return no
	}
	if unlock != nil {
		defer unlock()
	}
	switch {
	case rec.State == migAborted:
		return refuseWith(codeAborted, fmt.Errorf("move %s was aborted here", txid), rec)
	case rec.Name != name:
		return refuseWith(codeFailed, fmt.Errorf("move %s is for %s here, not %s", txid, rec.Name, name), rec)
	case migrateStateOrder(roleDestination, rec.State) >= migrateStateOrder(roleDestination, dstVerified):
		return &migrateReply{OK: true, Record: rec}
	}
	// An earlier attempt may have left its hvi running.
	stopIncomingVMM(s, name)
	if err := rec.advance(path, dstReceiving); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	hvi, err := destHviPath(rec.Spec.Hvi)
	if err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	instDir := s.InstanceDir(name)
	root, err := hviCheckpointRoot(instDir)
	if err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	r := &streamReceiver{s: s, rec: rec, instDir: instDir, root: root, hvi: hvi,
		diskPath: filepath.Join(instDir, "bundle", "rootfs.ext4"), files: map[string][]byte{}}
	rec.Stream = &migrateStreamStats{}
	started := time.Now()
	res, err := readMoveStream(in, r)
	if err == nil {
		err = r.checkBase()
	}
	if err == nil {
		err = checkDiskRounds(res.Header.Replay, res.Trailer.Disk, r.applied, r.files["disk.json"])
	}
	if err == nil {
		err = r.finish()
	}
	if err != nil {
		r.abandon()
		if r.restoreErr != nil {
			return refuseWith(codeRestoreRefused, rec.fail(path, fmt.Errorf("restore: %w", r.restoreErr)), rec)
		}
		var nb *noBaseError
		if errors.As(err, &nb) {
			return refuseWith(codeNoBase, rec.fail(path, err), rec)
		}
		return refuseWith(codeVerifyFailed, rec.fail(path, fmt.Errorf("receive: %w", err)), rec)
	}
	took := time.Since(started)
	rec.ManifestSHA256 = sha256Hex(r.manifest)
	rec.BaseSHA256 = r.base
	rec.Transport = transportStream
	rec.Disk = &migrateDisk{Path: r.diskPath, Size: r.size, DataBytes: res.Disk.Bytes}
	rec.Transfer = &migrateTransfer{Bytes: res.Bytes, Seconds: took.Seconds(), StartedAt: started.UTC(),
		BytesPerSec: perSec(res.Bytes, took.Seconds())}
	rec.Stream = &migrateStreamStats{Chain: res.Trailer.Chain, DiskBytes: res.Disk.Bytes,
		WireBytes: res.Bytes, Seconds: took.Seconds(), HviReply: res.Trailer.HviReply, Steps: rec.Stream.Steps}
	rec.Verify = fmt.Sprintf("stream: %d hvi components and %d disk blocks in %d batches checked; hvi restored "+
		"the guest paused", len(res.Components), res.Disk.Blocks, res.Disk.Batches)
	if state, err := s.GetInstance(name); err == nil && state.Migration != nil {
		state.Migration.ManifestSHA256 = rec.ManifestSHA256
		if err := s.SaveInstance(state); err != nil {
			return refuseWith(codeFailed, rec.fail(path, err), rec)
		}
	}
	// One write for the three: each is true once hvi has restored the
	// guest from a stream hull checked whole.
	if err := rec.advanceThrough(path, dstReceived, dstVerified, dstRestoredPaused); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	return &migrateReply{OK: true, Record: rec, VM: store.MigrationRestoredPaused}
}
