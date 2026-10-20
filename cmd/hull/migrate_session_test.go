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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A streamed move to a destination that holds a session takes one session
// from the stream's header to the resume: no status polled for the
// header, no migrate-commit, and the commit in the stream after its
// trailer.
func TestStreamedMoveInOneSession(t *testing.T) {
	f := newStreamFixture(t)
	f.hvi.writesAt["ram-round-0"] = []int64{1, 2}
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || !rec.Session {
		t.Fatalf("source %s, session %v", rec.State, rec.Session)
	}
	f.peer.mu.Lock()
	calls := slices.Clone(f.peer.calls)
	f.peer.mu.Unlock()
	at := slices.Index(calls, "migrate-receive --session")
	if at < 0 {
		t.Fatalf("no session: %v", calls)
	}
	if after := calls[at+1:]; len(after) != 0 {
		t.Errorf("calls after the session began: %v", after)
	}
	dst := f.dstRecord(t, rec.TxID)
	if dst.State != dstRunning || dst.Epoch != rec.Epoch {
		t.Errorf("destination %s epoch %d", dst.State, dst.Epoch)
	}
	if st := mustInstance(t, f.dst, f.name); st.OwnerEpoch != rec.Epoch {
		t.Errorf("destination owner epoch %d, want %d", st.OwnerEpoch, rec.Epoch)
	}
	f.checkArrived(t, rec)
}

// sessionlessPeer is a destination from before sessions: its probe lists
// no session, and it holds none.
type sessionlessPeer struct{ *localPeer }

func (p sessionlessPeer) call(ctx context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error) {
	reply, err := p.localPeer.call(ctx, verb, args, stdin)
	if reply != nil && reply.Probe != nil {
		reply.Probe.Capabilities = slices.DeleteFunc(slices.Clone(reply.Probe.Capabilities),
			func(c string) bool { return c == moveCapSession })
	}
	return reply, err
}

// A destination without sessions gets the move as before: the header is
// polled for, and the commit is its own verb.
func TestStreamedMoveWithoutASession(t *testing.T) {
	f := newStreamFixture(t)
	opt := f.options(false)
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, sessionlessPeer{f.peer}); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || rec.Session {
		t.Fatalf("source %s, session %v", rec.State, rec.Session)
	}
	if f.peer.called("migrate-commit") != 1 || f.peer.called("migrate-receive --session") != 0 {
		t.Errorf("calls %v", f.peer.calls)
	}
	f.checkArrived(t, rec)
}

// watchPeer notes the destination's record of a move as each
// migrate-commit reaches it, before it runs.
type watchPeer struct {
	*localPeer
	f      *streamFixture
	before []string
}

func (p *watchPeer) call(ctx context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error) {
	if verb == "migrate-commit" {
		rec, _, err := findMigrationByTxID(p.f.dst, args[1])
		state := "none"
		if err == nil {
			state = rec.State
		}
		p.before = append(p.before, state)
	}
	return p.localPeer.call(ctx, verb, args, stdin)
}

// A session that carries the commit to the destination and loses its
// answer leaves the source no answer: it sends the commit again as its own
// verb, which the destination, already running the guest, answers from
// its record.
func TestStreamedMoveCommitsAfterTheSessionIsCut(t *testing.T) {
	f := newStreamFixture(t)
	f.peer.cutSession = true
	peer := &watchPeer{localPeer: f.peer, f: f}
	opt := f.options(false)
	var out bytes.Buffer
	opt.out = &out
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted {
		t.Fatalf("source %s", rec.State)
	}
	if f.peer.called("migrate-commit") != 1 || !strings.Contains(out.String(), "the commit had no answer in the move's session") {
		t.Errorf("calls %v\n%s", f.peer.calls, out.String())
	}
	if len(peer.before) != 1 || peer.before[0] != dstRunning {
		t.Errorf("the destination before migrate-commit: %v, want it running from the session's commit", peer.before)
	}
	if dst := f.dstRecord(t, rec.TxID); dst.State != dstRunning {
		t.Errorf("destination %s", dst.State)
	}
}

// garbleCommitPeer damages the first byte the destination reads after it
// said verified: the commit's.
type garbleCommitPeer struct {
	*localPeer
	armed atomic.Bool
}

func (p *garbleCommitPeer) session(ctx context.Context, verb string, args []string, stdin io.Reader,
	event func(*migrateReply)) (*migrateReply, error) {
	return p.localPeer.session(ctx, verb, args, &garbleAfter{r: stdin, armed: &p.armed}, func(r *migrateReply) {
		if r.Event == eventVerified {
			p.armed.Store(true)
		}
		event(r)
	})
}

type garbleAfter struct {
	r     io.Reader
	armed *atomic.Bool
	done  bool
}

func (g *garbleAfter) Read(b []byte) (int, error) {
	n, err := g.r.Read(b)
	if n > 0 && !g.done && g.armed.Load() {
		b[0] ^= 0xff
		g.done = true
	}
	return n, err
}

// A commit the destination refuses in the session as damaged (code
// failed) is sent again as migrate-commit, which commits the guest the
// destination still holds restored paused.
func TestStreamedMoveSendsAGarbledCommitAgain(t *testing.T) {
	f := newStreamFixture(t)
	peer := &garbleCommitPeer{localPeer: f.peer}
	opt := f.options(false)
	var out bytes.Buffer
	opt.out = &out
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, peer); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if rec := f.srcRecord(t); rec.State != srcCommitted {
		t.Fatalf("source %s", rec.State)
	}
	if f.peer.called("migrate-commit") != 1 || !strings.Contains(out.String(), "after the trailer") {
		t.Errorf("calls %v\n%s", f.peer.calls, out.String())
	}
	if dst := f.dstRecord(t, f.srcRecord(t).TxID); dst.State != dstRunning {
		t.Errorf("destination %s", dst.State)
	}
}

// A source that fails a check after the destination said verified, before
// its commit point, ends the session with no commit: the destination keeps
// the guest restored paused, for the re-run or the abort.
func TestStreamedMoveEndsTheSessionWithoutACommit(t *testing.T) {
	f := newStreamFixture(t)
	prev := streamTestHook
	streamTestHook = func(stage string) {
		if stage == "answered" {
			// Something wrote the disk after the fence.
			at := time.Now().Add(time.Hour)
			if err := os.Chtimes(f.disk, at, at); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { streamTestHook = prev })
	err := f.migrate(t, "")
	if err == nil || !strings.Contains(err.Error(), "changed after the fence") {
		t.Fatalf("the move: %v", err)
	}
	rec := f.srcRecord(t)
	if migrateStateOrder(roleSource, rec.State) >= migrateStateOrder(roleSource, srcCommitting) {
		t.Errorf("source %s: it passed its commit point", rec.State)
	}
	if f.peer.called("migrate-commit") != 0 {
		t.Errorf("calls %v", f.peer.calls)
	}
	dst := f.dstRecord(t, rec.TxID)
	if dst.State != dstRestoredPaused {
		t.Errorf("destination %s, want %s", dst.State, dstRestoredPaused)
	}
	if dst.Stream != nil {
		for _, s := range dst.Stream.Steps {
			if s.Step == "commit received" {
				t.Errorf("the destination received a commit: %v", dst.Stream.Steps)
			}
		}
	}
}

// The destination's wait for the commit in a session ends before a
// migrate-commit or an abort that waits for its lock gives up.
func TestSessionCommitWaitIsShorterThanTheLockWait(t *testing.T) {
	if sessionCommitWait >= moveLockWait {
		t.Errorf("sessionCommitWait %s, moveLockWait %s", sessionCommitWait, moveLockWait)
	}
}

// What follows a session's trailer: nothing, which leaves the move
// verified, or the commit; anything else is refused.
func TestReadMoveCommit(t *testing.T) {
	var buf bytes.Buffer
	sw := newMoveStreamWriter(&buf)
	if err := sw.commit(moveStreamCommit{TxID: "t1", Epoch: 4}); err != nil {
		t.Fatal(err)
	}
	c, err := readMoveCommit(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil || c == nil || c.TxID != "t1" || c.Epoch != 4 {
		t.Errorf("commit %+v, %v", c, err)
	}
	if c, err := readMoveCommit(bufio.NewReader(strings.NewReader(""))); c != nil || err != nil {
		t.Errorf("the end of the stream: %+v, %v", c, err)
	}
	if _, err := readMoveCommit(bufio.NewReader(bytes.NewReader([]byte("T\x00\x00\x00\x02{}")))); err == nil {
		t.Error("a trailer after the trailer was taken")
	}
}

// The destination waits for the commit only so long, and refuses one with
// another epoch, as migrate-commit does: either way the move stays
// verified there.
func TestDestSessionCommit(t *testing.T) {
	rec := &migrationRecord{TxID: "t1", Role: roleDestination, Epoch: 2, State: dstRestoredPaused}
	verified := &migrateReply{OK: true, Record: rec, VM: "restored-paused"}
	prev := sessionCommitWait
	sessionCommitWait = 50 * time.Millisecond
	t.Cleanup(func() { sessionCommitWait = prev })
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	if got := destSessionCommit(nil, rec, "", bufio.NewReader(pr), verified); got != verified {
		t.Errorf("no commit within the wait: %+v", got)
	}
	var buf bytes.Buffer
	_ = newMoveStreamWriter(&buf).commit(moveStreamCommit{TxID: "t1", Epoch: 3})
	got := destSessionCommit(nil, rec, "", bufio.NewReader(&buf), verified)
	if got.OK || got.Code != codeWrongEpoch {
		t.Errorf("another epoch: %+v", got)
	}
	buf.Reset()
	_ = newMoveStreamWriter(&buf).commit(moveStreamCommit{TxID: "t2", Epoch: 2})
	if got := destSessionCommit(nil, rec, "", bufio.NewReader(&buf), verified); got.OK {
		t.Errorf("another move's commit: %+v", got)
	}
	if got := destSessionCommit(nil, rec, "", bufio.NewReader(strings.NewReader("")), verified); got != verified {
		t.Errorf("the end of the stream: %+v", got)
	}
}

// readSession hands each event on as it comes and returns the last line,
// and takes nothing after it or output that is not a reply.
func TestReadSession(t *testing.T) {
	var events []string
	reply, err := readSession(strings.NewReader(`{"protocol":1,"ok":true,"event":"accepted","detail":"a1"}`+"\n"+
		`{"protocol":1,"ok":true,"event":"acked","bytes":42}`+"\n"+`{"protocol":1,"ok":true,"vm":"running"}`+"\n"),
		func(r *migrateReply) { events = append(events, r.Event) })
	if err != nil || reply.VM != "running" || strings.Join(events, ",") != "accepted,acked" {
		t.Errorf("reply %+v, %v, events %v", reply, err, events)
	}
	for _, out := range []string{
		`{"protocol":1,"ok":true,"event":"acked","bytes":1}` + "\n",
		`{"protocol":1,"ok":true}` + "\n" + `{"protocol":1,"ok":true}` + "\n",
		"ssh: banner\n",
	} {
		if _, err := readSession(strings.NewReader(out), func(*migrateReply) {}); err == nil {
			t.Errorf("%q was taken", out)
		}
	}
}

// fakeClock is a clock a test moves.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// The window holds a write back until the destination has read enough,
// lets everything through once closed, and grows to twice the rate times
// the round trip.
func TestAckWindow(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	w := newAckWindow()
	w.now = clock.now
	if w.size() != windowMin {
		t.Fatalf("size %d before any ack", w.size())
	}
	w.wait(windowMin)
	waited := make(chan struct{})
	go func() {
		w.wait(1)
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("a write went past a full window")
	case <-time.After(50 * time.Millisecond):
	}
	clock.t = clock.t.Add(100 * time.Millisecond)
	w.ack(windowMin)
	<-waited
	if w.minRTT != 100*time.Millisecond {
		t.Errorf("round trip %s", w.minRTT)
	}
	// 1 MiB read in each of two spans of 100 ms: 10 MiB/s, so a window of
	// twice that over the 100 ms round trip. A write larger than the
	// window goes once nothing is on its way.
	clock.t = clock.t.Add(100 * time.Millisecond)
	w.ack(windowMin + 1)
	w.wait(1 << 20)
	clock.t = clock.t.Add(100 * time.Millisecond)
	w.ack(windowMin + 1 + 1<<20)
	w.wait(1 << 20)
	clock.t = clock.t.Add(100 * time.Millisecond)
	w.ack(windowMin + 1 + 2<<20)
	if got, want := w.size(), int64(2*(1<<20)*10*0.1); got < want*9/10 || got > want*11/10 {
		t.Errorf("size %d, want about %d", got, want)
	}
	// On a LAN the shortest round trip is a fraction of a millisecond:
	// the window allows windowSlack at the rate.
	lan := newAckWindow()
	lan.now = clock.now
	lan.wait(64 << 10)
	clock.t = clock.t.Add(200 * time.Microsecond)
	lan.ack(64 << 10)
	for i := 0; i < 3; i++ {
		lan.wait(1 << 20)
		clock.t = clock.t.Add(20 * time.Millisecond)
		lan.ack(64<<10 + int64(i+1)<<20)
	}
	if got, want := lan.size(), int64(float64(1<<20)/0.02*windowSlack.Seconds()); got < want*9/10 || got > want*11/10 {
		t.Errorf("LAN size %d, want about %d", got, want)
	}
	w.close()
	done := make(chan struct{})
	go func() {
		w.wait(windowMax * 2)
		w.wait(windowMax * 2)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a closed window held a write")
	}
}

// windowLink is a link a test drives a window over with a fake clock: the
// destination reads what was handed on at rate bytes per second, in order,
// so what the window lets on beyond the link's round trip queues, and
// says so rtt later. The writer always has the next 64 KiB to hand on.
type windowLink struct {
	w        *ackWindow
	clock    *fakeClock
	rate     float64
	rtt      time.Duration
	queue    []int64 // ends of the writes not yet wholly read
	read     int64
	acks     []windowAck
	credit   float64
	maxQueue int64 // the most on its way at once
}

type windowAck struct {
	n  int64
	at time.Time
}

func newWindowLink(rate float64, rtt time.Duration) *windowLink {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	w := newAckWindow()
	w.now = clock.now
	return &windowLink{w: w, clock: clock, rate: rate, rtt: rtt}
}

// run drives the link for d in steps of 100 µs and returns the bytes read
// in that time.
func (l *windowLink) run(d time.Duration) int64 {
	const step = 100 * time.Microsecond
	from := l.read
	for end := l.clock.t.Add(d); l.clock.t.Before(end); l.clock.t = l.clock.t.Add(step) {
		for !l.w.full(64 << 10) {
			l.w.take(64 << 10)
			l.queue = append(l.queue, l.w.sent)
		}
		l.maxQueue = max(l.maxQueue, l.w.sent-l.w.acked)
		l.credit += l.rate * step.Seconds()
		for len(l.queue) > 0 && l.credit >= 1 {
			n := min(int64(l.credit), l.queue[0]-l.read)
			l.read += n
			l.credit -= float64(n)
			if l.read == l.queue[0] {
				l.queue = l.queue[1:]
				at := l.clock.t.Add(l.rtt)
				if k := len(l.acks); k > 0 && at.Before(l.acks[k-1].at) {
					at = l.acks[k-1].at
				}
				l.acks = append(l.acks, windowAck{n: l.read, at: at})
			}
		}
		if len(l.queue) == 0 {
			l.credit = 0
		}
		for len(l.acks) > 0 && !l.acks[0].at.After(l.clock.t) {
			l.w.ack(l.acks[0].n)
			l.acks = l.acks[1:]
		}
	}
	return l.read - from
}

// The window keeps a link busy and its queue short whatever the link's
// round trip. When the round trip grows during the stream the window grows
// with it within a second and a half, rather than holding the rate to the
// window over the new round trip, which shrank it with each sample down to
// windowMin; and when the destination reads more slowly it shrinks.
func TestAckWindowOverALink(t *testing.T) {
	for _, c := range []struct {
		name     string
		rate     float64
		rtt      time.Duration
		then     time.Duration
		thenRate float64
	}{
		{name: "LAN", rate: 60e6, rtt: 500 * time.Microsecond},
		{name: "WAN", rate: 1.25e6, rtt: 100 * time.Millisecond},
		{name: "40 ms", rate: 60e6, rtt: 40 * time.Millisecond},
		{name: "LAN, then 40 ms", rate: 60e6, rtt: 500 * time.Microsecond, then: 40 * time.Millisecond},
		{name: "LAN, then 200 ms", rate: 60e6, rtt: 500 * time.Microsecond, then: 200 * time.Millisecond},
		{name: "40 ms, then slower", rate: 60e6, rtt: 40 * time.Millisecond, thenRate: 5e6},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newWindowLink(c.rate, c.rtt)
			l.run(2 * time.Second)
			if c.then != 0 || c.thenRate != 0 {
				if c.then != 0 {
					l.rtt = c.then
				}
				if c.thenRate != 0 {
					l.rate = c.thenRate
				}
				l.run(1500 * time.Millisecond)
			}
			l.maxQueue = 0
			got := float64(l.run(10*time.Second)) / 10
			t.Logf("read %.2f MB/s, most on its way %d, window %d, round trip %s", got/1e6, l.maxQueue,
				l.w.size(), l.w.minRTT)
			if got < 0.85*l.rate {
				t.Errorf("read %.2f MB/s of a %.2f MB/s link", got/1e6, l.rate/1e6)
			}
			// What is on its way stays within twice what the window
			// allows for: twice the link's round trip, or windowSlack,
			// at the link's rate.
			allow := int64(l.rate * max(2*l.rtt.Seconds(), windowSlack.Seconds()))
			if limit := 2*max(allow, windowMin) + 64<<10; l.maxQueue > limit {
				t.Errorf("%d bytes on their way, over %d", l.maxQueue, limit)
			}
		})
	}
}

// Over a real ssh stand-in, a session's lines reach the source as the
// destination writes them, before its stdin ends: the source answers the
// first line, and only then does the destination print the last.
func TestSSHSessionReadsLinesAsTheyCome(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\n" +
		`echo '{"protocol":1,"ok":true,"event":"accepted","detail":"a1"}'` + "\n" +
		"read line\n" +
		`echo '{"protocol":1,"ok":true,"event":"verified","detail":"'$line'"}'` + "\n" +
		`echo '{"protocol":1,"ok":true,"vm":"running"}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(migrateSSHEnv, script)
	t.Setenv(migrateSSHMuxEnv, "off")
	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []string
	p := &sshPeer{target: "pm@dest"}
	reply, err := p.session(ctx, "migrate-receive", nil, pr, func(r *migrateReply) {
		mu.Lock()
		got = append(got, r.Event+":"+r.Detail)
		mu.Unlock()
		if r.Event == eventAccepted {
			_, _ = io.WriteString(pw, "answered\n")
			_ = pw.Close()
		}
	})
	if err != nil || reply.VM != "running" {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	if strings.Join(got, ",") != "accepted:a1,verified:answered" {
		t.Errorf("events %v", got)
	}
}

// The destination's verb, in a process of its own, writes a session's
// lines on the stdout it was given, not on the stderr its verbs print to.
func TestMigrateReceiveSessionWritesItsReplyOnStdout(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := shortStore(t)
	// Used as it is: on macOS hull would otherwise mount a case-sensitive
	// image over it.
	if err := os.WriteFile(filepath.Join(s.RootDir(), ".case-sensitive-apfs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(self, "--store-dir", s.RootDir(), "migrate-receive", "--stream", "--session",
		"--txid", "0123456789abcdef0123456789abcdef", "--name", "vm1")
	c.Env = append(os.Environ(), runHullEnv+"=1", "DO_NOT_TRACK=1")
	c.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	_ = c.Run()
	var reply migrateReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("stdout %q (%v), stderr %q", stdout.String(), err, stderr.String())
	}
	if reply.OK || reply.Code != codeUnknownTxID || reply.Protocol != migrateProtocol {
		t.Errorf("reply %+v", reply)
	}
}

// stoppedPeer is a destination whose hull stops, as SIGSTOP stops it, once
// it has read after bytes of the stream: it reads and acks nothing more,
// and its session stays open until the source ends the call, as ssh's
// does. buffered is what the link takes on its behalf meanwhile, as ssh's
// window and the sockets would: 0 holds the source's write in the pipe,
// more lets it fill its window and wait there.
type stoppedPeer struct {
	*localPeer
	after, buffered int64
}

func (p *stoppedPeer) session(ctx context.Context, verb string, args []string, stdin io.Reader,
	event func(*migrateReply)) (*migrateReply, error) {
	if verb != "migrate-receive" {
		return p.localPeer.session(ctx, verb, args, stdin, event)
	}
	stopped := &stopReader{r: stdin, left: p.after, buffered: p.buffered, ctx: ctx}
	reply, err := p.localPeer.session(ctx, verb, args, stopped, event)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w to %s: signal: killed", errMoveNoAnswer, verb)
	}
	return reply, err
}

// stopReader reads left bytes of r, and then nothing until ctx ends.
type stopReader struct {
	r              io.Reader
	left, buffered int64
	ctx            context.Context
	stopped        bool
}

func (s *stopReader) Read(b []byte) (int, error) {
	if s.left <= 0 {
		if !s.stopped && s.buffered > 0 {
			go func() { _, _ = io.CopyN(io.Discard, s.r, s.buffered) }()
		}
		s.stopped = true
		<-s.ctx.Done()
		return 0, errors.New("the destination's hull was killed")
	}
	n, err := s.r.Read(b[:min(int64(len(b)), s.left)])
	s.left -= int64(n)
	return n, err
}

// A destination whose hull stops, its ssh session open, takes none of the
// stream: the window or the pipe holds the source's relay, and with it
// its reads of hvi's socket. The source cuts it off once hvi's own bound
// for a reader that takes nothing has passed, and the margin, and reads
// the rest of hvi's stream into its spool. hvi, which
// gave up on the stalled reader and closed the stream, has the guest
// running on, as its status says; an hvi still writing seals the stream,
// and the source holds it whole, fenced, for a re-run. Either way the
// move ends without the destination.
func TestStreamedMoveDestinationStopsReading(t *testing.T) {
	stall, margin := hviStreamStall, hviPausedMargin
	hviStreamStall, hviPausedMargin = 500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { hviStreamStall, hviPausedMargin = stall, margin })
	for _, tc := range []struct {
		name     string
		buffered int64
		hviGives bool
	}{
		{"in the window, hvi gives up", 8 << 20, true},
		{"in the pipe, hvi gives up", 0, true},
		{"in the window, hvi seals", 8 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t)
			// 1.5 MiB more in disk round 0, more than the socket and
			// hull's reader hold, so hvi waits on the stopped reader.
			f.hvi.bigDiskRound = 24
			if tc.hviGives {
				f.hvi.readerIdle = 200 * time.Millisecond
			}
			peer := &stoppedPeer{localPeer: f.peer, after: 64 << 10, buffered: tc.buffered}
			opt := f.options(false)
			var out bytes.Buffer
			opt.out = &out
			started := time.Now()
			err := sourceMigrate(context.Background(), f.src, f.name, opt, peer)
			took := time.Since(started)
			if err == nil || !strings.Contains(err.Error(), "the destination took none of the stream for 1.0 s") {
				t.Fatalf("err = %v\n%s", err, out.String())
			}
			if took > 10*time.Second {
				t.Errorf("the stopped destination held the move for %s", took)
			}
			rec := f.srcRecord(t)
			if rec.State != srcTransferring {
				t.Fatalf("the move is %s", rec.State)
			}
			f.hvi.mu.Lock()
			ops := slices.Clone(f.hvi.ops)
			f.hvi.mu.Unlock()
			at := slices.Index(ops, "migrate")
			if tc.hviGives {
				if !strings.Contains(err.Error(), "runs on here") || !strings.Contains(rec.Error, "runs on here") {
					t.Fatalf("err %v; record %q", err, rec.Error)
				}
				if at < 0 || slices.Index(ops[at:], "status") != 1 {
					t.Errorf("hull did not ask hvi's status once the stream ended: %v", ops)
				}
			} else if !strings.Contains(out.String(), "hvi sealed the stream and fenced") {
				t.Fatalf("hvi did not seal:\n%s", out.String())
			}
			if err := f.migrate(t, rec.TxID); err != nil {
				t.Fatal(err)
			}
			rec = f.srcRecord(t)
			want := "live"
			if !tc.hviGives {
				want = "replay"
			}
			if rec.State != srcCommitted || rec.Stream.Mode != want {
				t.Fatalf("the re-run: %s %s, want %s", rec.State, rec.Stream.Mode, want)
			}
			if tc.hviGives {
				f.checkArrived(t, rec)
			}
		})
	}
}
