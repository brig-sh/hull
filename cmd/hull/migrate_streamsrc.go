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
	"crypto/sha256"
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
// carries, in order, the disk blocks that differ from the base (round 0,
// then a few rounds of what the guest wrote meanwhile), hvi's migrate
// stream (RAM rounds while the guest runs, then the pause, the last RAM
// round, the machine's state and the manifest, after which hvi has fenced
// the guest), the disk blocks the guest wrote during the RAM rounds, and
// the trailer.
//
// hvi serves one control connection at a time, so the disk rounds hull
// asks for cannot run while the migrate op holds the socket: the last disk
// round is read once hvi has sealed the stream. The guest is fenced by
// then, and its disk synced, so that round and the RAM describe the same
// machine.

// Disk pre-copy policy: stop when a round finds at most this many blocks
// (2 MiB), or after streamRoundCap rounds.
const (
	streamDiskSettle = 32
	streamRoundCap   = 5
)

// blockHash is what one disk block holds, as far as a comparison needs.
type blockHash struct {
	zero bool
	sum  [32]byte
}

// diskCompare finds the blocks of a disk that differ from what the
// destination holds: its copy of the base, plus every block sent since.
// It is the disk engine when hvi has no dirty-block rounds, or when the
// running hvi did not boot from the base, and it reads the whole disk on
// every pass.
type diskCompare struct {
	size int64
	base map[int64][32]byte
	sent map[int64]blockHash
}

// newDiskCompare hashes the base's blocks.
func newDiskCompare(basePath string, size int64) (*diskCompare, error) {
	f, err := os.Open(basePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != size {
		return nil, fmt.Errorf("the base is %d bytes and the disk %d", info.Size(), size)
	}
	c := &diskCompare{size: size, base: map[int64][32]byte{}, sent: map[int64]blockHash{}}
	err = forEachDataBlock(f, size, func(index int64, data []byte) error {
		c.base[index] = sha256.Sum256(data)
		return nil
	})
	return c, err
}

// held is what the destination holds at block index.
func (c *diskCompare) held(index int64) blockHash {
	if h, ok := c.sent[index]; ok {
		return h
	}
	if sum, ok := c.base[index]; ok {
		return blockHash{sum: sum}
	}
	return blockHash{zero: true}
}

// diff calls emit for every block of disk that differs from what the
// destination holds, in index order, and records it as sent.
func (c *diskCompare) diff(disk *os.File, emit func(index int64, data []byte) error) error {
	seen := map[int64]bool{}
	err := forEachDataBlock(disk, c.size, func(index int64, data []byte) error {
		seen[index] = true
		h := blockHash{sum: sha256.Sum256(data)}
		if c.held(index) == h {
			return nil
		}
		c.sent[index] = h
		return emit(index, data)
	})
	if err != nil {
		return err
	}
	// Blocks that are zero on the disk now but hold data there.
	var zeroed []int64
	for index := range c.base {
		if !seen[index] && !c.held(index).zero {
			zeroed = append(zeroed, index)
		}
	}
	for index, h := range c.sent {
		if !seen[index] && !h.zero {
			if _, inBase := c.base[index]; !inBase {
				zeroed = append(zeroed, index)
			}
		}
	}
	sortInt64(zeroed)
	zero := make([]byte, diskBlockSize)
	for _, index := range zeroed {
		c.sent[index] = blockHash{zero: true}
		if err := emit(index, zero[:blockLen(c.size, index)]); err != nil {
			return err
		}
	}
	return nil
}

// sortInt64 sorts s ascending.
func sortInt64(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// batcher gathers adjacent blocks into disk batches of up to 16.
type batcher struct {
	size  int64
	first int64
	count int64
	buf   []byte
	flush func(first, count int64, data []byte) error
}

func newBatcher(size int64, flush func(first, count int64, data []byte) error) *batcher {
	return &batcher{size: size, buf: make([]byte, 0, 16*diskBlockSize), flush: flush}
}

func (b *batcher) add(index int64, data []byte) error {
	if b.count > 0 && (index != b.first+b.count || b.count == 16) {
		if err := b.done(); err != nil {
			return err
		}
	}
	if b.count == 0 {
		b.first = index
	}
	b.buf = append(b.buf, data...)
	b.count++
	return nil
}

func (b *batcher) done() error {
	if b.count == 0 {
		return nil
	}
	err := b.flush(b.first, b.count, b.buf)
	b.buf, b.count = b.buf[:0], 0
	return err
}

// streamSender writes one streamed move into sw.
type streamSender struct {
	m     *moveSource
	state *store.InstanceState
	sw    *moveStreamWriter
	disk  *os.File
	size  int64
	stats *migrateStreamStats
	// hviDisk is set while hvi answers disk-dirty; cmp is the compare
	// engine, when it is the one in use.
	hviDisk bool
	cmp     *diskCompare
	// lastRound is the round hvi numbered the last disk-dirty read.
	lastRound int
	// spoolDir is where hvi's stream is kept on this host.
	spoolDir string
	// manifest is hvi's manifest.json, once relayed.
	manifest []byte
	// roundTotal is the blocks of the disk round being sent, when known.
	roundTotal int64
	// link is what sw writes to; its error is the destination's failure.
	link *softLink
	// diskRounds gather each disk round's batches for the trailer.
	diskRounds []*diskRoundDigest
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
}

func (ss *streamSender) progress(format string, args ...any) {
	ss.m.logf("move: "+format, args...)
}

// beginDiskRound starts the trailer's account of a disk round: from hvi's
// dirty blocks when dirty is hvi's reply that started it, else found by
// comparison.
func (ss *streamSender) beginDiskRound(round int, final bool, dirty *hviDiskDirtyReply) *diskRoundDigest {
	d := &diskRoundDigest{round: streamDiskRound{Round: round, Final: final, From: "compare"}}
	if dirty != nil {
		d.round.From, d.round.Generation, d.round.BitmapSHA256 = "hvi", dirty.Generation, dirty.BitmapSHA256
	}
	ss.diskRounds = append(ss.diskRounds, d)
	return d
}

// diskTrailer is the trailer's account of the disk rounds sent.
func (ss *streamSender) diskTrailer() *streamDiskRounds {
	t := &streamDiskRounds{Engine: ss.stats.DiskEngine, Rounds: []streamDiskRound{}}
	for _, d := range ss.diskRounds {
		t.Rounds = append(t.Rounds, d.done())
	}
	return t
}

// sendBatch writes one disk batch and counts it in round r, of total
// blocks when that is known (0 when not), and in the trailer's account d.
func (ss *streamSender) sendBatch(r *streamRound, d *diskRoundDigest, round int,
	final bool) func(first, count int64, data []byte) error {
	started := time.Now()
	last := started
	return func(first, count int64, data []byte) error {
		if err := ss.link.err; err != nil {
			return err
		}
		wire, sum, err := ss.sw.disk(round, final, first, count, data)
		if err != nil {
			return err
		}
		d.add(count, sum)
		r.Blocks += count
		r.Bytes += int64(len(data))
		r.WireBytes += wire
		if now := time.Now(); now.Sub(last) >= progressEvery {
			last = now
			line := fmt.Sprintf("%s: %d blocks so far, %s/s", r.Name, r.Blocks,
				humanBytes(perSec(r.WireBytes, now.Sub(started).Seconds())))
			if ss.roundTotal > 0 {
				line += fmt.Sprintf(" (%d%% of %d)", r.Blocks*100/ss.roundTotal, ss.roundTotal)
			}
			ss.progress("%s", line)
		}
		return nil
	}
}

// endRound records a round and prints it.
func (ss *streamSender) endRound(r streamRound, started time.Time) {
	r.Seconds = time.Since(started).Seconds()
	ss.stats.Rounds = append(ss.stats.Rounds, r)
	switch r.Kind {
	case "disk":
		ss.stats.DiskBytes += r.Bytes
		ss.stats.DiskWireBytes += r.WireBytes
		line := fmt.Sprintf("%s: %d blocks, %s (%s on the wire) in %.2f s, %s/s", r.Name, r.Blocks,
			humanBytes(r.Bytes), humanBytes(r.WireBytes), r.Seconds, humanBytes(perSec(r.WireBytes, r.Seconds)))
		if r.DirtyPerSec > 0 {
			line += fmt.Sprintf(", guest wrote %.0f blocks/s", r.DirtyPerSec)
		}
		ss.progress("%s", line)
	case "ram":
		ss.stats.RAMBytes += r.Bytes
		ss.progress("%s: %s in %.2f s, %s/s", r.Name, humanBytes(r.Bytes), r.Seconds,
			humanBytes(perSec(r.Bytes, r.Seconds)))
	default:
		ss.stats.StateBytes += r.Bytes
	}
}

// perSec is n per second over secs.
func perSec(n int64, secs float64) int64 { return int64(float64(n) / max(secs, 1e-3)) }

// diskPreCopy sends the disk rounds that run while the guest does: round 0,
// every block that differs from the base, then the blocks written since,
// until a round is small or the cap is reached.
func (ss *streamSender) diskPreCopy(basePath string) error {
	started := time.Now()
	r := streamRound{Kind: "disk", Name: "disk round 0"}
	read, err := ss.diskDirty("boot")
	switch {
	case err == nil:
		ss.hviDisk = true
		ss.lastRound = read.Round
	case hviLacksOp(err):
	default:
		return fmt.Errorf("hvi's disk-dirty: %w", err)
	}
	fromBoot := ss.state.Base != nil && (ss.state.Base.FromBoot || hviCarriesBase(ss.state, ss.state.Base.SHA256))
	if ss.hviDisk && fromBoot {
		// hvi's blocks since boot are every difference from the base.
		ss.stats.DiskEngine = "hvi"
		if err := ss.hviRoundBlocks(&r, read, false); err != nil {
			return err
		}
	} else {
		// The running hvi did not boot from the base (it was restored),
		// or keeps no rounds: compare. With hvi's rounds, the read above
		// started round 0, so whatever the guest writes from here is in
		// the next round, the comparison's torn reads included.
		ss.stats.DiskEngine = "compare"
		cmp, err := newDiskCompare(basePath, ss.size)
		if err != nil {
			return fmt.Errorf("read the base: %w", err)
		}
		ss.cmp = cmp
		b := newBatcher(ss.size, ss.sendBatch(&r, ss.beginDiskRound(0, false, nil), 0, false))
		if err := cmp.diff(ss.disk, b.add); err != nil {
			return err
		}
		if err := b.done(); err != nil {
			return err
		}
	}
	ss.endRound(r, started)
	if !ss.hviDisk {
		return ss.sw.flush()
	}
	prev := time.Since(started).Seconds()
	for round := 1; round < streamRoundCap; round++ {
		started := time.Now()
		read, err := ss.diskDirty("round")
		if err != nil {
			return fmt.Errorf("hvi's disk-dirty: %w", err)
		}
		ss.lastRound = read.Round
		r := streamRound{Kind: "disk", Name: fmt.Sprintf("disk round %d", read.Round),
			DirtyPerSec: float64(read.Dirty) / max(prev, 1e-3)}
		if err := ss.hviRoundBlocks(&r, read, false); err != nil {
			return err
		}
		ss.endRound(r, started)
		prev = time.Since(started).Seconds()
		ss.stats.DiskWriteRate = max(ss.stats.DiskWriteRate, r.DirtyPerSec*diskBlockSize)
		if read.Dirty <= streamDiskSettle {
			break
		}
	}
	return ss.sw.flush()
}

// hviCarriesBase reports whether the running hvi's machine profile names
// base: an hvi that does carries its blocks written since boot across a
// restore (from disk.json), so they still cover every difference from the
// base on a restored guest.
func hviCarriesBase(state *store.InstanceState, base string) bool {
	if state.ProfilePath == "" {
		return false
	}
	data, err := os.ReadFile(state.ProfilePath)
	if err != nil {
		return false
	}
	var p struct {
		DiskBase string `json:"disk_base_sha256"`
	}
	return json.Unmarshal(data, &p) == nil && p.DiskBase != "" && p.DiskBase == base
}

// diskDirty reads hvi's dirty blocks since boot or since the last round,
// and holds its bitmap to the disk this host sees: the same block size, the
// same number of blocks, as many set as it says.
func (ss *streamSender) diskDirty(since string) (*hviDiskDirtyReply, error) {
	read, err := hviDiskDirty(ss.state.ControlSock, since)
	if err != nil {
		return nil, err
	}
	if _, err := read.dirtyBlocks(ss.size); err != nil {
		return nil, err
	}
	return read, nil
}

// hviRoundBlocks sends hvi's blocks of the round dirty started, which the
// compare engine then holds as sent. A round with no blocks is asked for
// too, so hvi records its sent_sha256 like any other's (disk.json), and a
// destination can tell it went out.
func (ss *streamSender) hviRoundBlocks(r *streamRound, dirty *hviDiskDirtyReply, final bool) error {
	round := dirty.Round
	ss.roundTotal = dirty.Dirty
	defer func() { ss.roundTotal = 0 }()
	d := ss.beginDiskRound(round, final, dirty)
	send := ss.sendBatch(r, d, round, final)
	sent, err := hviDiskBlocks(ss.state.ControlSock, round, func(first, count int64, data []byte) error {
		if ss.cmp != nil {
			for i := int64(0); i < count; i++ {
				lo := i * diskBlockSize
				block := data[lo:min(lo+diskBlockSize, int64(len(data)))]
				h := blockHash{sum: sha256.Sum256(block)}
				if isZero(block) {
					h = blockHash{zero: true}
				}
				ss.cmp.sent[first+i] = h
			}
		}
		return send(first, count, data)
	})
	if err != nil {
		return fmt.Errorf("hvi's disk-blocks of round %d: %w", round, err)
	}
	if got := d.done(); got.SentSHA256 != sent {
		return fmt.Errorf("round %d went out as batches whose digests make %s, hvi sent %s", round,
			got.SentSHA256, sent)
	}
	return nil
}

// diskFinal sends the blocks the guest wrote since the last disk round,
// now that hvi has fenced it and synced its disk.
func (ss *streamSender) diskFinal() error {
	started := time.Now()
	r := streamRound{Kind: "disk", Name: "disk final round", Final: true}
	if ss.hviDisk {
		read, err := ss.diskDirty("round")
		if err != nil {
			return fmt.Errorf("hvi's disk-dirty after the fence: %w", err)
		}
		r.Name = fmt.Sprintf("disk round %d (final)", read.Round)
		if err := ss.hviRoundBlocks(&r, read, true); err != nil {
			return err
		}
	} else {
		b := newBatcher(ss.size, ss.sendBatch(&r, ss.beginDiskRound(streamRoundCap, true, nil), streamRoundCap, true))
		if err := ss.cmp.diff(ss.disk, b.add); err != nil {
			return err
		}
		if err := b.done(); err != nil {
			return err
		}
	}
	ss.endRound(r, started)
	ss.stats.FinalDiskMs = time.Since(started).Milliseconds()
	ss.stats.FinalDiskBytes = r.Bytes
	return nil
}

// spoolWriter keeps hvi's stream on this host as hvi wrote it, and the
// small components beside it as files. Nothing syncs it while the stream
// runs: on a busy disk a sync of the spool held up the synced records the
// stopped guest waits for. The seal syncs it, behind the move.
type spoolWriter struct {
	dir string
	f   *os.File
	cw  *countingWriter
	bw  *bufio.Writer
}

// spoolSyncEvery is how much a destination writes to the incoming disk
// between the syncs it runs in the background while the stream goes on.
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

// spooledStateMax bounds the non-RAM components kept in memory to be
// written beside the spool.
const spooledStateMax = 64 << 20

// roundFolder folds hvi's frames into rounds for the record and the
// progress lines: hvi cuts a RAM round into frames of about 1 MiB, all
// named after the round.
type roundFolder struct {
	ss      *streamSender
	cur     *streamRound
	started time.Time
	printed time.Time
	lastRAM time.Time
	ramSeen bool
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
	if isRAMRound(name) {
		kind = "ram"
		f.lastRAM, f.ramSeen = now, true
	} else if f.ramSeen && f.ss.stats.PausedAt.IsZero() {
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
	if now := time.Now(); f.cur.Kind == "ram" && now.Sub(f.printed) >= progressEvery {
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
// fenced the guest.
func (ss *streamSender) relayHvi(sp *spoolWriter) (json.RawMessage, map[string][]byte, error) {
	m := ss.m
	conn, br, err := hviStreamOp(ss.state.ControlSock, map[string]any{"op": "migrate", "then": hviThenFence,
		"txid": m.rec.TxID, "epoch": m.rec.Epoch}, hviStreamIdle)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()
	files := map[string][]byte{}
	var kept int64
	folder := &roundFolder{ss: ss}
	reply, line, err := readHviComponents("migrate", br, func(line []byte, hdr hviComponentHeader, body io.Reader) error {
		if _, err := sp.bw.Write(append(append([]byte{}, line...), '\n')); err != nil {
			return err
		}
		var also io.Writer = sp.bw
		var keep *bytesBuffer
		if !isRAMRound(hdr.Component) {
			if kept+hdr.Size > spooledStateMax {
				return fmt.Errorf("hvi's %s of %d bytes is not a RAM round and too large to keep", hdr.Component, hdr.Size)
			}
			kept += hdr.Size
			keep = &bytesBuffer{}
			also = io.MultiWriter(sp.bw, keep)
		}
		if err := ss.relayFrame(folder, line, hdr, body, also); err != nil {
			return err
		}
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
	if err != nil {
		return nil, nil, err
	}
	if ss.manifest == nil {
		return nil, nil, errors.New("hvi sealed its migrate stream without a manifest.json")
	}
	ss.takeHviTimes(reply)
	return line, files, nil
}

// takeHviTimes takes, from hvi's reply, when the pre-copy rounds ended and
// when hvi sealed the stream, by this host's clock: closer to the pause than
// what hull sees of the frames.
func (ss *streamSender) takeHviTimes(reply *hviReply) {
	var st struct {
		PrecopyEnd int64 `json:"precopy_end_unix_ms"`
		SealedAt   int64 `json:"sealed_at_unix_ms"`
	}
	if reply != nil && json.Unmarshal(reply.Stats, &st) == nil && st.PrecopyEnd > 0 {
		ss.stats.PausedAt = time.UnixMilli(st.PrecopyEnd).UTC()
		if st.SealedAt >= st.PrecopyEnd {
			ss.stats.SealedMs = st.SealedAt - st.PrecopyEnd
			return
		}
	}
	if !ss.stats.PausedAt.IsZero() {
		ss.stats.SealedMs = time.Since(ss.stats.PausedAt).Milliseconds()
	}
}

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
	folder := &roundFolder{ss: ss}
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
	return json.RawMessage(strings.TrimSpace(string(reply))), nil
}

// streamTransfer is transfer for a streamed move. A destination that has
// verified the move already is not sent it again. A guest that hvi has not
// fenced yet is sent live; one whose stream hvi sealed in an earlier
// attempt is sent from the spool, with the disk compared against the base.
func (m *moveSource) streamTransfer(ctx context.Context) (retErr error) {
	if reply, err := m.peer.call(ctx, "migrate-status", []string{"--txid", m.rec.TxID}, nil); err == nil &&
		reply.Record != nil &&
		migrateStateOrder(roleDestination, reply.Record.State) >= migrateStateOrder(roleDestination, dstVerified) {
		if m.rec.Disk == nil || m.rec.Stream == nil {
			return errors.New("the destination has verified this move, but this host has no record of what it sent")
		}
		if err := m.checkDiskUnchanged(); err != nil {
			return err
		}
		if err := m.compareStreamReceived(reply.Record); err != nil {
			return err
		}
		m.destPaused = reply.Record.State == dstRestoredPaused
		return m.rec.advance(m.path, srcTransferred)
	}
	state, err := m.s.GetInstance(m.name)
	if err != nil {
		return err
	}
	replay := fencedFor(state, m.rec.TxID)
	if !replay && hviVMState(state) == store.MigrationFenced {
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
	f, err := os.Open(disk)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
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
	sw := newMoveStreamWriter(link)
	attempt, err := newMoveTxID()
	if err != nil {
		return err
	}
	m.sealGate, m.sealGateOnce = make(chan struct{}), sync.Once{}
	ss := &streamSender{m: m, state: state, sw: sw, link: link, disk: f, size: info.Size(), stats: stats,
		spoolDir: spoolDir, ctx: ctx, attempt: attempt, callDone: make(chan struct{}), sealGate: m.sealGate}
	type sendResult struct {
		hviReply json.RawMessage
		err      error
	}
	sent := make(chan sendResult, 1)
	go func() {
		reply, err := ss.send(replay)
		_ = pw.CloseWithError(err)
		sent <- sendResult{reply, err}
	}()
	reply, err := m.peer.call(ctx, "migrate-receive", []string{"--stream", "--txid", m.rec.TxID, "--name", m.name}, pr)
	close(ss.callDone)
	_ = pr.CloseWithError(errors.New("the destination stopped reading"))
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
			err = fmt.Errorf("%w (sending: %v)", err, res.err)
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
	m.logf("Sent %s in %.1f s (%s/s): disk %s as %s, RAM %s, state %s; destination: %s",
		humanBytes(stats.WireBytes), stats.Seconds, humanBytes(perSec(stats.WireBytes, stats.Seconds)),
		humanBytes(stats.DiskBytes), humanBytes(stats.DiskWireBytes), humanBytes(stats.RAMBytes),
		humanBytes(stats.StateBytes), reply.Record.Verify)
	if m.destPaused {
		// The destination holds the guest restored paused: the source's
		// commit point, recorded with the transfer in one write, as the
		// prepare step would record it.
		return m.rec.advanceThrough(m.path, srcTransferred, srcCommitting)
	}
	return m.rec.advance(m.path, srcTransferred)
}

// send writes the whole stream. Live, it ends with hvi having fenced the
// guest, the fence recorded on the instance and in the move's record.
func (ss *streamSender) send(replay bool) (json.RawMessage, error) {
	m := ss.m
	if err := ss.sw.header(moveStreamHeader{TxID: m.rec.TxID, Name: m.name, Epoch: m.rec.Epoch,
		BaseSHA256: m.rec.BaseSHA256, DiskSize: ss.size, BlockSize: diskBlockSize, Replay: replay,
		Attempt: ss.attempt}); err != nil {
		return nil, err
	}
	if err := ss.sw.flush(); err != nil {
		return nil, err
	}
	if !replay {
		// A refusal of the header (no base, another profile, no room
		// for the base's copy) must come while the guest still runs:
		// once hvi's migrate has begun, hull reads it to the end.
		if err := ss.waitAccepted(); err != nil {
			return nil, err
		}
	}
	basePath := m.s.BasePath(m.rec.BaseSHA256)
	if replay {
		// The guest is fenced: one comparison is the whole disk round.
		ss.stats.DiskEngine = "compare"
		cmp, err := newDiskCompare(basePath, ss.size)
		if err != nil {
			return nil, fmt.Errorf("read the base: %w", err)
		}
		ss.cmp = cmp
		started := time.Now()
		r := streamRound{Kind: "disk", Name: "disk round 0 (final)", Final: true}
		b := newBatcher(ss.size, ss.sendBatch(&r, ss.beginDiskRound(0, true, nil), 0, true))
		if err := cmp.diff(ss.disk, b.add); err != nil {
			return nil, err
		}
		if err := b.done(); err != nil {
			return nil, err
		}
		ss.endRound(r, started)
		reply, err := ss.replayHvi(ss.spoolDir)
		if err != nil {
			return nil, err
		}
		if err := ss.sw.trailer(reply, ss.diskTrailer()); err != nil {
			return nil, err
		}
		return reply, ss.link.err
	}

	// Everything the migrate op needs is set up first, so the last disk
	// round runs right before it: the guest's writes from then on are all
	// the final round holds.
	unlock, err := lockInstanceMigration(m.s, m.name)
	if err != nil {
		return nil, err
	}
	sp, err := newSpool(ss.spoolDir)
	if err != nil {
		unlock()
		return nil, err
	}
	if err := ss.diskPreCopy(basePath); err != nil {
		sp.close()
		unlock()
		return nil, err
	}
	if err := ss.link.err; err != nil {
		sp.close()
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
	// the last disk round, the trailer and the destination's checks, and
	// the spool is synced behind the move: on a busy disk every synced
	// write is a wait the stopped guest would otherwise sit through. A hull
	// that dies before they are done leaves a spool that a re-run or an
	// abort checks whole before using it (verifySpool).
	if err := ss.noteFence(); err != nil {
		m.signals.release()
		sp.close()
		unlock()
		return nil, err
	}
	ss.progress("hvi sealed the stream and fenced %s (%d ms after the last RAM round began)", m.name,
		ss.stats.SealedMs)
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
		sendErr = ss.diskFinal()
	}
	if sendErr == nil {
		sendErr = ss.sw.trailer(reply, ss.diskTrailer())
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
	for {
		reply, err := m.peer.call(ss.ctx, "migrate-status", []string{"--txid", m.rec.TxID}, nil)
		if err == nil && reply.Record != nil && reply.Record.StreamAccepted == ss.attempt {
			ss.stats.step("destination accepted")
			if took := time.Since(started); took > time.Second {
				ss.progress("the destination took the stream in %.1f s (it copied the base)", took.Seconds())
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
type softLink struct {
	w   io.Writer
	err error
}

func (l *softLink) Write(p []byte) (int, error) {
	if l.err != nil {
		return len(p), nil
	}
	if _, err := l.w.Write(p); err != nil {
		l.err = err
	}
	return len(p), nil
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
		SavedAt: time.Now().UTC(), TxID: m.rec.TxID,
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
