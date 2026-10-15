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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// The source's half of a streamed move (docs/migration.md, "The streamed
// move"): one ssh session to `migrate-receive --stream`, whose stdin
// carries hvi's migrate stream as hvi writes it, and a trailer. hvi sends
// the rounds while the guest runs (each disk round, the blocks the guest
// wrote since the one before, ahead of its RAM round), syncs the disk,
// pauses the guest, sends the final disk and RAM rounds, the machine's
// state and the manifest, and fences the guest once the manifest is in the
// socket's buffer. hull relays it and keeps a copy in the spool; it drives
// no disk rounds of its own.

// streamSender writes one streamed move into sw.
type streamSender struct {
	m     *moveSource
	state *store.InstanceState
	sw    *moveStreamWriter
	size  int64
	stats *migrateStreamStats
	// spoolDir is where hvi's stream is kept on this host.
	spoolDir string
	// manifest is hvi's manifest.json, once relayed.
	manifest []byte
	// link is what sw writes to; its error is the destination's failure.
	link *softLink
	// sealDone reports the spool's seal, which runs on behind the move
	// once the stream is out; recorded the record of the fence, which the
	// commit waits for, and recordedAt when it was made.
	sealDone   chan error
	recorded   chan error
	recordedAt time.Time
	// sealGate opens when the spool's seal may sync it.
	sealGate chan struct{}
	// ctx is the move's; attempt names this attempt, and callDone is
	// closed once the receive's ssh session has ended.
	ctx      context.Context
	attempt  string
	callDone chan struct{}
	// restream is a second stream of a guest hvi fenced already: one that
	// ends early leaves it fenced, never running on.
	restream bool
	// prev is the record of the stream of this move's earlier attempt,
	// when there was one.
	prev *migrateStreamStats
	// accepted is closed when a session's destination says it took this
	// attempt's header; nil outside a session.
	accepted chan struct{}
}

func (ss *streamSender) progress(format string, args ...any) {
	ss.m.logf("move: "+format, args...)
}

// endRound records a round and prints it.
func (ss *streamSender) endRound(r streamRound, started time.Time) {
	r.Seconds = time.Since(started).Seconds()
	ss.stats.Rounds = append(ss.stats.Rounds, r)
	switch r.Kind {
	case "disk":
		ss.stats.DiskWireBytes += r.WireBytes
	case "ram":
		ss.stats.RAMBytes += r.Bytes
	default:
		ss.stats.StateBytes += r.Bytes
		return
	}
	ss.progress("%s: %s in %.2f s, %s/s", r.Name, humanBytes(r.Bytes), r.Seconds,
		humanBytes(perSec(r.Bytes, r.Seconds)))
}

// perSec is n per second over secs.
func perSec(n int64, secs float64) int64 { return int64(float64(n) / max(secs, 1e-3)) }

// hviMigrateStats is what hull reads of the stats in hvi's reply to
// migrate (hvi docs/checkpoint.md, "Live moves").
type hviMigrateStats struct {
	Rounds []struct {
		Round   int   `json:"round"`
		InPause bool  `json:"in_pause"`
		Pages   int64 `json:"pages"`
		Bytes   int64 `json:"bytes"`
		Ms      int64 `json:"ms"`
	} `json:"rounds"`
	StopReason string `json:"stop_reason"`
	PausedMs   int64  `json:"paused_ms"`
	// GateMs is the wait for the I/O threads at the pause, which hvi
	// since ba9b2d2 runs the vCPUs through: the guest stops after it.
	GateMs     int64 `json:"gate_ms"`
	DiskRounds []struct {
		Name     string `json:"name"`
		Round    int    `json:"round"`
		InPause  bool   `json:"in_pause"`
		Blocks   int64  `json:"blocks"`
		RawBytes int64  `json:"raw_bytes"`
		Bytes    int64  `json:"bytes"`
		Ms       int64  `json:"ms"`
	} `json:"disk_rounds"`
	DiskRawBytes int64 `json:"disk_raw_bytes"`
	DiskSyncMs   int64 `json:"disk_sync_ms"`
	PauseSyncMs  int64 `json:"pause_sync_ms"`
	RoundsEnd    int64 `json:"rounds_end_unix_ms"`
	PrecopyEnd   int64 `json:"precopy_end_unix_ms"`
	SealedAt     int64 `json:"sealed_at_unix_ms"`
}

// takeHviStats puts what hvi's reply says of the rounds on the record: each
// disk round's blocks and raw bytes, which round was taken paused, the
// syncs, and when the rounds with the guest running ended, the pause came
// and the stream was sealed, by this host's clock. These are closer to the
// pause than what hull sees of the frames. It prints them on one line.
func (ss *streamSender) takeHviStats(reply *hviReply) {
	var st hviMigrateStats
	if reply == nil || json.Unmarshal(reply.Stats, &st) != nil {
		if !ss.stats.PausedAt.IsZero() {
			ss.stats.SealedMs = time.Since(ss.stats.PausedAt).Milliseconds()
		}
		return
	}
	paused := map[string]bool{}
	for _, r := range st.Rounds {
		paused[fmt.Sprintf("ram-round-%d", r.Round)] = r.InPause
	}
	disk := map[string]int{}
	for i, d := range st.DiskRounds {
		disk[d.Name] = i
		if d.InPause {
			ss.stats.FinalDiskBytes, ss.stats.FinalDiskMs = d.RawBytes, d.Ms
		}
	}
	for i := range ss.stats.Rounds {
		r := &ss.stats.Rounds[i]
		switch r.Kind {
		case "ram":
			r.Final = paused[r.Name]
		case "disk":
			if j, ok := disk[r.Name]; ok {
				d := st.DiskRounds[j]
				r.Blocks, r.Bytes, r.Final = d.Blocks, d.RawBytes, d.InPause
			}
		}
	}
	ss.stats.DiskBytes = st.DiskRawBytes
	ss.stats.StopReason = st.StopReason
	ss.stats.HviPausedMs, ss.stats.DiskSyncMs, ss.stats.PauseSyncMs = st.PausedMs, st.DiskSyncMs, st.PauseSyncMs
	if st.RoundsEnd > 0 {
		ss.stats.RoundsEndAt = time.UnixMilli(st.RoundsEnd).UTC()
	}
	if st.PrecopyEnd > 0 {
		// The pause: hvi takes it after the sync that follows the
		// rounds, with the guest running through that sync, and the
		// guest stops once the I/O threads are held.
		stop := st.PrecopyEnd + max(st.GateMs, 0)
		ss.stats.PausedAt = time.UnixMilli(stop).UTC()
		ss.stats.HviGateMs = st.GateMs
		if st.SealedAt >= stop {
			ss.stats.SealedMs = st.SealedAt - stop
		}
	} else if !ss.stats.PausedAt.IsZero() {
		ss.stats.SealedMs = time.Since(ss.stats.PausedAt).Milliseconds()
	}
	var blocks []string
	for _, d := range st.DiskRounds {
		blocks = append(blocks, fmt.Sprint(d.Blocks))
	}
	line := fmt.Sprintf("hvi: %d RAM rounds (%s)", len(st.Rounds), st.StopReason)
	if len(blocks) > 0 {
		line += fmt.Sprintf(", disk rounds of %s blocks (%s, %s on the wire)", strings.Join(blocks, ", "),
			humanBytes(st.DiskRawBytes), humanBytes(ss.stats.DiskWireBytes))
	}
	ss.progress("%s; paused %d ms, disk sync %d ms before the pause and %d ms in it", line, st.PausedMs,
		st.DiskSyncMs, st.PauseSyncMs)
}

// spoolWriter keeps hvi's stream on this host as hvi wrote it, and the
// small components beside it as files. Nothing syncs it while the stream
// runs: on a busy disk a sync of the spool held up the synced records the
// stopped guest waits for. The seal syncs it, behind the move. Every
// spoolSyncEvery bytes its writeback is started (startWriteback), with no
// wait: without that a 2 GiB stream left so many dirty pages that the
// kernel throttled every write on the host, the move's included.
type spoolWriter struct {
	dir string
	f   *os.File
	cw  *countingWriter
	bw  *bufio.Writer
	// started is how far the writeback has been started.
	started int64
}

// writeback starts the writeback of what was written since the last call,
// once that is spoolSyncEvery bytes or more.
func (sp *spoolWriter) writeback() {
	if sp.cw.n+int64(sp.bw.Buffered())-sp.started < spoolSyncEvery {
		return
	}
	if sp.bw.Flush() != nil {
		return // the seal reports it
	}
	startWriteback(sp.f, sp.started, sp.cw.n-sp.started)
	sp.started = sp.cw.n
}

// spoolSyncEvery is how much the source writes to its spool between the
// starts of its writeback, and a destination to the incoming disk between
// the syncs it runs in the background while the stream goes on.
const spoolSyncEvery = 32 << 20

func newSpool(dir string) (*spoolWriter, error) {
	emptyDir(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, spoolStream), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cw := &countingWriter{w: f}
	return &spoolWriter{dir: dir, f: f, cw: cw, bw: bufio.NewWriterSize(cw, 1<<20)}, nil
}

// seal flushes and syncs the stream, writes the small components and the
// reply, and syncs the directory: from here an abort can restore from it.
func (sp *spoolWriter) seal(files map[string][]byte, reply []byte) error {
	if err := sp.bw.Flush(); err != nil {
		return err
	}
	if err := syncFile(sp.f); err != nil {
		return err
	}
	if err := sp.f.Close(); err != nil {
		return err
	}
	for name, data := range files {
		if err := writeFileSynced(filepath.Join(sp.dir, name), data); err != nil {
			return err
		}
	}
	if err := writeFileSynced(filepath.Join(sp.dir, spoolSealed), append(reply, '\n')); err != nil {
		return err
	}
	return syncDir(sp.dir)
}

func (sp *spoolWriter) close() {
	if sp.f != nil {
		_ = sp.f.Close()
	}
}

// syncFile syncs f; a variable so a test can hold a sync and see what
// waits for it.
var syncFile = func(f *os.File) error { return f.Sync() }

// writeFileSynced writes data to path and syncs it.
func writeFileSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// progressEvery is how often a long round prints how far it is.
const progressEvery = 2 * time.Second

// isRAMRound reports whether an hvi component is one of the RAM rounds.
func isRAMRound(name string) bool { return strings.HasPrefix(name, "ram-round-") }

// isDiskRound reports whether an hvi component is one of the disk rounds:
// disk-round-0 and on, and disk-round-final, the one hvi takes paused.
func isDiskRound(name string) bool { return strings.HasPrefix(name, "disk-round-") }

// isRound reports whether an hvi component is a round, RAM or disk: bulk
// data that hull relays and spools but never holds whole. Every other
// component is a document, small enough to keep.
func isRound(name string) bool { return isRAMRound(name) || isDiskRound(name) }

// spooledStateMax bounds the documents (components that are not rounds)
// kept in memory to be written beside the spool. A variable so a test can
// show a round larger than it is not held to it.
var spooledStateMax int64 = 64 << 20

// roundFolder folds hvi's frames into rounds for the record and the
// progress lines: hvi cuts a RAM or disk round into frames of about 1 MiB,
// all named after the round.
type roundFolder struct {
	ss      *streamSender
	cur     *streamRound
	started time.Time
	printed time.Time
	lastRAM time.Time
	ramSeen bool
	// replay is a stream relayed from the spool: its frames arrive long
	// after the pause, which they say nothing of.
	replay bool
}

// begin starts the frame of component name, which ends the round before
// it when it is another one.
func (f *roundFolder) begin(name string) {
	if f.cur != nil && f.cur.Name == name {
		return
	}
	f.flush()
	now := time.Now()
	kind := "state"
	switch {
	case isRAMRound(name):
		kind = "ram"
		f.lastRAM, f.ramSeen = now, true
	case isDiskRound(name):
		kind = "disk"
	case f.ramSeen && !f.replay && f.ss.stats.PausedAt.IsZero():
		// The first document after the RAM rounds: the round before it
		// was the last, which hvi takes with the guest paused.
		f.ss.stats.PausedAt = f.lastRAM.UTC()
	}
	f.cur = &streamRound{Kind: kind, Name: name}
	f.started, f.printed = now, now
}

// add counts a frame of bytes, wire bytes on the wire.
func (f *roundFolder) add(bytes, wire int64) {
	f.cur.Bytes += bytes
	f.cur.WireBytes += wire
	if now := time.Now(); f.cur.Kind != "state" && now.Sub(f.printed) >= progressEvery {
		f.printed = now
		f.ss.progress("%s: %s so far, %s/s", f.cur.Name, humanBytes(f.cur.Bytes),
			humanBytes(perSec(f.cur.WireBytes, now.Sub(f.started).Seconds())))
	}
}

// flush records the round in progress.
func (f *roundFolder) flush() {
	if f.cur != nil {
		f.ss.endRound(*f.cur, f.started)
		f.cur = nil
	}
}

// relayFrame relays one of hvi's frames into the stream, and into also
// when that is not nil, and folds it into its round.
func (ss *streamSender) relayFrame(f *roundFolder, line []byte, hdr hviComponentHeader, body io.Reader,
	also io.Writer) error {
	f.begin(hdr.Component)
	before := ss.sw.written()
	if err := ss.sw.component(line, hdr, body, also); err != nil {
		return err
	}
	f.add(hdr.Size, ss.sw.written()-before)
	return nil
}

// relayHvi runs hvi's migrate op and relays its frames into the stream and
// the spool. It returns hvi's reply line once hvi has sealed the stream and
// fenced the guest. A stream that ends before its manifest without a fence
// is an *hviRanOnError: hvi refused it, or it broke and hvi says the guest
// is not fenced (fenceAfterBreak).
func (ss *streamSender) relayHvi(sp *spoolWriter) (json.RawMessage, map[string][]byte, error) {
	m := ss.m
	// In a session the window holds back what hull relays, and what hull
	// has read from hvi and not yet relayed is behind the window, where
	// the window does not count it: at hvi's seal the guest waits for it
	// too. A reader no larger than the session's writes keeps that short;
	// the rest of it is in hvi's side of the socket.
	size := 1 << 20
	if ss.link.win != nil {
		size = sessionWriteBuffer
	}
	conn, br, err := hviStreamOpSize(ss.state.ControlSock, map[string]any{"op": "migrate", "then": hviThenFence,
		"txid": m.rec.TxID, "epoch": m.rec.Epoch}, hviStreamIdle, size)
	if err != nil {
		if ss.restream {
			return nil, nil, err
		}
		// hvi never had the request, so the guest is as it was.
		return nil, nil, &hviRanOnError{err: err}
	}
	closed := false
	closeConn := func() {
		if !closed {
			closed = true
			_ = conn.Close()
		}
	}
	defer closeConn()
	files := map[string][]byte{}
	var kept int64
	folder := &roundFolder{ss: ss}
	// The wait for hvi drops to its own bound once the paused part begins
	// (hviPausedBound); 0 until then.
	var pausedBound, lastRun time.Duration
	ic, _ := conn.(*idleConn)
	reply, line, err := readHviComponents("migrate", br, func(line []byte, hdr hviComponentHeader, body io.Reader) error {
		if relayFrameTestHook != nil {
			if err := relayFrameTestHook(hdr.Component); err != nil {
				return err
			}
		}
		if ss.manifest != nil {
			return fmt.Errorf("hvi's %s comes after its manifest", hdr.Component)
		}
		// A second stream of a fenced guest has no paused part: hvi gives
		// it only the 30 s it gives any reader that takes nothing.
		if pausedBound == 0 && ic != nil && !ss.restream && pausedPartBegins(folder, hdr.Component) {
			// The round before is flushed, so it is among those timed.
			folder.begin(hdr.Component)
			lastRun = lastRunningRound(ss.stats.Rounds, !isRound(hdr.Component))
			pausedBound = hviPausedBound(lastRun)
			ic.setIdle(pausedBound)
		}
		if _, err := sp.bw.Write(append(append([]byte{}, line...), '\n')); err != nil {
			return err
		}
		var also io.Writer = sp.bw
		var keep *bytesBuffer
		if !isRound(hdr.Component) {
			if kept+hdr.Size > spooledStateMax {
				return fmt.Errorf("hvi's %s of %d bytes is not a round and too large to keep", hdr.Component, hdr.Size)
			}
			kept += hdr.Size
			keep = &bytesBuffer{}
			also = io.MultiWriter(sp.bw, keep)
		}
		if err := ss.relayFrame(folder, line, hdr, body, also); err != nil {
			return err
		}
		sp.writeback()
		if keep != nil {
			files[hdr.Component] = append(files[hdr.Component], keep.b...)
			if hdr.Component == "manifest.json" {
				ss.manifest = keep.b
				// hvi fences the guest once the manifest is in the
				// socket's buffer. From here the spool is the guest: every
				// byte goes to the file now, so a hull that dies before the
				// seal leaves a stream a re-run or an abort can check whole
				// (verifySpool).
				if err := sp.bw.Flush(); err != nil {
					return fmt.Errorf("spool the stream: %w", err)
				}
			}
		}
		return nil
	})
	folder.flush()
	if pausedBound > 0 && errors.Is(err, os.ErrDeadlineExceeded) {
		err = fmt.Errorf("hvi sent nothing for %.1f s in the part of its stream it writes with the guest "+
			"paused, longer than it gives that part (%.1f s: %g s and twice the last round with the guest "+
			"running, %.2f s): %w", pausedBound.Seconds(), (pausedBound - hviPausedMargin).Seconds(),
			hviPausedFloor.Seconds(), lastRun.Seconds(), err)
	}
	var refused *hviRefusal
	switch {
	case err != nil && ss.manifest == nil && ss.restream:
		// The guest was fenced before this stream began, and stays so.
		return nil, nil, fmt.Errorf("hvi's second stream of the fenced guest ended before its manifest: %w", err)
	case err != nil && ss.manifest == nil && errors.As(err, &refused):
		// hvi's own answer, in place of a frame: nothing was fenced.
		return nil, nil, &hviRanOnError{err: err}
	case err != nil && ss.manifest == nil:
		closeConn()
		return nil, nil, ss.fenceAfterBreak(err)
	case err != nil:
		// The manifest came whole, so hvi has fenced the guest: what is
		// lost is its reply, or the stream broke after it. A re-run or an
		// abort checks the spool frame by frame (verifySpool).
		return nil, nil, fmt.Errorf("hvi fenced the guest, and then: %w", err)
	case ss.manifest == nil:
		return nil, nil, errors.New("hvi sealed its migrate stream without a manifest.json")
	}
	ss.takeHviStats(reply)
	return line, files, nil
}

// hvi gives the part of a move's stream it writes with the guest paused,
// from disk-round-final to the manifest, a budget: hviPausedFloor plus
// twice the last round taken with the guest running, its RAM round and the
// disk round before it (hvi src/checkpoint/server.rs, paused_budget). Past
// it every write fails, and the guest runs on. hvi cannot say so in the
// middle of a frame, and it leaves the connection open, so hull, waiting
// for the rest of the frame, waited out the whole hviStreamIdle of 2
// minutes before it asked hvi whether the guest runs. Once the paused part
// begins hull waits for a byte only as long as that budget, with a margin.

// hviPausedFloor is hvi's STREAM_PAUSED_FLOOR. A variable so a test need
// not wait it out.
var hviPausedFloor = 10 * time.Second

// hviPausedMargin is what hull adds to hvi's budget for the paused part.
// hull times a round from its first frame to the next round's, as the
// frames reach it, which is longer than hvi's time for it by the sync and
// the pause before disk-round-final, and shorter by at most what sat in
// the socket's buffers when the round began.
var hviPausedMargin = 5 * time.Second

// pausedPartBegins reports whether the frame of component, about to be
// relayed, is the first of the paused part: disk-round-final, or, in a
// stream with no disk rounds, the first document after the RAM rounds
// (whose last round was the paused one).
func pausedPartBegins(f *roundFolder, component string) bool {
	return component == "disk-round-final" || (f.ramSeen && !isRound(component))
}

// lastRunningRound is how long hull took to relay the last round taken
// with the guest running: the last RAM round of rounds, or the one before
// it when the last was taken paused, and the last disk round but
// disk-round-final.
func lastRunningRound(rounds []streamRound, lastRAMPaused bool) time.Duration {
	var ram []float64
	disk := 0.0
	for _, r := range rounds {
		switch {
		case r.Kind == "ram":
			ram = append(ram, r.Seconds)
		case r.Kind == "disk" && r.Name != "disk-round-final":
			disk = r.Seconds
		}
	}
	if lastRAMPaused && len(ram) > 0 {
		ram = ram[:len(ram)-1]
	}
	last := disk
	if len(ram) > 0 {
		last += ram[len(ram)-1]
	}
	return time.Duration(last * float64(time.Second))
}

// hviPausedBound is how long the relay waits for a byte of hvi's stream
// once its paused part has begun: hvi's budget for that part, given the
// last running round as hull timed it, and the margin.
func hviPausedBound(lastRunning time.Duration) time.Duration {
	return hviPausedFloor + 2*lastRunning + hviPausedMargin
}

// hviUnwindWait is how long hull waits for hvi to answer whether it fenced
// the guest after a migrate stream broke before its manifest. hvi serves
// one connection at a time, so the answer comes once its migrate op has
// ended, and it sees the closed connection only at its next write: after
// a disk sync or a vCPU park that can take seconds on a busy host, and
// within the 30 s it waits for a stalled reader.
const hviUnwindWait = time.Minute

// relayFrameTestHook, when set, is called with each component hvi sends
// before hull relays it; an error it returns is hull failing on that frame.
var relayFrameTestHook func(component string) error

// fenceAfterBreak asks hvi whether it fenced the guest after hull stopped
// reading its migrate stream before the manifest, on a failure of its own
// (a spool write, a document too large to keep, a frame that does not
// match its sha256) or of the connection. hvi fences once the manifest is
// in the socket's buffer, which can be before hull has read the frames in
// front of it: with a small last round, what is left of the stream can sit
// there whole when hull fails. Only hvi knows which it was, and it has
// ended the op by the time it answers (hviUnwindWait).
func (ss *streamSender) fenceAfterBreak(err error) error {
	reply, qerr := hviControl(ss.state.ControlSock, map[string]any{"op": "status"}, hviUnwindWait)
	switch {
	case qerr != nil:
		return &hviFenceUnknownError{err: err, why: qerr}
	case reply.State == store.MigrationFenced:
		return fmt.Errorf("hvi fenced the guest, though hull did not take its whole stream: %w", err)
	}
	return &hviRanOnError{err: err}
}

// hviFenceUnknownError is a migrate stream that broke before its manifest
// when hvi then did not say whether it had fenced the guest.
type hviFenceUnknownError struct{ err, why error }

func (e *hviFenceUnknownError) Error() string {
	return fmt.Sprintf("hvi's stream broke before its manifest (%v), and hvi did not say whether it "+
		"fenced the guest (%v)", e.err, e.why)
}

func (e *hviFenceUnknownError) Unwrap() error { return e.err }

// hviRanOnError is a migrate stream that ended before its manifest with
// the guest not fenced: hvi refused the op, or said, after the stream
// broke, that the guest is not fenced (a capture that failed, a reader hvi
// cut for stalling, the connection lost, or hull failing on a frame before
// hvi had written its manifest). The guest runs on, and the move can be
// run again or aborted with nothing to restore.
type hviRanOnError struct{ err error }

func (e *hviRanOnError) Error() string {
	return fmt.Sprintf("hvi's stream ended before its manifest, so the guest runs on here: %v", e.err)
}

func (e *hviRanOnError) Unwrap() error { return e.err }

// bytesBuffer collects what is written to it.
type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

// replayHvi relays the spooled stream of an earlier attempt that hvi
// sealed: the guest is fenced already, and its RAM exists only there.
func (ss *streamSender) replayHvi(dir string) (json.RawMessage, error) {
	f, err := os.Open(filepath.Join(dir, spoolStream))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	reply, err := os.ReadFile(filepath.Join(dir, spoolSealed))
	if err != nil {
		return nil, fmt.Errorf("the spooled stream %s was never sealed: %w", dir, err)
	}
	br := bufio.NewReaderSize(f, 1<<20)
	folder := &roundFolder{ss: ss, replay: true}
	for {
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			break
		}
		line, err := readHviLine(br, migrateMaxHeader)
		if err != nil {
			return nil, fmt.Errorf("the spooled stream: %w", err)
		}
		hdr, err := parseHviComponentHeader(line)
		if err != nil {
			return nil, fmt.Errorf("the spooled stream: %w", err)
		}
		var keep *bytesBuffer
		var also io.Writer
		if hdr.Component == "manifest.json" {
			keep = &bytesBuffer{}
			also = keep
		}
		if err := ss.relayFrame(folder, line, hdr, br, also); err != nil {
			return nil, fmt.Errorf("the spooled stream: %w", err)
		}
		if keep != nil {
			ss.manifest = keep.b
		}
	}
	folder.flush()
	if ss.manifest == nil {
		return nil, errors.New("the spooled stream has no manifest.json")
	}
	line := json.RawMessage(strings.TrimSpace(string(reply)))
	ss.takeReplayPause(line)
	return line, nil
}

// takeReplayPause puts on the record of a replay when hvi paused the
// guest, which was in an earlier attempt: from the spool's reply when that
// is hvi's reply to the migrate that fenced the guest, or else from what
// the instance's record of the fence or the earlier attempt's record of
// the stream kept of it. A second stream's reply will not do, nor will a
// reply hull wrote for a spool it checked: hvi stamps the first with its
// own seal and the second has no stats. With none of them, the pause is
// left unknown, and so is the downtime, rather than counted from the
// replay.
func (ss *streamSender) takeReplayPause(reply json.RawMessage) {
	if _, _, ok := firstPause(reply); ok {
		var r hviReply
		if json.Unmarshal(reply, &r) == nil {
			ss.takeHviStats(&r)
			return
		}
	}
	m := ss.m
	if rec, err := readHviCheckpointRecord(instanceCheckpointDir(m.s, m.name)); err == nil &&
		rec.TxID == m.rec.TxID {
		if at, sealed, ok := firstPause(rec.Stats); ok {
			ss.stats.PausedAt, ss.stats.SealedMs = at, sealed
			return
		}
	}
	if p := ss.prev; p != nil && !p.PausedAt.IsZero() {
		ss.stats.PausedAt, ss.stats.SealedMs = p.PausedAt, p.SealedMs
	}
}

// firstPause reads when hvi paused the guest, and how long after that it
// sealed the stream, from hvi's reply to the migrate that fenced the
// guest. A reply to a second stream of a fenced guest, which names the
// first manifest, gives that stream's seal as its pause, and does not
// count.
func firstPause(line []byte) (time.Time, int64, bool) {
	var r hviReply
	if len(line) == 0 || json.Unmarshal(line, &r) != nil || len(r.Stats) == 0 {
		return time.Time{}, 0, false
	}
	var st struct {
		First      *string `json:"first_manifest_sha256"`
		PrecopyEnd int64   `json:"precopy_end_unix_ms"`
		GateMs     int64   `json:"gate_ms"`
		SealedAt   int64   `json:"sealed_at_unix_ms"`
	}
	if json.Unmarshal(r.Stats, &st) != nil || st.First != nil || st.PrecopyEnd <= 0 {
		return time.Time{}, 0, false
	}
	stop := st.PrecopyEnd + max(st.GateMs, 0)
	var sealed int64
	if st.SealedAt >= stop {
		sealed = st.SealedAt - stop
	}
	return time.UnixMilli(stop).UTC(), sealed, true
}

// streamTransfer is transfer for a streamed move. A destination that has
// verified the move already is not sent it again. A guest that hvi has not
// fenced yet is sent live; one whose stream hvi sealed in an earlier
// attempt is sent from the spool, which holds hvi's disk rounds too.
func (m *moveSource) streamTransfer(ctx context.Context) (retErr error) {
	prev := m.rec.Stream
	if reply, err := m.peer.call(ctx, "migrate-status", []string{"--txid", m.rec.TxID}, nil); err == nil &&
		reply.Record != nil &&
		migrateStateOrder(roleDestination, reply.Record.State) >= migrateStateOrder(roleDestination, dstVerified) {
		if m.rec.Disk == nil || m.rec.Stream == nil {
			if err := m.adoptVerifiedStream(reply.Record); err != nil {
				return err
			}
		} else {
			if err := m.checkDiskUnchanged(); err != nil {
				return err
			}
			if err := m.compareStreamReceived(reply.Record); err != nil {
				return err
			}
		}
		m.destPaused = reply.Record.State == dstRestoredPaused
		return m.rec.advance(m.path, srcTransferred)
	}
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	replay := fencedFor(state, m.rec.TxID)
	// A fence hvi made for this move that the move's record does not hold:
	// hull died after hvi's manifest, before it recorded the fence on the
	// instance, or after that and before the move's record took it (the
	// commit point writes it). Either way it is taken from the spool, which
	// is checked whole or streamed again.
	if (!replay && hviVMState(state) == store.MigrationFenced) || (replay && m.rec.Disk == nil) {
		if err := m.adoptStreamFence(state); err != nil {
			return err
		}
		if state, err = m.s.GetInstance(m.name); err != nil {
			return err
		}
		replay = true
	}
	if !replay {
		if err := requireRunningHvi(state, "migrate"); err != nil {
			return err
		}
		switch vm := hviVMState(state); vm {
		case "running", "paused":
		default:
			return fmt.Errorf("its VM is %s; a move takes a running or paused one", vm)
		}
	} else if err := m.checkDiskUnchanged(); err != nil {
		return err
	}
	if replay && m.rec.BundleDir != "" {
		// A hull that died after recording the fence may not have lived
		// to seal the spool.
		if _, err := m.ensureStreamSpool(state, m.rec.BundleDir); err != nil {
			return err
		}
	}
	disk, ok := flagValue(state.CmdLine, "--disk")
	if !ok {
		return errors.New("the instance has no --disk")
	}
	if !baseHeld(m.s, m.rec.BaseSHA256) {
		return fmt.Errorf("the base %s is gone from this store", m.rec.BaseSHA256)
	}
	info, err := os.Stat(disk)
	if err != nil {
		return err
	}
	root, ok := flagValue(state.CmdLine, "--checkpoint-root")
	if !ok {
		return errors.New("the instance has no checkpoint root")
	}
	spoolDir := filepath.Join(root, m.rec.TxID)
	mode := "live"
	if replay {
		mode = "replay"
	}
	stats := &migrateStreamStats{Mode: mode}
	m.logf("Streaming %s to %s (%s)", m.name, m.rec.Peer, mode)
	started := time.Now()
	pr, pw := io.Pipe()
	link := &softLink{w: pw}
	sp, session := m.peer.(sessionPeer)
	session = session && m.rec.Session
	sw := newMoveStreamWriter(link)
	if session {
		// The window bounds what is on its way, and the writer's buffer
		// would be more of it.
		link.win = newAckWindow()
		sw = newMoveStreamWriterSize(link, sessionWriteBuffer)
	}
	attempt, err := newMoveTxID()
	if err != nil {
		return err
	}
	m.sealGate, m.sealGateOnce = make(chan struct{}), sync.Once{}
	ss := &streamSender{m: m, state: state, sw: sw, link: link, size: info.Size(), stats: stats,
		spoolDir: spoolDir, ctx: ctx, attempt: attempt, callDone: make(chan struct{}), sealGate: m.sealGate,
		prev: prev}
	if session {
		ss.accepted = make(chan struct{})
	}
	type sendResult struct {
		hviReply json.RawMessage
		err      error
	}
	sent := make(chan sendResult, 1)
	go func() {
		reply, err := ss.send(replay)
		if err != nil || !session {
			_ = pw.CloseWithError(err)
		}
		sent <- sendResult{reply, err}
	}()
	args := []string{"--stream", "--txid", m.rec.TxID, "--name", m.name}
	var reply *migrateReply
	var open *openSession
	if session {
		reply, open, err = ss.runSession(sp, append(args, "--session"), pr, pw)
	} else {
		reply, err = m.peer.call(ctx, "migrate-receive", args, pr)
		close(ss.callDone)
		_ = pr.CloseWithError(errors.New("the destination stopped reading"))
	}
	// A session left open is for the commit; one this attempt does not
	// get to the commit point is ended, which leaves the guest restored
	// paused on the destination.
	defer func() {
		if open != nil && m.session != open {
			open.end()
		}
	}()
	res := <-sent
	m.spoolSealing = ss.sealDone
	if ss.recorded != nil {
		m.fenceRecord, m.fenceRecordedAt = ss.recorded, &ss.recordedAt
		defer func() {
			// A move that stops here leaves the instance's record of the
			// fence, and its sealed spool, for the abort and the re-run.
			if retErr != nil {
				m.releaseSeal()
				if err := m.waitFenceRecord(); err != nil {
					retErr = errors.Join(retErr, err)
				}
			}
		}()
	}
	if res.err == nil {
		stats.Chain = sw.chain.sum()
		stats.WireBytes = sw.written()
		stats.Seconds = time.Since(started).Seconds()
		stats.HviReply = res.hviReply
	}
	stats.step("destination answered")
	if streamTestHook != nil && res.err == nil && err == nil {
		streamTestHook("answered")
	}
	// What was sent is on record before the move goes on: a reply that is
	// lost leaves a re-run to compare the destination's record with it.
	// A step that fails from here records it (stopped), and one that
	// succeeds records it with the next state, in one write.
	m.rec.Stream = stats
	if res.err != nil && !errors.Is(res.err, io.ErrClosedPipe) && err == nil {
		err = fmt.Errorf("send: %w", res.err)
	}
	if err != nil {
		if res.err != nil && !errors.Is(res.err, io.ErrClosedPipe) && !strings.Contains(err.Error(), res.err.Error()) {
			err = fmt.Errorf("%w (sending: %w)", err, res.err)
		}
		return fmt.Errorf("transfer to %s: %w", m.rec.Peer, err)
	}
	if reply.Record == nil {
		return errors.New("the destination's receive reply has no record")
	}
	if err := m.checkDiskUnchanged(); err != nil {
		return err
	}
	if err := m.compareStreamReceived(reply.Record); err != nil {
		return err
	}
	m.rec.Transfer = &migrateTransfer{Bytes: stats.WireBytes, Seconds: stats.Seconds, StartedAt: started.UTC(),
		BytesPerSec: perSec(stats.WireBytes, stats.Seconds)}
	m.timing("transfer", started)
	m.destPaused = reply.Record.State == dstRestoredPaused
	if reply.Record.Stream != nil {
		stats.DestSteps = mergeSteps(stats.DestSteps, reply.Record.Stream.Steps)
	}
	m.logf("Sent %s in %.1f s (%s/s): disk %s as %s, RAM %s, state %s; destination: %s",
		humanBytes(stats.WireBytes), stats.Seconds, humanBytes(perSec(stats.WireBytes, stats.Seconds)),
		humanBytes(stats.DiskBytes), humanBytes(stats.DiskWireBytes), humanBytes(stats.RAMBytes),
		humanBytes(stats.StateBytes), reply.Record.Verify)
	if m.destPaused {
		// The destination holds the guest restored paused: the source's
		// commit point, recorded with the transfer in one write, as the
		// prepare step would record it.
		if err := m.rec.advanceThrough(m.path, srcTransferred, srcCommitting); err != nil {
			return err
		}
		stats.step("commit point recorded")
		// The commit goes in the session, which stays open for it.
		m.session = open
		return nil
	}
	return m.rec.advance(m.path, srcTransferred)
}

// sessionWriteBuffer is the stream writer's buffer in a session.
const sessionWriteBuffer = 64 << 10

// runSession runs the streamed receive as a session: the destination's
// accepted line lets hvi's migrate begin, its acked lines move the
// window, and its verified line is the receive's reply, with the session
// left open for the commit. A session that ends before it says verified
// gives its last line instead, and no session to commit in.
func (ss *streamSender) runSession(sp sessionPeer, args []string, pr *io.PipeReader,
	pw *io.PipeWriter) (*migrateReply, *openSession, error) {
	win := ss.link.win
	verified := make(chan *migrateReply, 1)
	done := make(chan sessionResult, 1)
	var acceptOnce sync.Once
	go func() {
		reply, err := sp.session(ss.ctx, "migrate-receive", args, pr, func(ev *migrateReply) {
			switch ev.Event {
			case eventAccepted:
				if ev.Detail == ss.attempt {
					acceptOnce.Do(func() { close(ss.accepted) })
				}
			case eventAcked:
				win.ack(ev.Bytes)
			case eventVerified:
				select {
				case verified <- ev:
				default:
				}
			}
		})
		win.close()
		close(ss.callDone)
		_ = pr.CloseWithError(errors.New("the destination stopped reading"))
		done <- sessionResult{reply, err}
	}()
	select {
	case v := <-verified:
		return v, &openSession{sw: ss.sw, pw: pw, win: win, done: done}, nil
	case r := <-done:
		select {
		case v := <-verified:
			// It said verified, and then ended the session itself: the
			// commit goes by migrate-commit.
			return v, nil, nil
		default:
		}
		return r.reply, nil, r.err
	}
}

// send writes the whole stream. Live, it ends with hvi having fenced the
// guest, the fence recorded on the instance and in the move's record.
func (ss *streamSender) send(replay bool) (json.RawMessage, error) {
	m := ss.m
	if err := ss.sw.header(moveStreamHeader{TxID: m.rec.TxID, Name: m.name, Epoch: m.rec.Epoch,
		BaseSHA256: m.rec.BaseSHA256, DiskSize: ss.size, Replay: replay, Attempt: ss.attempt}); err != nil {
		return nil, err
	}
	if err := ss.sw.flush(); err != nil {
		return nil, err
	}
	if replay {
		// The guest is fenced, and the spool holds hvi's whole stream,
		// its disk rounds included: the destination applies them to a
		// fresh copy of the base.
		reply, err := ss.replayHvi(ss.spoolDir)
		if err != nil {
			return nil, err
		}
		if err := ss.sw.trailer(reply); err != nil {
			return nil, err
		}
		return reply, ss.link.err
	}
	// A refusal of the header (no base, another profile, no room for the
	// base's copy) must come while the guest still runs: once hvi's
	// migrate has begun, hull reads it to the end.
	if err := ss.waitAccepted(); err != nil {
		return nil, err
	}
	unlock, err := lockInstanceMigration(m.s, m.name)
	if err != nil {
		return nil, err
	}
	sp, err := newSpool(ss.spoolDir)
	if err != nil {
		unlock()
		return nil, err
	}
	select {
	case <-ss.callDone:
		// The destination's receive ended before hvi's migrate began,
		// which it never does unless it failed: its reply says why.
		sp.close()
		unlock()
		return nil, errors.New("the destination stopped receiving before hvi's migrate began")
	default:
	}
	// A restored guest that was never resumed may be resumed by the
	// migrate op: hvi resumes the guest when the stream fails before its
	// seal.
	err = markRestoredGuestRunning(m.s, m.name)
	var reply json.RawMessage
	var files map[string][]byte
	if err == nil {
		// From the migrate op until the fence is recorded, a signal that
		// would end hull waits (moveSignals).
		m.signals.hold()
		reply, files, err = ss.relayHvi(sp)
		if err != nil {
			m.signals.release()
		}
	}
	if err != nil {
		sp.close()
		unlock()
		var ranOn *hviRanOnError
		if errors.As(err, &ranOn) {
			ss.progress("hvi did not fence %s: its stream ended before the manifest, and the guest runs on here",
				m.name)
		}
		return nil, err
	}
	if streamTestHook != nil {
		streamTestHook("relayed")
	}
	ss.stats.HviReply = reply
	ss.stats.step("hvi sealed the stream")
	// hvi has fenced the guest, and every byte of its stream is in the
	// spool's file. The move's record takes the fence in memory, and
	// writes it with the commit point, the one synced write the commit
	// waits for. The instance's records of the fence are written beside
	// the trailer and the destination's checks, and the spool is synced
	// behind the move: on a busy disk every synced write is a wait the
	// stopped guest would otherwise sit through. A hull that dies before
	// they are done leaves a spool that a re-run or an abort checks whole
	// before using it (verifySpool).
	if err := ss.noteFence(); err != nil {
		m.signals.release()
		sp.close()
		unlock()
		return nil, err
	}
	ss.progress("hvi sealed the stream and fenced %s (%d ms after the pause)", m.name, ss.stats.SealedMs)
	ss.recorded = make(chan error, 1)
	fenceDone := make(chan struct{})
	go func() {
		err := ss.recordInstanceFence()
		ss.recordedAt = time.Now().UTC()
		m.signals.release()
		close(fenceDone)
		if err == nil && streamTestHook != nil {
			streamTestHook("recorded")
		}
		ss.recorded <- err
	}()
	sendErr := ss.link.err
	if sendErr == nil {
		sendErr = ss.sw.trailer(reply)
		ss.stats.step("trailer sent")
	}
	ss.sealDone = make(chan error, 1)
	go func() {
		// The seal's sync of the whole spool waits until the commit is
		// answered or the move stops (moveSource.releaseSeal): run beside
		// the commit point's synced record, it held that record up for
		// seconds, the guest stopped all the while.
		<-ss.sealGate
		err := sp.seal(files, reply)
		sp.close()
		<-fenceDone
		unlock()
		ss.sealDone <- err
	}()
	if sendErr != nil {
		return nil, sendErr
	}
	return reply, ss.link.err
}

// acceptPoll is how often the source asks whether the destination took the
// stream's header.
const acceptPoll = 50 * time.Millisecond

// waitAccepted waits until the destination's record says it took this
// attempt's header and copied the base, and fails if its receive ends
// first.
func (ss *streamSender) waitAccepted() error {
	m := ss.m
	started := time.Now()
	if ss.accepted != nil {
		// A session says so on its own: no status to poll.
		select {
		case <-ss.accepted:
		case <-ss.callDone:
			select {
			case <-ss.accepted:
			default:
				return errors.New("the destination refused the stream before hvi's migrate began")
			}
		case <-ss.ctx.Done():
			return ss.ctx.Err()
		}
		ss.stats.step("destination accepted")
		if took := time.Since(started); took > time.Second {
			ss.progress("the destination took the stream in %.1f s (it made the guest's disk from the base)", took.Seconds())
		}
		return nil
	}
	for {
		reply, err := m.peer.call(ss.ctx, "migrate-status", []string{"--txid", m.rec.TxID}, nil)
		if err == nil && reply.Record != nil && reply.Record.StreamAccepted == ss.attempt {
			ss.stats.step("destination accepted")
			if took := time.Since(started); took > time.Second {
				ss.progress("the destination took the stream in %.1f s (it made the guest's disk from the base)", took.Seconds())
			}
			return nil
		}
		select {
		case <-ss.callDone:
			return errors.New("the destination refused the stream before hvi's migrate began")
		case <-ss.ctx.Done():
			return ss.ctx.Err()
		case <-time.After(acceptPoll):
		}
	}
}

// streamTestHook, when set, is called at points of a streamed move a test
// stops it at: "relayed" is once hvi has sealed its stream and fenced the
// guest, and hull has read it all, before anything is recorded; "recorded"
// is once the fence is recorded, maybe before the spool is sealed.
var streamTestHook func(stage string)

// softLink is the writer between the stream and the destination. Once a
// write to the destination fails it keeps the error and takes every later
// write without sending it, so a source that is reading hvi's migrate
// stream reads it to the end into its spool whatever became of the
// destination. Stopping half way would not stop hvi: the rest of a small
// last round, the state and the manifest can all sit in the socket's
// buffer, and hvi seals, and fences the guest, once they are written there.
// A source that left them unread would be fenced with no whole copy of the
// guest anywhere.
//
// In a session its window (win) holds a write back until the destination
// has read enough of what went before; a session that ends lets every
// write through.
type softLink struct {
	w   io.Writer
	err error
	win *ackWindow
}

func (l *softLink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && l.err == nil {
		piece := p[:min(len(p), sessionWriteBuffer)]
		if l.win != nil {
			l.win.wait(int64(len(piece)))
		}
		if _, err := l.w.Write(piece); err != nil {
			l.err = err
		}
		p = p[len(piece):]
	}
	return n, nil
}

// noteFence puts the fence hvi made on the move's record, in memory: the
// spool, the manifest it ends with, the disk as fenced. The record carries
// them to disk with its next write, which before the commit is the one
// that records the commit point; until then the move's record, synced
// before the stream began, already keeps the instance from running here
// (moveRefusal), and a spool a re-run or an abort finds without its record
// is checked whole before it is used.
func (ss *streamSender) noteFence() error {
	m := ss.m
	disk, _ := flagValue(ss.state.CmdLine, "--disk")
	size, mod, err := fileModNs(disk)
	if err != nil {
		return err
	}
	m.rec.BundleDir = ss.spoolDir
	m.rec.ManifestSHA256 = sha256Hex(ss.manifest)
	m.rec.Disk = &migrateDisk{Path: disk, Size: size, ModNs: mod}
	return nil
}

// recordInstanceFence records the fence on the instance: the spool as its
// checkpoint, taken over by this move. latest.json goes first, as a
// checkpoint writes it: a restore that finds it naming a checkpoint the
// record never took in refuses rather than rewinding past it. It runs
// beside the end of the move; the commit does not wait for it.
func (ss *streamSender) recordInstanceFence() error {
	m := ss.m
	sum := sha256Hex(ss.manifest)
	if err := writeHviCheckpointRecord(instanceCheckpointDir(m.s, m.name), hviCheckpointRecord{
		Dir: ss.spoolDir, ManifestSHA256: sum, Then: hviThenFence, Stats: ss.stats.HviReply,
		SavedAt: time.Now().UTC(), TxID: m.rec.TxID, Manifest: ss.manifest,
	}); err != nil {
		return fmt.Errorf("the stream is sealed in %s but recording it failed: %w", ss.spoolDir, err)
	}
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	state.Migration = &store.MigrationState{State: store.MigrationMigrating, BundleDir: ss.spoolDir,
		TxID: m.rec.TxID, ManifestSHA256: sum, MoveTxID: m.rec.TxID, Peer: m.rec.Peer}
	return m.s.SaveInstance(state)
}

// adoptStreamFence records a stream hvi sealed for this move when hull did
// not live to record it. The spool must be whole: sealed, or checked frame
// by frame, or taken again from the hvi that still holds the fenced guest.
func (m *moveSource) adoptStreamFence(state *store.InstanceState) error {
	root, ok := flagValue(state.CmdLine, "--checkpoint-root")
	if !ok {
		return errors.New("the instance has no checkpoint root")
	}
	dir := filepath.Join(root, m.rec.TxID)
	man, err := m.ensureStreamSpool(state, dir)
	if err != nil {
		return err
	}
	state.Migration = &store.MigrationState{State: store.MigrationMigrating, BundleDir: dir, TxID: m.rec.TxID,
		ManifestSHA256: sha256Hex(man), MoveTxID: m.rec.TxID, Peer: m.rec.Peer}
	if err := m.s.SaveInstance(state); err != nil {
		return err
	}
	disk, _ := flagValue(state.CmdLine, "--disk")
	size, mod, err := fileModNs(disk)
	if err != nil {
		return err
	}
	m.rec.BundleDir, m.rec.ManifestSHA256 = dir, state.Migration.ManifestSHA256
	m.rec.Disk = &migrateDisk{Path: disk, Size: size, ModNs: mod}
	m.logf("Adopted the stream hvi fenced the guest with, spooled in %s", dir)
	return writeMigrationRecord(m.path, m.rec)
}

// adoptVerifiedStream takes on a destination's verified copy of this move
// when hull here died after the destination checked the stream and before
// the move's record took what was sent: the record has no account of the
// stream to compare with, but the fence does. The fence is adopted from
// the spool (checked whole, or streamed again by the fenced hvi, whose
// reply names the first manifest), and the destination's manifest must be
// the one the guest was fenced with. hvi on the destination checked every
// byte against that manifest, so its copy is the fenced machine, and it
// has not run: only the commit resumes it.
func (m *moveSource) adoptVerifiedStream(dst *migrationRecord) error {
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	if !fencedFor(state, m.rec.TxID) && hviVMState(state) != store.MigrationFenced {
		return errors.New("the destination has verified this move, but the guest here is not fenced for it")
	}
	if err := m.adoptStreamFence(state); err != nil {
		return err
	}
	switch {
	case dst.TxID != m.rec.TxID || dst.Epoch != m.rec.Epoch:
		return fmt.Errorf("the destination recorded move %s epoch %d, not %s epoch %d", dst.TxID, dst.Epoch,
			m.rec.TxID, m.rec.Epoch)
	case dst.ManifestSHA256 == "" ||
		(dst.ManifestSHA256 != m.rec.ManifestSHA256 && dst.ManifestSHA256 != m.rec.FirstManifestSHA256):
		return fmt.Errorf("the destination verified manifest %s, and the guest here was fenced with %s",
			dst.ManifestSHA256, m.recordedFirstManifest(state))
	case dst.BaseSHA256 != m.rec.BaseSHA256:
		return fmt.Errorf("the destination applied the disk to base %s, not %s", dst.BaseSHA256, m.rec.BaseSHA256)
	}
	m.rec.Stream = &migrateStreamStats{Mode: "adopted", Rounds: []streamRound{}}
	if dst.Stream != nil {
		m.rec.Stream.Chain, m.rec.Stream.HviReply = dst.Stream.Chain, dst.Stream.HviReply
	}
	m.logf("%s verified the stream the guest was fenced with (manifest %s) before this host recorded what it "+
		"sent; the move goes on with that copy", m.rec.Peer, digestHead(dst.ManifestSHA256))
	return nil
}

// compareStreamReceived checks the destination's record of a streamed move
// against what the source sent.
func (m *moveSource) compareStreamReceived(dst *migrationRecord) error {
	if dst.TxID != m.rec.TxID || dst.Epoch != m.rec.Epoch {
		return fmt.Errorf("the destination recorded move %s epoch %d, not %s epoch %d", dst.TxID, dst.Epoch,
			m.rec.TxID, m.rec.Epoch)
	}
	if m.rec.ManifestSHA256 == "" || dst.ManifestSHA256 != m.rec.ManifestSHA256 {
		return fmt.Errorf("the destination has manifest sha256 %s, the source %s", dst.ManifestSHA256, m.rec.ManifestSHA256)
	}
	if m.rec.Stream == nil || dst.Stream == nil || dst.Stream.Chain == "" || dst.Stream.Chain != m.rec.Stream.Chain {
		return errors.New("the destination's record of the stream is not the stream the source sent")
	}
	if dst.BaseSHA256 != m.rec.BaseSHA256 {
		return fmt.Errorf("the destination applied the disk to base %s, not %s", dst.BaseSHA256, m.rec.BaseSHA256)
	}
	return nil
}

// sendBaseFirst puts the move's base on the destination before the guest
// stops, as `hull migrate-prepare` would have. A base that cannot be sent
// turns the move into a whole-bundle one: nothing is fenced yet.
func (m *moveSource) sendBaseFirst(ctx context.Context) error {
	m.logf("Sending base %s to %s (once per base; `hull migrate-prepare` does it ahead of a move)",
		digestHead(m.rec.BaseSHA256), m.rec.Peer)
	st, err := sendBase(ctx, m.s, m.peer, m.rec.BaseSHA256)
	if err != nil {
		m.rec.Transport, m.rec.BaseSHA256 = transportBundle, ""
		m.rec.TransportWhy = "sending the base failed: " + refusalDetail(err)
		m.logf("Transport: the whole checkpoint and disk, because %s", m.rec.TransportWhy)
		return writeMigrationRecord(m.path, m.rec)
	}
	m.logf("  base: %s (%s)", st.Result, st.Detail)
	return nil
}

// streamTransferWithBase is streamTransfer, which sends the base and tries
// once more when the destination answers that it does not hold it.
func (m *moveSource) streamTransferWithBase(ctx context.Context) error {
	err := m.streamTransfer(ctx)
	if refusalCode(err) != codeNoBase {
		return err
	}
	m.logf("%s does not hold base %s", m.rec.Peer, digestHead(m.rec.BaseSHA256))
	if _, err := sendBase(ctx, m.s, m.peer, m.rec.BaseSHA256); err != nil {
		return fmt.Errorf("send base %s: %w", digestHead(m.rec.BaseSHA256), err)
	}
	return m.streamTransfer(ctx)
}
