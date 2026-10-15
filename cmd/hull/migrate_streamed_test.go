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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// fakeStreamHvi is a source hvi that speaks the streaming ops a streamed
// move drives, as hvi documents them, written here independently of hull's
// parser: disk-dirty and disk-blocks over a real disk file, and migrate,
// which streams RAM rounds, pauses, writes the state and the manifest and
// fences. Its "guest" writes disk blocks between the RAM rounds. It serves
// one connection at a time, as hvi does.
type fakeStreamHvi struct {
	t    *testing.T
	path string
	disk string
	size int64

	mu sync.Mutex
	// state is running, fenced or stopped.
	state     string
	diskOps   bool
	sinceBoot map[int64]bool
	sinceRead map[int64]bool
	round     int
	last      []int64
	sealFail  bool
	// refuseFenced makes migrate refuse a fenced guest, as no hvi does
	// today; hvi streams one again.
	refuseFenced bool
	// generation counts the guest's writes; rounds are the disk-dirty
	// reads since the last since-boot one, as disk.json lists them.
	generation uint64
	rounds     []fakeDiskRound
	// diskWritesAt are blocks the guest writes while hvi sends the disk
	// blocks of a round, by round: they land in the next round.
	diskWritesAt map[int][]int64
	// migrateReq is the last migrate request.
	migrateReq map[string]any
	writesAt   map[string][]int64
	emitted    []byte
	ops        []string
	migrations int
	// pid is the process standing in for this hvi, which stop ends.
	pid int
}

func newFakeStreamHvi(t *testing.T, disk string, diskOps bool) *fakeStreamHvi {
	t.Helper()
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeStreamHvi{t: t, path: filepath.Join(shortSocketDir(t), "c.sock"), disk: disk, size: info.Size(),
		state: "running", diskOps: diskOps, sinceBoot: map[int64]bool{}, sinceRead: map[int64]bool{}, round: -1,
		writesAt: map[string][]int64{}, diskWritesAt: map[int][]int64{}}
	ln, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			h.serve(conn)
			_ = conn.Close()
		}
	}()
	return h
}

// fakeDiskRound is one round as hvi records it in disk.json.
type fakeDiskRound struct {
	Round        int     `json:"round"`
	Generation   uint64  `json:"generation"`
	Blocks       int     `json:"blocks"`
	BitmapSHA256 string  `json:"bitmap_sha256"`
	SentSHA256   *string `json:"sent_sha256"`
}

// guestWrite has the guest write block index, as virtio-blk would: marked,
// then written.
func (h *fakeStreamHvi) guestWrite(index int64, b byte) {
	h.generation++
	h.sinceBoot[index] = true
	h.sinceRead[index] = true
	f, err := os.OpenFile(h.disk, os.O_WRONLY, 0)
	if err != nil {
		h.t.Error(err)
		return
	}
	defer func() { _ = f.Close() }()
	n := min(diskBlockSize, h.size-index*diskBlockSize)
	if _, err := f.WriteAt(bytes.Repeat([]byte{b}, int(n)), index*diskBlockSize); err != nil {
		h.t.Error(err)
	}
}

func (h *fakeStreamHvi) serve(conn net.Conn) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req map[string]any
	if json.Unmarshal(line, &req) != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	op, _ := req["op"].(string)
	h.ops = append(h.ops, op)
	w := bufio.NewWriter(conn)
	defer func() { _ = w.Flush() }()
	reply := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	switch op {
	case "status":
		reply(`{"v":1,"ok":true,"state":%q,"vcpus":1,"vcpus_parked":0}`, h.state)
	case "profile":
		reply(`{"v":1,"ok":true,"profile":{"profile_version":1,"checkpoint_format":2}}`)
	case "disk-dirty":
		if !h.diskOps {
			reply(`{"v":1,"ok":false,"code":"unknown_op","error":"no op disk-dirty"}`)
			return
		}
		var set map[int64]bool
		switch req["since"] {
		case "boot":
			h.round, set = 0, h.sinceBoot
		case "round":
			if h.round < 0 {
				reply(`{"v":1,"ok":false,"code":"refused","error":"no round 0 yet"}`)
				return
			}
			h.round++
			set = h.sinceRead
		}
		h.sinceRead = map[int64]bool{}
		h.last = h.last[:0]
		blocks := (h.size + diskBlockSize - 1) / diskBlockSize
		bits := make([]byte, (blocks+7)/8)
		for b := range set {
			bits[b/8] |= 1 << (b % 8)
			h.last = append(h.last, b)
		}
		sort.Slice(h.last, func(i, j int) bool { return h.last[i] < h.last[j] })
		if h.round == 0 {
			h.rounds = nil
		}
		bitmapSum := sha256.Sum256(bits)
		h.rounds = append(h.rounds, fakeDiskRound{Round: h.round, Generation: h.generation, Blocks: len(set),
			BitmapSHA256: hex.EncodeToString(bitmapSum[:])})
		reply(`{"v":1,"ok":true,"since":%q,"round":%d,"block_size":65536,"blocks":%d,"dirty":%d,"generation":%d,"bitmap_sha256":"%s","bitmap_b64":%q}`,
			req["since"], h.round, blocks, len(set), h.generation, hex.EncodeToString(bitmapSum[:]),
			base64.StdEncoding.EncodeToString(bits))
	case "disk-blocks":
		if int(req["round"].(float64)) != h.round {
			reply(`{"v":1,"ok":false,"code":"refused","error":"not the last round"}`)
			return
		}
		f, err := os.Open(h.disk)
		if err != nil {
			h.t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		batch, sent := 0, 0
		var digests strings.Builder
		for i := 0; i < len(h.last); {
			first, count := h.last[i], int64(1)
			for i+int(count) < len(h.last) && h.last[i+int(count)] == first+count && count < 16 {
				count++
			}
			end := min((first+count)*diskBlockSize, h.size)
			data := make([]byte, end-first*diskBlockSize)
			_, _ = f.ReadAt(data, first*diskBlockSize)
			sum := sha256.Sum256(data)
			reply(`{"component":"disk-blocks","batch":%d,"first":%d,"count":%d,"offset":%d,"size":%d,"sha256":"%s"}`,
				batch, first, count, first*diskBlockSize, len(data), hex.EncodeToString(sum[:]))
			_, _ = w.Write(data)
			digests.WriteString(hex.EncodeToString(sum[:]))
			batch++
			sent += len(data)
			i += int(count)
		}
		for _, b := range h.diskWritesAt[h.round] {
			h.guestWrite(b, byte(0xd0+h.round))
		}
		chain := sha256.Sum256([]byte(digests.String()))
		sentSum := hex.EncodeToString(chain[:])
		if n := len(h.rounds); n > 0 && h.rounds[n-1].Round == h.round {
			h.rounds[n-1].SentSHA256 = &sentSum
		}
		reply(`{"v":1,"ok":true,"round":%d,"blocks":%d,"bytes":%d,"batches":%d,"generation":%d,"max_lock_us":3,"sent_sha256":"%s"}`,
			h.round, len(h.last), sent, batch, h.generation, sentSum)
	case "migrate":
		// hvi streams a fenced guest again, unchanged: it has not run since
		// the stream that fenced it. refuseFenced stands for an hvi that
		// would not.
		if h.state == "fenced" && h.refuseFenced {
			reply(`{"v":1,"ok":false,"code":"fenced","error":"the VM was fenced after a checkpoint"}`)
			return
		}
		h.migrations++
		h.migrateReq = req
		h.emitted = nil
		// The manifest lists each component once, as hvi's does: its
		// frames' bytes, their sha256 in order, and how many.
		type listing struct {
			name   string
			size   int
			frames int
			h      hash.Hash
		}
		var comps []*listing
		// As hvi: a stream the client stops reading before the seal is a
		// failed capture, and the guest runs on.
		lost := false
		emit := func(name string, body []byte) {
			if lost {
				return
			}
			sum := sha256.Sum256(body)
			frame := fmt.Sprintf(`{"component":%q,"size":%d,"sha256":"%s"}`+"\n", name, len(body), hex.EncodeToString(sum[:]))
			_, _ = w.WriteString(frame)
			_, _ = w.Write(body)
			h.emitted = append(h.emitted, frame...)
			h.emitted = append(h.emitted, body...)
			if name != "manifest.json" {
				if n := len(comps); n == 0 || comps[n-1].name != name {
					comps = append(comps, &listing{name: name, h: sha256.New()})
				}
				c := comps[len(comps)-1]
				c.size += len(body)
				c.frames++
				c.h.Write(body)
			}
			if w.Flush() != nil {
				lost = true
				return
			}
			for _, b := range h.writesAt[name] {
				h.guestWrite(b, byte(len(name)+h.migrations))
			}
		}
		emit("profile.json", []byte(`{"profile_version":1,"checkpoint_format":2}`))
		// hvi cuts a round into frames of about 1 MiB, all of one name.
		for i := 0; i < 3; i++ {
			emit("ram-round-0", bytes.Repeat([]byte{'p', byte(i)}, 48<<10))
		}
		emit("ram-round-1", bytes.Repeat([]byte("dirt"), 4<<10))
		if h.sealFail {
			reply(`{"v":1,"ok":false,"code":"capture_failed","error":"the capture failed; the VM runs on"}`)
			return
		}
		emit("ram-round-2", bytes.Repeat([]byte("last"), 1<<10))
		for _, name := range []string{"cpu.json", "platform.json", "devices.json", "disk.json", "session.json"} {
			doc := []byte(`{"version":1,"of":"` + name + `"}`)
			if name == "disk.json" {
				// As hvi captures it with the guest fenced: the
				// generation and every round read so far.
				doc, _ = json.Marshal(map[string]any{"version": 1, "generation": h.generation,
					"rounds": append([]fakeDiskRound{}, h.rounds...)})
			}
			emit(name, doc)
		}
		var listed []string
		for _, c := range comps {
			listed = append(listed, fmt.Sprintf(`{"name":%q,"size":%d,"sha256":"%s","frames":%d}`, c.name, c.size,
				hex.EncodeToString(c.h.Sum(nil)), c.frames))
		}
		man := []byte(`{"checkpoint_format":2,"components":[` + strings.Join(listed, ",") + `]}`)
		emit("manifest.json", man)
		if lost {
			return
		}
		h.state = "fenced"
		sum := sha256.Sum256(man)
		now := time.Now().UnixMilli()
		reply(`{"v":1,"ok":true,"state":"fenced","manifest_sha256":"%s","stats":{"pause_ms":4,`+
			`"precopy_end_unix_ms":%d,"sealed_at_unix_ms":%d}}`, hex.EncodeToString(sum[:]), now-7, now)
	case "stop":
		h.state = "stopped"
		if h.pid > 0 {
			_ = syscall.Kill(h.pid, syscall.SIGKILL)
		}
		reply(`{"v":1,"ok":true,"state":"stopping"}`)
	default:
		reply(`{"v":1,"ok":false,"code":"unknown_op","error":"?"}`)
	}
}

// fakeDestRestore stands in for `hvi restore --from-stream` on the
// destination: it reads hvi's framing from its stdin, checking every
// component's sha256 as hvi would, and once its input ends with a manifest
// it reports the guest restored paused, as the bundle fixture's restore
// does. failNext makes the next restore refuse after reading its input.
type fakeDestRestore struct {
	mu       sync.Mutex
	got      [][]byte
	failNext int
	restores int
	// refuseProfile makes the next restore stop reading after
	// profile.json, as hvi does when the machine is not one it can build.
	refuseProfile bool
}

func (r *fakeDestRestore) install(t *testing.T) {
	prev := destStreamRestore
	t.Cleanup(func() { destStreamRestore = prev })
	destStreamRestore = func(s *store.Store, name, txid string) (*streamRestore, error) {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		r.mu.Lock()
		refuse := r.refuseProfile
		r.refuseProfile = false
		r.mu.Unlock()
		go func() {
			if refuse {
				line, _ := bufio.NewReader(pr).ReadBytes('\n')
				_ = pr.CloseWithError(errors.New("closed"))
				done <- fmt.Errorf("hvi refused the profile %s: the host is not compatible", bytes.TrimSpace(line))
				return
			}
			data, err := io.ReadAll(pr)
			if err == nil {
				err = checkHviFraming(data)
			}
			r.mu.Lock()
			r.got = append(r.got, data)
			r.mu.Unlock()
			done <- err
		}()
		return &streamRestore{
			in: pw,
			wait: func() error {
				if err := <-done; err != nil {
					return err
				}
				r.mu.Lock()
				fail := r.failNext > 0
				if fail {
					r.failNext--
				}
				r.restores++
				r.mu.Unlock()
				if fail {
					return errors.New("hvi refused the restore")
				}
				return fakeRestoredPaused(t, s, name, txid)
			},
			kill: func() { _ = pw.CloseWithError(errors.New("killed")) },
		}, nil
	}
}

// checkHviFraming reads hvi's framing as hvi would and checks each
// component, and that the manifest comes last.
func checkHviFraming(data []byte) error {
	br := bufio.NewReader(bytes.NewReader(data))
	last := ""
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("hvi's stream ended in a header: %w", err)
		}
		var c struct {
			Component string `json:"component"`
			Size      int64  `json:"size"`
			SHA256    string `json:"sha256"`
		}
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		body := make([]byte, c.Size)
		if _, err := io.ReadFull(br, body); err != nil {
			return fmt.Errorf("%s: %w", c.Component, err)
		}
		if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != c.SHA256 {
			return fmt.Errorf("%s arrived corrupted", c.Component)
		}
		last = c.Component
	}
	if last != "manifest.json" {
		return fmt.Errorf("hvi's stream ends with %q, not the manifest", last)
	}
	return nil
}

// fakeRestoredPaused records the instance as an hvi restore that reached
// restored-paused would: a process named hvi with a control socket.
func fakeRestoredPaused(t *testing.T, s *store.Store, name, txid string) error {
	var mu sync.Mutex
	resumed := false
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		mu.Lock()
		defer mu.Unlock()
		switch req["op"] {
		case "resume":
			resumed = true
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
		case "status":
			if resumed {
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
			}
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused"}`}
		case "stop":
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"stopping"}`}
		}
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"unknown_op","error":"?"}`}
	})
	pid, _ := startProcessAs(t, "hvi", "/bin/sleep", "60")
	st, err := s.GetInstance(name)
	if err != nil {
		return err
	}
	if err := checkMoveRestore(s, st, hviRestoreOptions{move: txid, paused: true}); err != nil {
		return err
	}
	st.Status, st.PID, st.ControlSock = "running", pid, stub.path
	st.Migration.State = store.MigrationRestoredPaused
	return s.SaveInstance(st)
}

// streamFixture is the move fixture with a source that streams: an hvi of
// checkpoint format 2 behind the fake above, a base kept before the guest
// wrote, and a guest that wrote two blocks since its boot.
type streamFixture struct {
	*moveFixture
	hvi  *fakeStreamHvi
	dest *fakeDestRestore
	peer *localPeer
}

func newStreamFixture(t *testing.T, diskOps, fromBoot bool) *streamFixture {
	t.Helper()
	f := newMoveFixture(t, true)
	sum, err := keepBase(f.src, f.disk)
	if err != nil {
		t.Fatal(err)
	}
	h := newFakeStreamHvi(t, f.disk, diskOps)
	h.pid = f.vm.pid
	// Written since the boot: one where the base has data, one in a hole,
	// and one zeroed where the base had data.
	h.guestWrite(0, 0xa1)
	h.guestWrite(700, 0xa2)
	h.guestWrite(48, 0)
	state := mustInstance(t, f.src, f.name)
	state.ControlSock = h.path
	state.ProfilePath = filepath.Join(f.src.InstanceDir(f.name), "profile.json")
	if err := os.WriteFile(state.ProfilePath, []byte(`{"profile_version":1,"checkpoint_format":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state.Base = &store.BaseState{SHA256: sum, FromBoot: fromBoot}
	if err := f.src.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	dest := &fakeDestRestore{}
	dest.install(t)
	return &streamFixture{moveFixture: f, hvi: h, dest: dest, peer: newLocalPeer(f.dst)}
}

func (f *streamFixture) migrate(t *testing.T, txid string) error {
	t.Helper()
	opt := f.options(false)
	opt.txid = txid
	return sourceMigrate(context.Background(), f.src, f.name, opt, f.peer)
}

// checkArrived checks what the destination ended with: the source's disk,
// byte for byte, and the components hvi emitted, as it emitted them.
func (f *streamFixture) checkArrived(t *testing.T, rec *migrationRecord) {
	t.Helper()
	assertSameContent(t, f.disk, filepath.Join(f.dst.InstanceDir(f.name), "bundle", "rootfs.ext4"))
	f.dest.mu.Lock()
	got := f.dest.got[len(f.dest.got)-1]
	f.dest.mu.Unlock()
	f.hvi.mu.Lock()
	emitted := f.hvi.emitted
	f.hvi.mu.Unlock()
	if !bytes.Equal(got, emitted) {
		t.Errorf("the destination's hvi read %d bytes, the source's hvi wrote %d", len(got), len(emitted))
	}
	dst := f.dstRecord(t, rec.TxID)
	if dst.Transport != transportStream || dst.Stream == nil || dst.Stream.Chain != rec.Stream.Chain {
		t.Errorf("destination record %+v", dst)
	}
}

// A streamed move with a fake hvi on both ends: the base goes first, the
// disk rounds carry what the guest wrote since boot and during the RAM
// rounds, hvi's stream arrives whole at the destination's hvi, and the move
// commits with its numbers on record.
func TestStreamedMove(t *testing.T) {
	f := newStreamFixture(t, true, true)
	// The guest writes during the RAM rounds: these land in the last disk
	// round, read after hvi fenced it.
	f.hvi.writesAt["ram-round-0"] = []int64{1, 2, 900}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || rec.Transport != transportStream || rec.Stream == nil {
		t.Fatalf("source record %+v", rec)
	}
	st := rec.Stream
	if st.DiskEngine != "hvi" || st.Mode != "live" {
		t.Errorf("engine %s mode %s", st.DiskEngine, st.Mode)
	}
	var final *streamRound
	ram := 0
	for i, r := range st.Rounds {
		switch {
		case r.Kind == "disk" && r.Final:
			final = &st.Rounds[i]
		case r.Kind == "ram":
			ram++
		}
	}
	if ram != 3 || final == nil || final.Blocks != 3 || st.Rounds[0].Blocks != 3 {
		t.Errorf("rounds %+v", st.Rounds)
	}
	if st.PausedAt.IsZero() || st.SealedMs != 7 || st.DowntimeMs <= 0 || st.RAMBytes != 3*96<<10+16<<10+4<<10 ||
		st.WireBytes == 0 {
		t.Errorf("numbers %+v", st)
	}
	if !baseHeld(f.dst, rec.BaseSHA256) || f.peer.called("migrate-take-base") != 1 {
		t.Errorf("the base was not sent first: held %v", baseHeld(f.dst, rec.BaseSHA256))
	}
	f.checkArrived(t, rec)
	if f.peer.called("migrate-prepare") != 0 {
		t.Error("the source asked for a restore the streamed receive had done")
	}
	// The source keeps the stream it sent, sealed, for an abort that
	// never came, and the destination keeps hvi's small documents.
	for _, name := range []string{spoolStream, spoolSealed, "manifest.json"} {
		if _, err := os.Stat(filepath.Join(rec.BundleDir, name)); err != nil {
			t.Errorf("source spool: %v", err)
		}
	}
	dst := f.dstRecord(t, rec.TxID)
	if _, err := os.Stat(filepath.Join(dst.BundleDir, "cpu.json")); err != nil {
		t.Errorf("destination documents: %v", err)
	}
	if dst.State != dstRunning {
		t.Errorf("destination %s", dst.State)
	}
	state := mustInstance(t, f.dst, f.name)
	if state.Base == nil || state.Base.SHA256 != rec.BaseSHA256 || state.Base.FromBoot ||
		!isStreamRestore(state.CmdLine) {
		t.Errorf("destination instance %+v %v", state.Base, state.CmdLine)
	}
	if _, err := hviRestoreArgv(state.CmdLine, "/b", "/c", "/p"); err != nil {
		t.Errorf("the moved instance cannot be checkpointed and moved on: %v", err)
	}
}

// Without hvi's dirty-block rounds, or when the running hvi did not boot
// from the base and does not say it carries the base, hull compares the
// disk with the base instead; a restored hvi whose profile names the base
// carries its blocks since boot, and its rounds are used. Either way the
// destination ends with the source's disk.
func TestStreamedMoveComparesWithoutHviRounds(t *testing.T) {
	for _, tc := range []struct {
		name              string
		diskOps, fromBoot bool
		carries           bool
		engine            string
	}{
		{"no disk ops", false, true, false, "compare"},
		{"restored hvi", true, false, false, "compare"},
		{"restored hvi carrying the base", true, false, true, "hvi"},
		{"files written before boot", true, false, false, "compare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t, tc.diskOps, tc.fromBoot)
			switch tc.name {
			case "restored hvi":
				// hvi restored the guest and, its profile naming no
				// base, started its since-boot blocks afresh: the
				// writes before the restore are only on the disk.
				f.hvi.mu.Lock()
				f.hvi.sinceBoot = map[int64]bool{}
				f.hvi.mu.Unlock()
			case "files written before boot":
				// As `hull run` writes an instance's own files over its
				// image's base before hvi boots: on the disk, in no
				// bitmap of hvi's.
				f.hvi.mu.Lock()
				f.hvi.sinceBoot = map[int64]bool{}
				f.hvi.mu.Unlock()
				fh, err := os.OpenFile(f.disk, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fh.WriteAt(bytes.Repeat([]byte{0x5e}, 4096), 9*diskBlockSize); err != nil {
					t.Fatal(err)
				}
				_ = fh.Close()
			}
			if tc.carries {
				st := mustInstance(t, f.src, f.name)
				prof := fmt.Sprintf(`{"profile_version":1,"checkpoint_format":2,"disk_base_sha256":%q}`, st.Base.SHA256)
				if err := os.WriteFile(st.ProfilePath, []byte(prof), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.hvi.writesAt["ram-round-1"] = []int64{3, 701}
			if err := f.migrate(t, ""); err != nil {
				t.Fatal(err)
			}
			rec := f.srcRecord(t)
			if rec.State != srcCommitted || rec.Stream.DiskEngine != tc.engine {
				t.Fatalf("record %s engine %s", rec.State, rec.Stream.DiskEngine)
			}
			f.checkArrived(t, rec)
		})
	}
}

// A capture that fails before hvi seals its stream leaves the guest running
// here: the move stops in transferring, not fenced, and run again it
// streams from the start and commits.
func TestStreamedMoveCaptureFailsBeforeTheSeal(t *testing.T) {
	f := newStreamFixture(t, true, true)
	f.hvi.sealFail = true
	err := f.migrate(t, "")
	if err == nil || !strings.Contains(err.Error(), "capture_failed") || !strings.Contains(err.Error(), "still runs here") {
		t.Fatalf("err = %v", err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcTransferring || fencedFor(mustInstance(t, f.src, f.name), rec.TxID) {
		t.Fatalf("after the failed capture: %s", rec.State)
	}
	if dst := f.dstRecord(t, rec.TxID); dst.State != dstReceiving || dst.Error == "" {
		t.Errorf("destination %s %q", dst.State, dst.Error)
	}
	f.hvi.sealFail = false
	if err := f.migrate(t, rec.TxID); err != nil {
		t.Fatal(err)
	}
	rec = f.srcRecord(t)
	if rec.State != srcCommitted || rec.Stream.Mode != "live" {
		t.Fatalf("the retry: %s %s", rec.State, rec.Stream.Mode)
	}
	f.checkArrived(t, rec)
}

// damageAt returns a mangle that flips one byte at offset off of a verb's
// stdin, or, with cut, ends it there.
func damageAt(verb string, off int64, cut bool, times *int) func(string, []string, io.Reader) io.Reader {
	return func(v string, args []string, in io.Reader) io.Reader {
		if v != verb || *times == 0 || (verb == "migrate-receive" && !slices.Contains(args, "--stream")) {
			return in
		}
		*times--
		if cut {
			return io.LimitReader(in, off)
		}
		return &flipReader{r: in, at: off}
	}
}

type flipReader struct {
	r   io.Reader
	at  int64
	pos int64
}

func (f *flipReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if f.at >= f.pos && f.at < f.pos+int64(n) {
		p[f.at-f.pos] ^= 0x20
	}
	f.pos += int64(n)
	return n, err
}

// A stream damaged or cut on the way, after hvi sealed it, is refused by
// the destination, which keeps nothing running. The source is fenced with
// its spooled copy: run again, the move replays the spool, compares the
// disk with the base, and commits.
func TestStreamedMoveRefusedInTransitThenReplayed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  bool
		want string
	}{{"corrupted", false, "corrupted"}, {"truncated", true, "ended"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t, true, true)
			// Into the RAM rounds: past the header and the first disk
			// round (three blocks, compressible), well before the end.
			times := 1
			f.peer.mangle = damageAt("migrate-receive", 120<<10, tc.cut, &times)
			err := f.migrate(t, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "stays fenced") {
				t.Fatalf("err = %v", err)
			}
			rec := f.srcRecord(t)
			state := mustInstance(t, f.src, f.name)
			if rec.State != srcTransferring || !fencedFor(state, rec.TxID) {
				t.Fatalf("source %s, instance %+v", rec.State, state.Migration)
			}
			dst := f.dstRecord(t, rec.TxID)
			if dst.State != dstReceiving || f.dest.restores != 0 {
				t.Fatalf("destination %s, restores %d", dst.State, f.dest.restores)
			}
			if err := f.migrate(t, rec.TxID); err != nil {
				t.Fatal(err)
			}
			rec = f.srcRecord(t)
			if rec.State != srcCommitted || rec.Stream.Mode != "replay" || rec.Stream.DiskEngine != "compare" {
				t.Fatalf("the replay: %s %+v", rec.State, rec.Stream)
			}
			f.checkArrived(t, rec)
			if f.hvi.migrations != 1 {
				t.Errorf("hvi ran migrate %d times; the replay must not ask a fenced hvi again", f.hvi.migrations)
			}
		})
	}
}

// A destination whose hvi refuses the restore after the seal leaves the
// source fenced; the abort brings the guest back from the spooled stream.
func TestStreamedMoveAbortRestoresFromTheSpool(t *testing.T) {
	f := newStreamFixture(t, true, true)
	f.dest.failNext = 1
	var restoredFrom *os.File
	localRestore = func(s *store.Store, name string) error {
		st, err := s.GetInstance(name)
		if err != nil {
			return err
		}
		restoredFrom, err = restoreStreamSource(st, hviRestoreOptions{detach: true})
		return err
	}
	err := f.migrate(t, "")
	if err == nil || !strings.Contains(err.Error(), "refused the restore") {
		t.Fatalf("err = %v", err)
	}
	rec := f.srcRecord(t)
	if err := sourceAbort(context.Background(), f.src, f.name, io.Discard, f.peer); err != nil {
		t.Fatal(err)
	}
	if restoredFrom == nil || restoredFrom.Name() != filepath.Join(rec.BundleDir, spoolStream) {
		t.Fatalf("the abort restored from %v", restoredFrom)
	}
	_ = restoredFrom.Close()
	state := mustInstance(t, f.src, f.name)
	if state.Migration.State != store.MigrationFenced || state.Migration.BundleDir != rec.BundleDir {
		t.Errorf("instance %+v", state.Migration)
	}
	if _, err := os.Stat(f.dst.InstanceDir(f.name)); !os.IsNotExist(err) {
		t.Errorf("the destination kept the instance: %v", err)
	}
	argv := streamRestoreArgv([]string{"hvi", "restore", "--from", rec.BundleDir, "--disk", "/d"})
	if strings.Join(argv, " ") != "hvi restore --from-stream - --disk /d" {
		t.Errorf("restore argv %v", argv)
	}
}

// A move that never got as far as hvi's seal aborts without touching the
// guest, which still runs here.
func TestStreamedMoveAbortBeforeTheSeal(t *testing.T) {
	f := newStreamFixture(t, true, true)
	f.hvi.sealFail = true
	if err := f.migrate(t, ""); err == nil {
		t.Fatal("the move went through")
	}
	restored := false
	localRestore = func(*store.Store, string) error { restored = true; return nil }
	var out bytes.Buffer
	if err := sourceAbort(context.Background(), f.src, f.name, &out, f.peer); err != nil {
		t.Fatal(err)
	}
	if restored || !strings.Contains(out.String(), "never stopped running here") {
		t.Errorf("restored %v; %s", restored, out.String())
	}
	if f.hvi.state != "running" || syscall.Kill(f.vm.pid, 0) != nil {
		t.Errorf("the guest is %s", f.hvi.state)
	}
}

// The transport is decided from what both hosts can do, and the reason is
// said when it is the whole bundle.
func TestDecideTransport(t *testing.T) {
	all := transportFacts{SourceFormat: 2, HasBase: true, DestStream: true, DestHviStream: true}
	if got, why := decideTransport(all); got != transportStream || why != "" {
		t.Fatalf("all there: %q %q", got, why)
	}
	for _, tc := range []struct {
		mut  func(*transportFacts)
		want string
	}{
		{func(f *transportFacts) { f.WholeBundle = true }, "--whole-bundle"},
		{func(f *transportFacts) { f.DestStream = false; f.DestHull = "v0.9" }, "hull there (v0.9) takes no streamed"},
		{func(f *transportFacts) { f.DestHviStream = false }, "cannot restore from a stream"},
		{func(f *transportFacts) { f.SourceFormat = 1 }, "checkpoint format 1"},
		{func(f *transportFacts) { f.SourceFormat = 0 }, "checkpoint format 0"},
		{func(f *transportFacts) { f.HasBase = false; f.BaseReason = "no base kept" }, "no base kept"},
	} {
		f := all
		tc.mut(&f)
		got, why := decideTransport(f)
		if got != transportBundle || !strings.Contains(why, tc.want) {
			t.Errorf("%+v: %q %q, want the bundle because %q", f, got, why, tc.want)
		}
	}
}

// The dry run of a streamed move says how it would go; one to a hull from
// before streamed moves says it would send the whole bundle, and why.
func TestStreamedMoveDryRun(t *testing.T) {
	f := newStreamFixture(t, true, true)
	var out bytes.Buffer
	opt := f.options(false)
	opt.dryRun, opt.out = true, &out
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, f.peer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "transport: streamed") || !strings.Contains(out.String(), "base is sent first") {
		t.Errorf("dry run: %s", out.String())
	}
	probe := destProbe(f.dst, f.name, f.moveFixture.hvi, "")
	probe.Probe.Capabilities = nil
	f.peer.answer["migrate-probe"] = probe
	out.Reset()
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, f.peer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "takes no streamed move") {
		t.Errorf("dry run to an old hull: %s", out.String())
	}
}

// A destination hvi that refuses the machine stops reading, and its
// refusal is the move's reason. The source still reads hvi's stream to its
// end into the spool: hvi may seal it, and fence the guest, from what sits
// in the socket's buffer, and a fenced source must have the whole stream
// to restore from. Here hvi seals; the abort brings the guest back from
// the spool.
func TestStreamedMoveDestinationHviRefuses(t *testing.T) {
	f := newStreamFixture(t, true, true)
	f.dest.refuseProfile = true
	err := f.migrate(t, "")
	if err == nil || !strings.Contains(err.Error(), "the host is not compatible") ||
		!strings.Contains(err.Error(), codeRestoreRefused) || !strings.Contains(err.Error(), "stays fenced") {
		t.Fatalf("err = %v", err)
	}
	rec := f.srcRecord(t)
	if !fencedFor(mustInstance(t, f.src, f.name), rec.TxID) {
		t.Fatal("the fence hvi sealed was not recorded")
	}
	if err := checkHviFramingFile(filepath.Join(rec.BundleDir, spoolStream)); err != nil {
		t.Fatalf("the spool: %v", err)
	}
	restored := false
	localRestore = func(*store.Store, string) error { restored = true; return nil }
	if err := sourceAbort(context.Background(), f.src, f.name, io.Discard, f.peer); err != nil || !restored {
		t.Fatalf("abort: %v, restored %v", err, restored)
	}
}

// checkHviFramingFile is checkHviFraming of a file.
func checkHviFramingFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return checkHviFraming(data)
}

// A guest that writes to its disk while the disk rounds run: each round
// carries what was written during the one before, the rounds go on until
// one is small, and the last, read after the fence, holds only what the
// guest wrote during hvi's RAM rounds.
func TestStreamedMoveDiskRoundsFollowTheGuestsWrites(t *testing.T) {
	f := newStreamFixture(t, true, true)
	// 40 blocks during round 0 (more than a round may settle at), then
	// 3 during round 1, then none.
	for b := int64(100); b < 140; b++ {
		f.hvi.diskWritesAt[0] = append(f.hvi.diskWritesAt[0], b)
	}
	f.hvi.diskWritesAt[1] = []int64{200, 201, 202}
	f.hvi.writesAt["ram-round-1"] = []int64{300}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	st := rec.Stream
	var blocks []int64
	for _, r := range st.Rounds {
		if r.Kind == "disk" {
			blocks = append(blocks, r.Blocks)
		}
	}
	if fmt.Sprint(blocks) != "[3 40 3 1]" {
		t.Errorf("disk rounds of %v blocks", blocks)
	}
	if st.DiskWriteRate <= 0 {
		t.Errorf("no disk write rate on record: %+v", st)
	}
	if _, ok := f.hvi.migrateReq["max_secs"]; ok {
		t.Errorf("hvi's pre-copy was limited: %v", f.hvi.migrateReq)
	}
	if st.FinalDiskBytes != diskBlockSize {
		t.Errorf("final disk round %d bytes", st.FinalDiskBytes)
	}
	f.checkArrived(t, rec)
}

// A destination that refuses the stream's header (here: it lost the base
// after the source checked) refuses it while the guest still runs: hvi's
// migrate never starts, the source sends the base, and the move goes on
// live, by itself.
func TestStreamedMoveSendsTheBaseOnNoBase(t *testing.T) {
	f := newStreamFixture(t, true, true)
	st := mustInstance(t, f.src, f.name)
	removed := 0
	f.peer.mangle = func(verb string, args []string, in io.Reader) io.Reader {
		if verb == "migrate-receive" && slices.Contains(args, "--stream") && removed == 0 {
			removed++
			if err := os.Remove(f.dst.BasePath(st.Base.SHA256)); err != nil {
				t.Error(err)
			}
		}
		return in
	}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || rec.Stream.Mode != "live" {
		t.Fatalf("source %s, stream %+v", rec.State, rec.Stream)
	}
	if f.hvi.migrations != 1 || removed != 1 {
		t.Errorf("hvi streamed %d times, the base was removed %d times", f.hvi.migrations, removed)
	}
	if n := f.peer.called("migrate-take-base"); n != 2 {
		t.Errorf("the base was sent %d times, want 2: %v", n, f.peer.calls)
	}
	f.checkArrived(t, rec)
}

// A destination that takes a base keeps a spare copy of it, which the
// move's receive takes as the instance's disk rather than copying the base
// while the source waits; a prepare makes the next one.
func TestStreamedMoveTakesTheSpareBase(t *testing.T) {
	f := newStreamFixture(t, true, true)
	st := mustInstance(t, f.src, f.name)
	sum := st.Base.SHA256
	if _, err := sendBase(t.Context(), f.src, f.peer, sum); err != nil {
		t.Fatal(err)
	}
	spare := spareBasePath(f.dst, sum)
	if _, err := os.Stat(spare); err != nil {
		t.Fatalf("no spare after the base came: %v", err)
	}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spare); !os.IsNotExist(err) {
		t.Errorf("the receive left the spare: %v", err)
	}
	f.checkArrived(t, f.srcRecord(t))
	if reply := destSpareBase(f.dst, sum); !reply.OK || reply.Detail == "held" {
		t.Errorf("prepare's spare: %+v", reply)
	}
	if reply := destSpareBase(f.dst, sum); !reply.OK || reply.Detail != "held" {
		t.Errorf("prepare's spare again: %+v", reply)
	}
	if err := removeBase(f.dst, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spare); !os.IsNotExist(err) {
		t.Errorf("the spare outlived its base: %v", err)
	}
}
