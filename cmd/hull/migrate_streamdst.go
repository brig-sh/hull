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
	"bufio"
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
	"syscall"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"golang.org/x/sys/unix"

	"github.com/brig-sh/hull/pkg/store"
)

// The destination's half of a streamed move: `migrate-receive --stream`
// reads the stream on stdin, makes a synced copy of the base the
// instance's disk, and hands hvi's stream, as hvi wrote it, to an `hvi
// restore --from-stream - --disk <the copy> --disk-base-sha256 <base>` it
// starts for the instance. hvi writes the disk rounds into the copy and
// the RAM rounds into guest RAM as they arrive, so no RAM lands on this
// host's disk, and checks every frame, the manifest and the disk rounds
// before it builds the machine. The move is verified, and the guest
// restored paused, when hull has checked the stream's trailer and hvi has
// restored; only the source's commit resumes it.

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
	path     string
	instDir  string
	root     string
	hvi      string
	diskPath string
	size     int64
	base     string

	restore *streamRestore
	// profileLine and pending are hvi's profile.json, held until it is
	// checked; pending is then each document being read, kept for the
	// record.
	profileLine []byte
	pending     *bytesBuffer
	// files are the documents kept beside the record, docBytes their size.
	files    map[string][]byte
	docBytes int64
	manifest []byte
	// restoreErr is why hvi's restore failed, once it did.
	restoreErr error
	// filesDone reports the documents written beside the record.
	filesDone chan error
	// baseChecked reports the check of the base's digest, which runs while
	// the stream arrives.
	baseChecked chan error
	// out takes a session's lines; nil outside a session.
	out *sessionOut
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
	// it while the stream's rounds arrive and is waited for before the
	// move is verified.
	r.baseChecked = make(chan error, 1)
	go func(path, want string) {
		r.baseChecked <- checkBaseDigest(path, want)
	}(r.s.BasePath(h.BaseSHA256), h.BaseSHA256)
	// A fresh copy of the base on every attempt: whatever an earlier one
	// applied is gone with it.
	if err := os.MkdirAll(filepath.Dir(r.diskPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(r.diskPath)
	if !takeSpareBase(r.s, h.BaseSHA256, r.diskPath) {
		if _, err := copyDisk(r.s.BasePath(h.BaseSHA256), r.diskPath); err != nil {
			return fmt.Errorf("copy the base: %w", err)
		}
	}
	if err := os.Chmod(r.diskPath, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(r.diskPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && info.Size() != h.DiskSize {
		err = fmt.Errorf("the base is %d bytes and the guest's disk %d", info.Size(), h.DiskSize)
	}
	if err == nil {
		// hvi syncs the disk after the rounds it writes and waits for that
		// sync before the guest may run, so a copy whose writeback is
		// still under way is paid for in the guest's downtime: seconds,
		// on a busy Pi. The copy is synced here, before the guest stops.
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// The source waits for this before hvi's migrate fences the guest: a
	// refusal from here on cannot be one the header would have told.
	if h.Attempt != "" {
		r.rec.StreamAccepted = h.Attempt
		if err := writeMigrationRecord(r.path, r.rec); err != nil {
			return err
		}
		r.out.event(&migrateReply{OK: true, Event: eventAccepted, Detail: h.Attempt})
	}
	return nil
}

// checkBaseDigest checks that the base at path hashes to want. A base is
// read-only and never written, so a check that passed is recorded beside it
// (<base>.checked: the digest, and the file's size, change time and inode)
// and stands until any of them changes; a move with little to send then never waits for a hash of
// a 1.6 GB base.
func checkBaseDigest(path, want string) error {
	stamp, err := baseStamp(path)
	if err != nil {
		return err
	}
	stamp = want + " " + stamp
	if old, err := os.ReadFile(path + ".checked"); err == nil && string(old) == stamp {
		return nil
	}
	got, _, err := diskDigestFile(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("its blocks hash to %s", got)
	}
	_ = os.WriteFile(path+".checked", []byte(stamp), 0o600)
	return nil
}

// baseStamp is what a base's check is recorded against: its size, change
// time and inode.
func baseStamp(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("size %d ctime %d.%09d ino %d\n", st.Size, st.Ctim.Sec, st.Ctim.Nsec, st.Ino), nil
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

// noBaseError is a stream whose base this store does not hold.
type noBaseError struct{ sum string }

func (e *noBaseError) Error() string {
	return fmt.Sprintf("this host does not hold base %s: `hull migrate-prepare` sends it", e.sum)
}

// component takes one of hvi's frames. profile.json, first, is held until
// it is checked and hvi's restore is started for it; every frame after it
// goes to hvi as it arrives, rounds and documents alike, since hvi checks
// each frame's sha256, the manifest over all of them and the disk rounds
// against disk.json before anything runs, and writes the disk rounds into
// the copy of the base itself. The documents are kept for the record too.
func (r *streamReceiver) component(line []byte, hdr hviComponentHeader, body io.Reader) error {
	name := hdr.Component
	if r.manifest != nil {
		return fmt.Errorf("hvi's %s comes after its manifest", name)
	}
	if r.restore == nil {
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
	}
	if err := r.toHvi(name, append(append([]byte{}, line...), '\n')); err != nil {
		return err
	}
	r.pending = nil
	if !isRound(name) {
		if r.docBytes+hdr.Size > spooledStateMax {
			return fmt.Errorf("hvi's %s of %d bytes is not a round and too large to keep", name, hdr.Size)
		}
		r.docBytes += hdr.Size
		r.pending = &bytesBuffer{}
	}
	buf := make([]byte, 1<<20)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if werr := r.toHvi(name, buf[:n]); werr != nil {
				return werr
			}
			if r.pending != nil {
				_, _ = r.pending.Write(buf[:n])
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

func (r *streamReceiver) componentVerified(hdr hviComponentHeader) error {
	name := hdr.Component
	if isRound(name) {
		return nil
	}
	if name == "profile.json" && r.restore != nil {
		return errors.New("hvi's stream carries profile.json twice")
	}
	r.files[name] = append(r.files[name], r.pending.b...)
	switch name {
	case "manifest.json":
		r.manifest = r.pending.b
		r.rec.Stream.step("manifest read")
	case "profile.json":
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

// finish ends hvi's input once the stream is verified whole, and waits for
// hvi to report the guest restored paused: by then hvi has checked every
// frame, the manifest and the disk rounds, and synced the disk. The check
// of the base runs meanwhile, and the documents kept beside the record are
// written, unsynced.
func (r *streamReceiver) finish() error {
	if r.restore == nil || r.manifest == nil {
		return errors.New("the stream carried no hvi checkpoint")
	}
	r.rec.Stream.step("trailer read")
	r.filesDone = make(chan error, 1)
	go func() {
		// Kept for the record only: hvi has the machine from the stream,
		// so they are not synced, which on a busy disk would cost the
		// stopped guest a sync each.
		for name, data := range r.files {
			if err := os.WriteFile(filepath.Join(r.rec.BundleDir, name), data, 0o600); err != nil {
				r.filesDone <- err
				return
			}
		}
		r.filesDone <- nil
	}()
	if err := r.restore.in.Close(); err != nil {
		return err
	}
	if err := r.restore.wait(); err != nil {
		r.restoreErr = err
		return err
	}
	r.rec.Stream.step("hvi restored the guest paused")
	if err := r.checkBase(); err != nil {
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
	if r.filesDone != nil {
		<-r.filesDone
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

// destReceiveStream is `migrate-receive --stream`, and with out, which
// takes the lines of a session, `--stream --session`: then it says when it
// took the header and what it has read, says verified, and waits for the
// source's commit after the trailer (sessionCommitWait), which it runs as
// migrate-commit would. Its reply is then the commit's, or, when the source
// ends the stream or sends nothing, the verified one again.
func destReceiveStream(s *store.Store, txid, name string, in io.Reader, out *sessionOut) *migrateReply {
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
	rec.StreamAccepted = ""
	r := &streamReceiver{s: s, rec: rec, path: path, instDir: instDir, root: root, hvi: hvi,
		diskPath: filepath.Join(instDir, "bundle", "rootfs.ext4"), files: map[string][]byte{}, out: out}
	rec.Stream = &migrateStreamStats{}
	started := time.Now()
	if out != nil {
		acks := newAckReader(in, out)
		defer acks.close()
		in = acks
	}
	res, rest, err := readMoveStreamOpen(in, r, out != nil)
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
	diskBytes := hviReplyDiskBytes(res.Trailer.HviReply)
	rec.Disk = &migrateDisk{Path: r.diskPath, Size: r.size, DataBytes: diskBytes}
	rec.Transfer = &migrateTransfer{Bytes: res.Bytes, Seconds: took.Seconds(), StartedAt: started.UTC(),
		BytesPerSec: perSec(res.Bytes, took.Seconds())}
	rec.Stream = &migrateStreamStats{Chain: res.Trailer.Chain, DiskBytes: diskBytes,
		WireBytes: res.Bytes, Seconds: took.Seconds(), HviReply: res.Trailer.HviReply, Steps: rec.Stream.Steps}
	rec.Verify = fmt.Sprintf("stream: %d hvi components checked; hvi checked them against its manifest, "+
		"wrote the disk rounds onto base %s and restored the guest paused", len(res.Components), digestHead(r.base))
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
	// On the record with the commit's write; the reply carries it now.
	rec.Stream.step("verified recorded")
	verified := &migrateReply{OK: true, Record: rec, VM: store.MigrationRestoredPaused}
	if out == nil {
		return verified
	}
	out.event(&migrateReply{OK: true, Event: eventVerified, Record: rec, VM: store.MigrationRestoredPaused})
	return destSessionCommit(s, rec, path, rest, verified)
}

// destSessionCommit waits, after a session's verified line, for what the
// source sends after the trailer, and runs the commit it sends.
func destSessionCommit(s *store.Store, rec *migrationRecord, path string, rest *bufio.Reader,
	verified *migrateReply) *migrateReply {
	type read struct {
		c   *moveStreamCommit
		err error
	}
	got := make(chan read, 1)
	go func() {
		c, err := readMoveCommit(rest)
		got <- read{c, err}
	}()
	var k read
	select {
	case k = <-got:
	case <-time.After(sessionCommitWait):
		log.Warnf("Move %s: no commit came in the session within %s; it stays restored paused here",
			rec.TxID, sessionCommitWait)
		return verified
	}
	switch {
	case k.err != nil:
		// Left verified: the source learns the commit's fate by
		// migrate-commit, as outside a session.
		return refuseWith(codeFailed, fmt.Errorf("after the trailer: %w", k.err), rec)
	case k.c == nil:
		return verified
	case k.c.TxID != rec.TxID:
		return refuseWith(codeFailed, fmt.Errorf("the session of move %s carries the commit of %s", rec.TxID,
			k.c.TxID), rec)
	}
	return destCommitLocked(s, rec, path, k.c.Epoch)
}

// hviReplyDiskBytes is the bytes of disk blocks hvi's reply to migrate says
// its disk rounds carried, 0 when it does not say.
func hviReplyDiskBytes(reply json.RawMessage) int64 {
	var r struct {
		Stats struct {
			DiskRawBytes int64 `json:"disk_raw_bytes"`
		} `json:"stats"`
	}
	if json.Unmarshal(reply, &r) != nil {
		return 0
	}
	return r.Stats.DiskRawBytes
}
