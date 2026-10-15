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
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// A streamed move's session (docs/migration.md, "One session"): the
// source's `migrate-receive --stream --session` is the only ssh session
// from the stream's header to the destination's resume. The destination
// writes lines on stdout as the move goes, each a migrateReply with an
// event:
//
//	accepted  it took the header and copied the base (detail: the attempt)
//	acked     it has read this many bytes of the stream (bytes)
//	verified  hvi restored the guest paused from a stream checked whole
//	          (record, vm), as the reply to a receive outside a session
//
// and, last, a line with no event: the reply to the commit the source
// sends after the trailer ('K', migrate_stream2.go), or, when the source
// ends the stream instead, the verified reply again. Outside a session
// the source polled migrate-status for the header, waited for the reply,
// and sent the commit in a new ssh session, each a round trip and a new
// session over the link while the guest was stopped; and it wrote into
// ssh as fast as ssh took it, so on a slow link the megabytes in ssh's
// window and the kernel's buffers were still on their way when hvi sealed
// the stream, and the guest waited for them. The acks bound that.

// moveCapSession is the capability a destination's probe lists when its
// streamed receive takes --session.
const moveCapSession = "stream-session"

// Session events.
const (
	eventAccepted = "accepted"
	eventAcked    = "acked"
	eventVerified = "verified"
)

// sessionCommitWait bounds how long a destination waits after its verified
// line for the source's commit, holding the move's lock. A source that
// never sends it falls back to migrate-commit, which waits for that lock.
var sessionCommitWait = time.Minute

// sessionOut writes a session's lines to the destination's stdout, one
// whole line at a time. A write that fails (the source is gone and ssh
// with it) ends the lines; the receive goes on to its own end.
type sessionOut struct {
	mu     sync.Mutex
	w      io.Writer
	failed bool
}

func (o *sessionOut) send(r *migrateReply) {
	r.Protocol = migrateProtocol
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed {
		return
	}
	if _, err := o.w.Write(append(line, '\n')); err != nil {
		o.failed = true
	}
}

// event sends one event line.
func (o *sessionOut) event(r *migrateReply) {
	if o != nil {
		o.send(r)
	}
}

// ackEvery is how many bytes the destination reads before it says so at
// once; ackTick how often it says what it read since, if anything.
const (
	ackEvery = 64 << 10
	ackTick  = 5 * time.Millisecond
)

// ackReader counts what the destination reads of the stream and sends an
// acked line, every ackEvery bytes or ackTick, from a goroutine of its own.
type ackReader struct {
	r    io.Reader
	out  *sessionOut
	n    atomic.Int64
	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newAckReader(r io.Reader, out *sessionOut) *ackReader {
	a := &ackReader{r: r, out: out, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		done: make(chan struct{})}
	go a.run()
	return a
}

func (a *ackReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		before := a.n.Add(int64(n)) - int64(n)
		if before/ackEvery != (before+int64(n))/ackEvery {
			select {
			case a.kick <- struct{}{}:
			default:
			}
		}
	}
	return n, err
}

func (a *ackReader) run() {
	defer close(a.done)
	tick := time.NewTicker(ackTick)
	defer tick.Stop()
	var sent int64
	for {
		select {
		case <-a.stop:
			return
		case <-a.kick:
		case <-tick.C:
		}
		if n := a.n.Load(); n > sent {
			sent = n
			a.out.event(&migrateReply{OK: true, Event: eventAcked, Bytes: n})
		}
	}
}

// close stops the acks; no line follows from it once it returns.
func (a *ackReader) close() {
	close(a.stop)
	<-a.done
}

// sessionPeer is a movePeer that can hold a session: stdin goes to verb as
// in call, and each event line is handed to event as it arrives, from
// another goroutine. It returns the last line, the one with no event.
type sessionPeer interface {
	session(ctx context.Context, verb string, args []string, stdin io.Reader,
		event func(*migrateReply)) (*migrateReply, error)
}

// readSession reads a session's lines from r: events go to event, and the
// last line, which has none, is returned. Lines that are not replies, or a
// stream with no last line, are no answer.
func readSession(r io.Reader, event func(*migrateReply)) (*migrateReply, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var last *migrateReply
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			reply, perr := parseMigrateReply(line)
			if perr != nil {
				_, _ = io.Copy(io.Discard, br)
				return nil, perr
			}
			if last != nil {
				// A line after the last: the last was not the last.
				return nil, errors.New("its output goes on after its reply")
			}
			if reply.Event != "" {
				event(reply)
			} else {
				last = reply
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if last == nil {
		return nil, errors.New("it printed no reply")
	}
	return last, nil
}

// session runs verb on the destination over ssh as call does, reading its
// lines as they come.
func (p *sshPeer) session(ctx context.Context, verb string, args []string, stdin io.Reader,
	event func(*migrateReply)) (*migrateReply, error) {
	argv := p.sshArgv(verb, args)
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	c.Stdin = stdin
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.WaitDelay = 5 * time.Second
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("%w to %s on %s: %v", errMoveNoAnswer, verb, p.target, err)
	}
	reply, perr := readSession(out, event)
	runErr := c.Wait()
	if perr != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2000 {
			detail = detail[len(detail)-2000:]
		}
		if runErr == nil {
			runErr = perr
		}
		if detail != "" {
			detail = "; it said: " + detail
		}
		return nil, fmt.Errorf("%w to %s on %s: %v%s", errMoveNoAnswer, verb, p.target, runErr, detail)
	}
	if !reply.OK {
		return reply, &migrateRefusal{Verb: verb, Reply: reply}
	}
	return reply, nil
}

// The source's window: what it has handed to the link that the
// destination has not yet said it read. A write waits while that would go
// over the window, so the bytes on their way, in ssh, the kernels and the
// link, stay about two of the link's round trips' worth at the rate the
// destination reads them, rather than all that ssh's window and the
// socket buffers hold: on a slow link those take seconds to drain, and at
// hvi's seal they are what the stopped guest waits for.
const (
	windowMin = 128 << 10
	windowMax = 64 << 20
	// windowRateSpan is how many rate samples the window keeps the
	// fastest of; a sample spans at least the shortest round trip seen.
	windowRateSpan = 8
)

// ackWindow is the source's window over a session's acks.
type ackWindow struct {
	mu   sync.Mutex
	cond *sync.Cond
	// sent is what was handed to the link, acked what the destination
	// read; closed lets every write through.
	sent, acked int64
	closed      bool
	// writes are when each write still unacked was handed on, by the
	// offset it ended at, for the round trip; minRTT is the shortest.
	writes []windowWrite
	minRTT time.Duration
	// rates are the latest delivery rates in bytes per second, and
	// sampleAt and sampleAcked where the sample in progress began.
	rates       []float64
	sampleAt    time.Time
	sampleAcked int64
	// now is the clock, a variable for the tests.
	now func() time.Time
}

type windowWrite struct {
	end int64
	at  time.Time
}

func newAckWindow() *ackWindow {
	w := &ackWindow{now: time.Now}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// size is the window now: twice the fastest recent delivery rate times
// the shortest round trip, held between windowMin and windowMax. Until
// both are measured it is windowMin. A window that holds the rate back
// grows with each sample, since the rate it lets through is about the
// window over the round trip; once the link holds it back the rate stops
// rising and the window settles at twice what the link holds in flight.
func (w *ackWindow) size() int64 {
	var rate float64
	for _, r := range w.rates {
		rate = max(rate, r)
	}
	size := int64(2 * rate * w.minRTT.Seconds())
	return min(max(size, windowMin), windowMax)
}

// wait blocks until n more bytes fit in the window, or it is closed, and
// counts them as handed on.
func (w *ackWindow) wait(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A write larger than the window goes once nothing is on its way.
	for !w.closed && w.sent-w.acked > 0 && w.sent-w.acked+n > w.size() {
		w.cond.Wait()
	}
	w.sent += n
	w.writes = append(w.writes, windowWrite{end: w.sent, at: w.now()})
}

// ack takes an acked line: the destination has read n bytes.
func (w *ackWindow) ack(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n <= w.acked {
		return
	}
	now := w.now()
	// The round trip of the last write wholly read.
	i := 0
	for i < len(w.writes) && w.writes[i].end <= n {
		i++
	}
	if i > 0 {
		if rtt := now.Sub(w.writes[i-1].at); w.minRTT == 0 || rtt < w.minRTT {
			w.minRTT = max(rtt, time.Millisecond)
		}
		w.writes = w.writes[i:]
	}
	w.acked = n
	if w.sampleAt.IsZero() {
		w.sampleAt, w.sampleAcked = now, n
	} else if span := now.Sub(w.sampleAt); span >= max(w.minRTT, 10*time.Millisecond) {
		w.rates = append(w.rates, float64(n-w.sampleAcked)/span.Seconds())
		if len(w.rates) > windowRateSpan {
			w.rates = w.rates[1:]
		}
		w.sampleAt, w.sampleAcked = now, n
	}
	w.cond.Broadcast()
}

// close lets every write through from now on: the session ended, or the
// move did.
func (w *ackWindow) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

// openSession is a session the source holds open after the destination's
// verified line: the stream's writer, which takes the commit, the pipe to
// ssh, and the session's end.
type openSession struct {
	sw   *moveStreamWriter
	pw   *io.PipeWriter
	win  *ackWindow
	done chan sessionResult
}

// sessionResult is how a session ended: its last line, or why there was
// none.
type sessionResult struct {
	reply *migrateReply
	err   error
}

// end closes the stream with no commit, which leaves the guest restored
// paused on the destination, and waits for the session to end.
func (o *openSession) end() sessionResult {
	o.win.close()
	_ = o.pw.Close()
	return <-o.done
}

// commitInSession sends the commit in the open session and returns its
// answer. A nil reply means the session ended without one.
func (m *moveSource) commitInSession() (*migrateReply, error) {
	o := m.session
	m.session = nil
	o.win.close()
	werr := o.sw.commit(moveStreamCommit{TxID: m.rec.TxID, Epoch: m.rec.Epoch})
	_ = o.pw.Close()
	res := <-o.done
	if res.reply == nil && res.err == nil {
		res.err = werr
	}
	return res.reply, res.err
}

// endSession ends a session left open by a move that stops before its
// commit: the destination keeps the guest restored paused.
func (m *moveSource) endSession() {
	if m.session != nil {
		o := m.session
		m.session = nil
		o.end()
	}
}
