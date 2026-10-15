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
	"encoding/binary"
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

// fakeStreamHvi is a source hvi that speaks the migrate op as hvi documents
// it (docs/checkpoint.md, "Live moves"), written here independently of
// hull's parser: it streams a disk round before each RAM round while the
// guest runs (round 0 the blocks written since boot, each later one the
// blocks written since the round before), pauses, streams the final disk
// and RAM rounds, the documents and a manifest over all of them, and
// fences. A fenced guest is streamed again only for the txid and epoch
// that fenced it, with a manifest of its own. Its "guest" writes disk
// blocks between frames. It serves one connection at a time, as hvi does.
type fakeStreamHvi struct {
	t    *testing.T
	path string
	disk string
	size int64
	base string

	mu sync.Mutex
	// state is running, fenced or stopped.
	state string
	// sinceBoot are the blocks written since boot, sinceRound since the
	// last disk round; generation counts the guest's writes.
	sinceBoot  map[int64]bool
	sinceRound map[int64]bool
	generation uint64
	sealFail   bool
	// failRestream makes a second stream of a fenced guest fail before
	// its manifest, as one hvi cut for a stalled reader would.
	failRestream bool
	// fenceTxID and fenceEpoch are the migrate that fenced the guest;
	// firstManifest the sha256 of its manifest; docs the documents it
	// sent, which a second stream sends again unchanged.
	fenceTxID     string
	fenceEpoch    float64
	firstManifest string
	docs          map[string][]byte
	// migrateReq is the last migrate request.
	migrateReq map[string]any
	// writesAt are blocks the guest writes once the frame of that name is
	// out: they land in the next disk round.
	writesAt   map[string][]int64
	emitted    []byte
	ops        []string
	migrations int
	// bigDiskRound pads disk round 0 with this many more blocks of data
	// (they exist on the disk), to make a round larger than a document may
	// be.
	bigDiskRound int64
	// pid is the process standing in for this hvi, which stop ends.
	pid int
	// fencedNow is closed once the guest is fenced, before the reply;
	// pausedAtMs is when its reply says the pause came.
	fencedNow  chan struct{}
	pausedAtMs int64
	// restreamDocs replaces documents in a second stream, as an hvi
	// that did not keep them would.
	restreamDocs map[string][]byte
}

func newFakeStreamHvi(t *testing.T, disk, base string) *fakeStreamHvi {
	t.Helper()
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeStreamHvi{t: t, path: filepath.Join(shortSocketDir(t), "c.sock"), disk: disk, size: info.Size(),
		base: base, state: "running", sinceBoot: map[int64]bool{}, sinceRound: map[int64]bool{},
		writesAt: map[string][]int64{}, fencedNow: make(chan struct{})}
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

// guestWrite has the guest write block index, as virtio-blk would: marked,
// then written.
func (h *fakeStreamHvi) guestWrite(index int64, b byte) {
	h.generation++
	h.sinceBoot[index] = true
	h.sinceRound[index] = true
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

// fakeDiskRound is the payload of a disk round as hvi frames it: for each
// run of up to 16 adjacent blocks, ascending, a 24-byte little-endian
// header (first block, count, flags with bit 0 for raw, the blocks' length,
// the payload's length) and the blocks as they are.
func (h *fakeStreamHvi) fakeDiskRound(set map[int64]bool) ([]byte, int) {
	var blocks []int64
	for b := range set {
		blocks = append(blocks, b)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	f, err := os.Open(h.disk)
	if err != nil {
		h.t.Error(err)
		return nil, 0
	}
	defer func() { _ = f.Close() }()
	var out []byte
	for i := 0; i < len(blocks); {
		first, count := blocks[i], int64(1)
		for i+int(count) < len(blocks) && blocks[i+int(count)] == first+count && count < 16 {
			count++
		}
		end := min((first+count)*diskBlockSize, h.size)
		data := make([]byte, end-first*diskBlockSize)
		_, _ = f.ReadAt(data, first*diskBlockSize)
		var hdr [24]byte
		binary.LittleEndian.PutUint64(hdr[0:], uint64(first))
		binary.LittleEndian.PutUint32(hdr[8:], uint32(count))
		binary.LittleEndian.PutUint32(hdr[12:], 1)
		binary.LittleEndian.PutUint32(hdr[16:], uint32(len(data)))
		binary.LittleEndian.PutUint32(hdr[20:], uint32(len(data)))
		out = append(append(out, hdr[:]...), data...)
		i += int(count)
	}
	return out, len(blocks)
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
		reply(`{"v":1,"ok":true,"profile":{"profile_version":1,"checkpoint_format":3,"disk_base_sha256":%q}}`, h.base)
	case "migrate":
		h.migrate(req, w, reply)
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

// migrate streams the guest, or, fenced, streams it again for the txid and
// epoch that fenced it.
func (h *fakeStreamHvi) migrate(req map[string]any, w *bufio.Writer, reply func(string, ...any)) {
	again := h.state == "fenced"
	if again && (req["txid"] != h.fenceTxID || req["epoch"] != h.fenceEpoch || req["then"] != "fence" ||
		h.fenceTxID == "") {
		reply(`{"v":1,"ok":false,"code":"fenced","error":"the VM is fenced for another move"}`)
		return
	}
	h.migrations++
	h.migrateReq = req
	h.emitted = nil
	// The manifest lists each component once, as hvi's does: its frames'
	// bytes, their sha256 in order, and how many.
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
		if !again {
			for _, b := range h.writesAt[name] {
				h.guestWrite(b, byte(len(name)+h.migrations))
			}
		}
	}
	type diskRound struct {
		Round      int     `json:"round"`
		Generation uint64  `json:"generation"`
		Blocks     int     `json:"blocks"`
		SentSHA256 *string `json:"sent_sha256"`
	}
	var rounds []diskRound
	var statsRounds []string
	var diskRaw int64
	diskRoundOf := func(name string, k int, set map[int64]bool) {
		body, n := h.fakeDiskRound(set)
		diskRaw += int64(n) * diskBlockSize
		gen := h.generation
		h.sinceRound = map[int64]bool{}
		emit(name, body)
		sum := sha256.Sum256(body)
		sent := hex.EncodeToString(sum[:])
		rounds = append(rounds, diskRound{Round: k, Generation: gen, Blocks: n, SentSHA256: &sent})
		statsRounds = append(statsRounds, fmt.Sprintf(`{"name":%q,"round":%d,"in_pause":%v,"blocks":%d,`+
			`"raw_bytes":%d,"bytes":%d,"ms":2,"generation":%d}`, name, k, name == "disk-round-final", n,
			int64(n)*diskBlockSize, len(body), gen))
	}
	profile := []byte(fmt.Sprintf(`{"profile_version":1,"checkpoint_format":3,"disk_base_sha256":%q}`, h.base))
	if d, ok := h.restreamDocs["profile.json"]; ok && again {
		profile = d
	}
	emit("profile.json", profile)
	var docs []string
	if again {
		// The fenced guest again: one disk round of every block written
		// since boot, one RAM round, the documents as first sent.
		if h.failRestream {
			reply(`{"v":1,"ok":false,"code":"capture_failed","error":"the reader stalled"}`)
			return
		}
		diskRoundOf("disk-round-final", 0, h.sinceBoot)
		emit("ram-round-0", bytes.Repeat([]byte{'p', 0}, 64<<10))
		docs = []string{"cpu.json", "platform.json", "devices.json", "disk.json", "session.json"}
	} else {
		if h.bigDiskRound > 0 {
			for b := int64(0); b < h.bigDiskRound; b++ {
				h.guestWrite(1000+b, 0x77)
			}
		}
		diskRoundOf("disk-round-0", 0, h.sinceBoot)
		// hvi cuts a round into frames of about 1 MiB, all of one name.
		for i := 0; i < 3; i++ {
			emit("ram-round-0", bytes.Repeat([]byte{'p', byte(i)}, 48<<10))
		}
		diskRoundOf("disk-round-1", 1, h.sinceRound)
		emit("ram-round-1", bytes.Repeat([]byte("dirt"), 4<<10))
		if h.sealFail {
			reply(`{"v":1,"ok":false,"code":"capture_failed","error":"the capture failed; the VM runs on"}`)
			return
		}
		diskRoundOf("disk-round-final", 2, h.sinceRound)
		emit("ram-round-2", bytes.Repeat([]byte("last"), 1<<10))
		docs = []string{"cpu.json", "platform.json", "devices.json", "disk.json", "session.json"}
		h.docs = map[string][]byte{}
	}
	for _, name := range docs {
		doc, ok := h.docs[name]
		if !ok || name == "disk.json" {
			doc = []byte(`{"version":1,"of":"` + name + `"}`)
			if name == "disk.json" {
				doc, _ = json.Marshal(map[string]any{"version": 1, "base_sha256": h.base,
					"generation": h.generation, "rounds": rounds})
			}
			if !again {
				h.docs[name] = doc
			}
		}
		if d, ok := h.restreamDocs[name]; ok && again {
			doc = d
		}
		emit(name, doc)
	}
	var listed []string
	for _, c := range comps {
		listed = append(listed, fmt.Sprintf(`{"name":%q,"size":%d,"sha256":"%s","frames":%d}`, c.name, c.size,
			hex.EncodeToString(c.h.Sum(nil)), c.frames))
	}
	man := []byte(fmt.Sprintf(`{"checkpoint_format":3,"components":[%s],"sealed_unix_ms":%d}`,
		strings.Join(listed, ","), time.Now().UnixNano()))
	emit("manifest.json", man)
	if lost {
		return
	}
	sum := sha256.Sum256(man)
	now := time.Now().UnixMilli()
	if again {
		// As hvi's: the first manifest and what equals the first stream's
		// are in the stats, and the rounds' end and the pause are this
		// stream's seal.
		reply(`{"v":1,"ok":true,"state":"fenced","manifest_sha256":"%s","stats":{"first_manifest_sha256":"%s",`+
			`"same_as_first":["profile.json","cpu.json","platform.json","devices.json","session.json"],`+
			`"disk_rounds":[%s],"rounds_end_unix_ms":%d,"precopy_end_unix_ms":%d,"sealed_at_unix_ms":%d}}`,
			hex.EncodeToString(sum[:]), h.firstManifest, strings.Join(statsRounds, ","), now, now, now)
		return
	}
	h.state = "fenced"
	close(h.fencedNow)
	h.fenceTxID, _ = req["txid"].(string)
	h.fenceEpoch, _ = req["epoch"].(float64)
	h.firstManifest = hex.EncodeToString(sum[:])
	h.pausedAtMs = now - 7
	reply(`{"v":1,"ok":true,"state":"fenced","manifest_sha256":"%s","stats":{"pause_ms":4,"paused_ms":6,`+
		`"stop_reason":"converged","rounds":[{"round":0,"in_pause":false},{"round":1,"in_pause":false},`+
		`{"round":2,"in_pause":true}],"disk_rounds":[%s],"disk_raw_bytes":%d,"disk_sync_ms":1,"pause_sync_ms":1,`+
		`"rounds_end_unix_ms":%d,"precopy_end_unix_ms":%d,"sealed_at_unix_ms":%d}}`, hex.EncodeToString(sum[:]),
		strings.Join(statsRounds, ","), diskRaw, now-9, now-7, now)
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
		st, err := s.GetInstance(name)
		if err != nil {
			return nil, err
		}
		disk, _ := flagValue(st.CmdLine, "--disk")
		if st.Base == nil || disk == "" {
			return nil, fmt.Errorf("the incoming instance names no base (%+v) or no disk", st.Base)
		}
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
				err = applyHviStream(data, disk)
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
func checkHviFraming(data []byte) error { return applyHviStream(data, "") }

// applyHviStream reads hvi's framing as hvi would, checks each component,
// that the manifest comes last and lists every other component, and, when
// disk is not "", writes the disk rounds' blocks into it in order.
func applyHviStream(data []byte, disk string) error {
	var f *os.File
	if disk != "" {
		var err error
		if f, err = os.OpenFile(disk, os.O_WRONLY, 0); err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
	}
	br := bufio.NewReader(bytes.NewReader(data))
	last := ""
	seen := map[string]bool{}
	var manifest []byte
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
		if c.Component == "manifest.json" {
			manifest = body
			continue
		}
		seen[c.Component] = true
		if f != nil && strings.HasPrefix(c.Component, "disk-round-") {
			for p := body; len(p) > 0; {
				if len(p) < 24 {
					return fmt.Errorf("%s: a chunk header cut short", c.Component)
				}
				first := int64(binary.LittleEndian.Uint64(p[0:]))
				n := int(binary.LittleEndian.Uint32(p[20:]))
				if len(p) < 24+n {
					return fmt.Errorf("%s: a chunk cut short", c.Component)
				}
				if _, err := f.WriteAt(p[24:24+n], first*diskBlockSize); err != nil {
					return err
				}
				p = p[24+n:]
			}
		}
	}
	if last != "manifest.json" {
		return fmt.Errorf("hvi's stream ends with %q, not the manifest", last)
	}
	var man struct {
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
	}
	if err := json.Unmarshal(manifest, &man); err != nil {
		return err
	}
	if len(man.Components) != len(seen) {
		return fmt.Errorf("the manifest lists %d components, the stream has %d", len(man.Components), len(seen))
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
// checkpoint format 3 behind the fake above, booted against a base kept
// before the guest wrote, and a guest that wrote three blocks since its
// boot.
type streamFixture struct {
	*moveFixture
	hvi  *fakeStreamHvi
	dest *fakeDestRestore
	peer *localPeer
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	f := newMoveFixture(t, true)
	sum, err := keepBase(f.src, f.disk)
	if err != nil {
		t.Fatal(err)
	}
	h := newFakeStreamHvi(t, f.disk, sum)
	h.pid = f.vm.pid
	// Written since the boot: one where the base has data, one in a hole,
	// and one zeroed where the base had data.
	h.guestWrite(0, 0xa1)
	h.guestWrite(700, 0xa2)
	h.guestWrite(48, 0)
	state := mustInstance(t, f.src, f.name)
	state.ControlSock = h.path
	state.ProfilePath = filepath.Join(f.src.InstanceDir(f.name), "profile.json")
	prof := fmt.Sprintf(`{"profile_version":1,"checkpoint_format":3,"disk_base_sha256":%q}`, sum)
	if err := os.WriteFile(state.ProfilePath, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	state.Base = &store.BaseState{SHA256: sum}
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

// A streamed move with a fake hvi on both ends: the base goes first, hvi's
// stream carries the disk rounds with the RAM rounds, it arrives whole at
// the destination's hvi, which writes the disk rounds onto its copy of the
// base, and the move commits with hvi's numbers on record.
func TestStreamedMove(t *testing.T) {
	f := newStreamFixture(t)
	// The guest writes during the RAM rounds: these land in the next disk
	// round, the last of them in the one hvi takes paused.
	f.hvi.writesAt["ram-round-0"] = []int64{1, 2, 900}
	f.hvi.writesAt["ram-round-1"] = []int64{5}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || rec.Transport != transportStream || rec.Stream == nil {
		t.Fatalf("source record %+v", rec)
	}
	st := rec.Stream
	if st.Mode != "live" || st.DiskEngine != "" {
		t.Errorf("mode %s engine %q", st.Mode, st.DiskEngine)
	}
	var disk, ram []string
	for _, r := range st.Rounds {
		switch r.Kind {
		case "disk":
			disk = append(disk, fmt.Sprintf("%s:%d:%v", r.Name, r.Blocks, r.Final))
		case "ram":
			ram = append(ram, fmt.Sprintf("%s:%v", r.Name, r.Final))
		}
	}
	if strings.Join(disk, " ") != "disk-round-0:3:false disk-round-1:3:false disk-round-final:1:true" ||
		strings.Join(ram, " ") != "ram-round-0:false ram-round-1:false ram-round-2:true" {
		t.Errorf("rounds: disk %v, ram %v", disk, ram)
	}
	if st.PausedAt.IsZero() || st.RoundsEndAt.IsZero() || st.SealedMs != 7 || st.DowntimeMs <= 0 ||
		st.RAMBytes != 3*96<<10+16<<10+4<<10 || st.WireBytes == 0 || st.FinalDiskBytes != diskBlockSize ||
		st.DiskBytes != 7*diskBlockSize || st.DiskWireBytes == 0 || st.StopReason != "converged" ||
		st.HviPausedMs != 6 {
		t.Errorf("numbers %+v", st)
	}
	if !baseHeld(f.dst, rec.BaseSHA256) || f.peer.called("migrate-take-base") != 1 {
		t.Errorf("the base was not sent first: held %v", baseHeld(f.dst, rec.BaseSHA256))
	}
	if req := f.hvi.migrateReq; req["txid"] != rec.TxID || req["epoch"] != float64(rec.Epoch) {
		t.Errorf("hvi's migrate was asked without the move's txid and epoch: %v", req)
	}
	f.checkArrived(t, rec)
	if f.peer.called("migrate-prepare") != 0 {
		t.Error("the source asked for a restore the streamed receive had done")
	}
	// The source keeps the stream it sent, sealed, for an abort that
	// never came, and the destination keeps hvi's small documents.
	for _, name := range []string{spoolStream, spoolSealed, "manifest.json", "disk.json"} {
		if _, err := os.Stat(filepath.Join(rec.BundleDir, name)); err != nil {
			t.Errorf("source spool: %v", err)
		}
	}
	dst := f.dstRecord(t, rec.TxID)
	for _, name := range []string{"cpu.json", "disk.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dst.BundleDir, name)); err != nil {
			t.Errorf("destination documents: %v", err)
		}
	}
	if dst.State != dstRunning || dst.Disk == nil || dst.Disk.DataBytes != 7*diskBlockSize {
		t.Errorf("destination %s %+v", dst.State, dst.Disk)
	}
	state := mustInstance(t, f.dst, f.name)
	if state.Base == nil || state.Base.SHA256 != rec.BaseSHA256 || !isStreamRestore(state.CmdLine) {
		t.Errorf("destination instance %+v %v", state.Base, state.CmdLine)
	}
	if _, err := hviRestoreArgv(state.CmdLine, "/b", "/c", "/p"); err != nil {
		t.Errorf("the moved instance cannot be checkpointed and moved on: %v", err)
	}
}

// The destination's resume is timed on its own: the downtime runs to the
// record of running, written before the resume, plus the resume, so a
// resume timed from before the commit's synced records counted them
// twice.
func TestStreamedMoveTimesTheResumeAlone(t *testing.T) {
	f := newStreamFixture(t)
	var took time.Duration
	prev := destResume
	destResume = func(s *store.Store, name string) error {
		started := time.Now()
		err := prev(s, name)
		took = time.Since(started)
		return err
	}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	dst := f.dstRecord(t, rec.TxID)
	resume, commit := dst.Timings["resume"], dst.Timings["commit"]
	if resume > took.Milliseconds()+1 || commit < resume {
		t.Errorf("resume %d ms, commit %d ms; the resume itself took %s", resume, commit, took)
	}
	var running time.Time
	for _, h := range dst.History {
		if h.State == dstRunning {
			running = h.At
		}
	}
	if want := running.Sub(rec.Stream.PausedAt).Milliseconds() + resume; rec.Stream.DowntimeMs != want {
		t.Errorf("downtime %d ms, want %d", rec.Stream.DowntimeMs, want)
	}
}

// A disk round is bulk data, like a RAM round: relayed, spooled and handed
// to hvi as it comes, never held whole, so one larger than every document
// together may be does not fail the move.
func TestStreamedMoveRelaysADiskRoundLargerThanTheDocuments(t *testing.T) {
	f := newStreamFixture(t)
	prev := spooledStateMax
	spooledStateMax = 256 << 10
	t.Cleanup(func() { spooledStateMax = prev })
	f.hvi.bigDiskRound = 8 // 512 KiB more in disk round 0
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if r := rec.Stream.Rounds[1]; r.Name != "disk-round-0" || r.Blocks != 11 || r.Bytes != 11*diskBlockSize {
		t.Errorf("rounds %+v", rec.Stream.Rounds)
	}
	f.checkArrived(t, rec)
	if _, err := verifySpool(rec.BundleDir); err != nil {
		t.Errorf("the spool: %v", err)
	}
}

// A capture that fails before hvi seals its stream leaves the guest running
// here: the move stops in transferring, not fenced, and run again it
// streams from the start and commits.
func TestStreamedMoveCaptureFailsBeforeTheSeal(t *testing.T) {
	f := newStreamFixture(t)
	f.hvi.sealFail = true
	err := f.migrate(t, "")
	if err == nil || !strings.Contains(err.Error(), "capture_failed") || !strings.Contains(err.Error(), "still runs here") {
		t.Fatalf("err = %v", err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcTransferring || fencedFor(mustInstance(t, f.src, f.name), rec.TxID) {
		t.Fatalf("after the failed capture: %s", rec.State)
	}
	if !strings.Contains(rec.Error, "ended before its manifest, so the guest runs on here") {
		t.Errorf("the record does not say the guest runs on: %q", rec.Error)
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

// hull failing on one of hvi's frames, as a full disk under the spool
// would, stops reading hvi's stream. Whether the guest then runs on is
// hvi's to say, and hull says what hvi says: early in the stream hvi finds
// the connection closed and resumes the guest; at a late document the
// rest of the stream, its manifest included, is already in the socket's
// buffer, hvi has fenced the guest, and the move is left for a re-run,
// which has hvi stream it again.
func TestStreamedMoveHullFailsOnAFrame(t *testing.T) {
	for _, tc := range []struct {
		at     string
		fenced bool
	}{{"disk-round-0", false}, {"cpu.json", true}} {
		t.Run(tc.at, func(t *testing.T) {
			f := newStreamFixture(t)
			// A stream longer than any socket's buffer, so hvi is still
			// writing it when hull fails early.
			f.hvi.bigDiskRound = 8
			failed := false
			relayFrameTestHook = func(component string) error {
				if component != tc.at || failed {
					return nil
				}
				failed = true
				if tc.fenced {
					// The tail is small: hvi writes all of it, and
					// fences, while hull is still on this frame.
					select {
					case <-f.hvi.fencedNow:
					case <-time.After(10 * time.Second):
						t.Error("hvi never fenced the guest")
					}
				}
				return syscall.ENOSPC
			}
			t.Cleanup(func() { relayFrameTestHook = nil })
			err := f.migrate(t, "")
			if err == nil || !strings.Contains(err.Error(), syscall.ENOSPC.Error()) {
				t.Fatalf("err = %v", err)
			}
			rec := f.srcRecord(t)
			f.hvi.mu.Lock()
			state := f.hvi.state
			f.hvi.mu.Unlock()
			if rec.State != srcTransferring || (state == "fenced") != tc.fenced {
				t.Fatalf("the move is %s and hvi %s", rec.State, state)
			}
			saysRanOn := strings.Contains(err.Error(), "runs on here") ||
				strings.Contains(err.Error(), "still runs here")
			if tc.fenced {
				if saysRanOn || !strings.Contains(err.Error(), "The source stays fenced") ||
					!strings.Contains(rec.Error, "hvi fenced the guest") || strings.Contains(rec.Error, "runs on") {
					t.Fatalf("a fenced guest reported as %v; record %q", err, rec.Error)
				}
			} else if !saysRanOn || !strings.Contains(rec.Error, "runs on here") {
				t.Fatalf("a guest that runs on reported as %v; record %q", err, rec.Error)
			}
			relayFrameTestHook = nil
			if err := f.migrate(t, rec.TxID); err != nil {
				t.Fatal(err)
			}
			rec = f.srcRecord(t)
			want := "live"
			if tc.fenced {
				want = "replay"
			}
			if rec.State != srcCommitted || rec.Stream.Mode != want {
				t.Fatalf("the re-run: %s %s", rec.State, rec.Stream.Mode)
			}
			assertSameContent(t, f.disk, filepath.Join(f.dst.InstanceDir(f.name), "bundle", "rootfs.ext4"))
		})
	}
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
// its spooled copy: run again, the move replays the spool, disk rounds and
// all, onto a fresh copy of the base there, and commits.
func TestStreamedMoveRefusedInTransitThenReplayed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  bool
		want string
	}{{"corrupted", false, "corrupted"}, {"truncated", true, "ended"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t)
			// Into the RAM rounds: past the header and the first disk
			// round (three blocks), well before the end.
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
			// The guest stays paused until the replay's commit, and the
			// downtime counts from the first attempt's pause.
			time.Sleep(300 * time.Millisecond)
			if err := f.migrate(t, rec.TxID); err != nil {
				t.Fatal(err)
			}
			rec = f.srcRecord(t)
			if rec.State != srcCommitted || rec.Stream.Mode != "replay" {
				t.Fatalf("the replay: %s %+v", rec.State, rec.Stream)
			}
			if st := rec.Stream; !st.PausedAt.Equal(time.UnixMilli(f.hvi.pausedAtMs)) || st.DowntimeMs < 300 ||
				st.SealedMs != 7 {
				t.Errorf("the replay's pause %v (hvi's %d), sealed %d ms, downtime %d ms", st.PausedAt,
					f.hvi.pausedAtMs, st.SealedMs, st.DowntimeMs)
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
	f := newStreamFixture(t)
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
	f := newStreamFixture(t)
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
	all := transportFacts{SourceFormat: 3, HasBase: true, HviBase: true, DestStream: true, DestHviStream: true}
	if got, why := decideTransport(all); got != transportStream || why != "" {
		t.Fatalf("all there: %q %q", got, why)
	}
	for _, tc := range []struct {
		mut  func(*transportFacts)
		want string
	}{
		{func(f *transportFacts) { f.WholeBundle = true }, "--whole-bundle"},
		{func(f *transportFacts) { f.DestStream = false; f.DestHull = "v0.9" }, "hull there (v0.9) does not take a stream-v3"},
		{func(f *transportFacts) { f.DestHviStream = false }, "cannot restore a stream's disk rounds"},
		{func(f *transportFacts) { f.SourceFormat = 2 }, "checkpoint format 2"},
		{func(f *transportFacts) { f.SourceFormat = 0 }, "checkpoint format 0"},
		{func(f *transportFacts) { f.HasBase = false; f.BaseReason = "no base kept" }, "no base kept"},
		{func(f *transportFacts) { f.HviBase = false }, "started by an older hull"},
		{func(f *transportFacts) { f.HviBase = false; f.HviWhy = "/bin/hvi takes no --disk-written" },
			"not given its base, because /bin/hvi takes no --disk-written"},
		{func(f *transportFacts) { f.HviBase = false; f.HviDiskBase = strings.Repeat("e", 64) },
			"booted against base eeeeeeeeeeee"},
	} {
		f := all
		tc.mut(&f)
		got, why := decideTransport(f)
		if got != transportBundle || !strings.Contains(why, tc.want) {
			t.Errorf("%+v: %q %q, want the bundle because %q", f, got, why, tc.want)
		}
	}
}

// The facts come from the running hvi's profile and the probe: an hvi
// that writes format 2, or that was booted without the instance's base,
// and a destination whose probe lists only stream-v2 or whose hvi cannot
// take a base, each send the whole bundle.
func TestMoveTransportFacts(t *testing.T) {
	f := newStreamFixture(t)
	state := mustInstance(t, f.src, f.name)
	probe := destProbe(f.dst, f.name, f.moveFixture.hvi, "").Probe
	if got, why := decideTransport(moveTransportFacts(f.src, state, probe, false)); got != transportStream {
		t.Fatalf("the fixture: %q %q", got, why)
	}
	for _, tc := range []struct {
		name, profile string
		probe         func(*migrateProbe)
		want          string
	}{
		{"format 2", `{"checkpoint_format":2,"disk_base_sha256":"%s"}`, nil, "checkpoint format 2"},
		{"no base at boot", `{"checkpoint_format":3}`, nil, "started by an older hull"},
		{"another base", `{"checkpoint_format":3,"disk_base_sha256":"` + strings.Repeat("e", 64) + `"}`, nil,
			"booted against base eeeeeeeeeeee"},
		{"a stream-v2 hull", `{"checkpoint_format":3,"disk_base_sha256":"%s"}`,
			func(p *migrateProbe) { p.Capabilities = []string{"stream-v2"} }, "does not take a stream-v3"},
		{"an hvi without the disk rounds", `{"checkpoint_format":3,"disk_base_sha256":"%s"}`,
			func(p *migrateProbe) { p.HviStreamDisk = false }, "cannot restore a stream's disk rounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof := tc.profile
			if strings.Contains(prof, "%s") {
				prof = fmt.Sprintf(prof, state.Base.SHA256)
			}
			if err := os.WriteFile(state.ProfilePath, []byte(prof), 0o600); err != nil {
				t.Fatal(err)
			}
			p := *probe
			if tc.probe != nil {
				tc.probe(&p)
			}
			got, why := decideTransport(moveTransportFacts(f.src, state, &p, false))
			if got != transportBundle || !strings.Contains(why, tc.want) {
				t.Errorf("%q %q, want the bundle because %q", got, why, tc.want)
			}
		})
	}
	// hull run records why it could not boot hvi against the base, and
	// the move says that rather than blame an older hull.
	if err := os.WriteFile(state.ProfilePath, []byte(`{"checkpoint_format":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state.Base.HviWhy = "/opt/hvi takes no --disk-written"
	if got, why := decideTransport(moveTransportFacts(f.src, state, probe, false)); got != transportBundle ||
		!strings.Contains(why, "because /opt/hvi takes no --disk-written") {
		t.Errorf("%q %q", got, why)
	}
}

// The dry run of a streamed move says how it would go; one to a hull from
// before streamed moves says it would send the whole bundle, and why.
func TestStreamedMoveDryRun(t *testing.T) {
	f := newStreamFixture(t)
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
	if !strings.Contains(out.String(), "does not take a stream-v3 streamed move") {
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
	f := newStreamFixture(t)
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

// A guest that writes to its disk while hvi streams it: each disk round
// carries what was written since the one before, the one hvi takes paused
// holds the last writes, and the destination's copy of the base ends as
// the source's disk. hull drives no disk round of its own.
func TestStreamedMoveDiskRoundsFollowTheGuestsWrites(t *testing.T) {
	f := newStreamFixture(t)
	for b := int64(100); b < 140; b++ {
		f.hvi.writesAt["ram-round-0"] = append(f.hvi.writesAt["ram-round-0"], b)
	}
	f.hvi.writesAt["ram-round-1"] = []int64{200, 201, 202, 100}
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
	if fmt.Sprint(blocks) != "[3 40 4]" {
		t.Errorf("disk rounds of %v blocks", blocks)
	}
	if st.FinalDiskBytes != 4*diskBlockSize {
		t.Errorf("final disk round %d bytes", st.FinalDiskBytes)
	}
	if slices.ContainsFunc(f.hvi.ops, func(op string) bool { return strings.HasPrefix(op, "disk-") }) {
		t.Errorf("hull drove hvi's disk ops: %v", f.hvi.ops)
	}
	f.checkArrived(t, rec)
}

// A destination that refuses the stream's header (here: it lost the base
// after the source checked) refuses it while the guest still runs: hvi's
// migrate never starts, the source sends the base, and the move goes on
// live, by itself.
func TestStreamedMoveSendsTheBaseOnNoBase(t *testing.T) {
	f := newStreamFixture(t)
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
	f := newStreamFixture(t)
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

// A streamed move's restore on the destination names the base its disk is
// a copy of; a restore from the spool on the source, or of a directory,
// names none, and the base and the written blocks of the boot never carry
// into a restore.
func TestMoveRestoreBase(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	st := &store.InstanceState{Base: &store.BaseState{SHA256: sum}}
	argv := []string{"hvi", "restore", "--from-stream", "-", "--disk", "/d"}
	if got := strings.Join(moveRestoreBase(argv, st, true), " "); got != strings.Join(argv, " ")+" --disk-base-sha256 "+sum {
		t.Errorf("incoming: %s", got)
	}
	if got := moveRestoreBase(argv, st, false); len(got) != len(argv) {
		t.Errorf("from the spool: %v", got)
	}
	if got := moveRestoreBase(argv, &store.InstanceState{}, true); len(got) != len(argv) {
		t.Errorf("no base: %v", got)
	}
	boot := []string{"hvi", "boot", "--disk", "/d", hviDiskBaseFlag, sum, hviDiskWrittenFlag, "/w"}
	out, err := hviRestoreArgv(boot, "/b", "/c", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(out, hviDiskBaseFlag) || slices.Contains(out, hviDiskWrittenFlag) {
		t.Errorf("the boot's base carried into a restore: %v", out)
	}
}
