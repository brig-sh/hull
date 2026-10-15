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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// A session cut after the destination said verified, with the commit sent
// into it, leaves the source no answer: it sends the commit again as its
// own verb, which the destination answers from its record.
func TestStreamedMoveCommitsAfterTheSessionIsCut(t *testing.T) {
	f := newStreamFixture(t)
	f.peer.cutSession = true
	opt := f.options(false)
	var out bytes.Buffer
	opt.out = &out
	if err := sourceMigrate(context.Background(), f.src, f.name, opt, f.peer); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted {
		t.Fatalf("source %s", rec.State)
	}
	if f.peer.called("migrate-commit") != 1 || !strings.Contains(out.String(), "the commit had no answer in the move's session") {
		t.Errorf("calls %v\n%s", f.peer.calls, out.String())
	}
	if dst := f.dstRecord(t, rec.TxID); dst.State != dstRunning {
		t.Errorf("destination %s", dst.State)
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
