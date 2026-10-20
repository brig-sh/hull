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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// hvi's streaming op on the control socket, which a streamed move drives:
// `migrate` (the disk and RAM rounds, the pause, the machine's state and
// the manifest). It answers with a run of components, every one a JSON
// header line naming its size and sha256 and then its bytes, and ends with
// the op's reply line, the one object with a "v" key (hvi
// docs/checkpoint.md, "The stream format").

// hviStreamIdle is how long a streaming op may go without a byte from hvi
// before hull gives up on it. hvi cuts a reader that takes nothing for 30 s
// itself, and a final round that runs past its bound, and closes the
// connection when it does, so this only catches an hvi that stopped
// writing. A move's relay waits less once the paused part of the stream
// has begun (hviPausedBound).
const hviStreamIdle = 2 * time.Minute

// hviTailMax is how much of the end of hvi's stream a connection keeps, to
// find the refusal hvi writes before it closes a stream that failed. A
// refusal line is a few hundred bytes.
const hviTailMax = 4096

// idleConn extends the connection's deadline before every read and write,
// so a stream that keeps moving is never cut, and one that stalls is. It
// keeps the last bytes read and the error the reads ended with, which say
// whether hvi closed the stream and what it said first (hviEnded). One
// goroutine reads it.
type idleConn struct {
	net.Conn
	idle atomic.Int64
	tail []byte
	end  error
}

// newIdleConn wraps conn with an idle bound of idle.
func newIdleConn(conn net.Conn, idle time.Duration) *idleConn {
	c := &idleConn{Conn: conn, tail: make([]byte, 0, hviTailMax)}
	c.idle.Store(int64(idle))
	return c
}

// setIdle changes the bound from the next read or write on.
func (c *idleConn) setIdle(idle time.Duration) { c.idle.Store(int64(idle)) }

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(time.Duration(c.idle.Load())))
	n, err := c.Conn.Read(p)
	c.keepTail(p[:n])
	if err != nil && c.end == nil {
		c.end = err
	}
	return n, err
}

// keepTail keeps the last hviTailMax bytes of what was read, b last.
func (c *idleConn) keepTail(b []byte) {
	if len(b) >= hviTailMax {
		c.tail = append(c.tail[:0], b[len(b)-hviTailMax:]...)
		return
	}
	if over := len(c.tail) + len(b) - hviTailMax; over > 0 {
		c.tail = append(c.tail[:0], c.tail[over:]...)
	}
	c.tail = append(c.tail, b...)
}

// hviEnded returns err as an *hviStreamEndError when the reads of hvi's
// stream ended because hvi closed the connection: the end of the stream,
// or, on Linux, a reset, which a peer that closes with bytes of ours unread
// gives. hvi closes a stream that failed once it began, and writes its
// refusal first when the socket has room for it (hvi docs/cli.md, "The
// control socket"). Any other err, a failure of hull's own or a deadline,
// comes back as it is.
func (c *idleConn) hviEnded(op string, err error) error {
	if !errors.Is(c.end, io.EOF) && !errors.Is(c.end, syscall.ECONNRESET) {
		return err
	}
	refusal, partial := hviTailRefusal(op, c.tail)
	return &hviStreamEndError{op: op, refusal: refusal, partial: partial, err: err}
}

// hviTailRefusal finds hvi's refusal at the end of what it sent before it
// closed a stream: a line with a "v" key running to the end of tail. hvi
// gives the refusal one write, so it may come cut short; it may also land
// among a frame's bytes, where the reader took it for a part of the frame.
// A whole refusal comes back as an *hviRefusal; what arrived of one cut
// short, or of a line that does not parse, as text. Neither, when tail does
// not end in such a line.
func hviTailRefusal(op string, tail []byte) (*hviRefusal, string) {
	i := bytes.LastIndex(tail, []byte(`{"v":`))
	if i < 0 {
		return nil, ""
	}
	line := tail[i:]
	if j := bytes.IndexByte(line, '\n'); j >= 0 {
		if j != len(line)-1 {
			return nil, ""
		}
		line = line[:j]
		var r hviReply
		if json.Unmarshal(line, &r) == nil && !r.OK && r.Code != "" {
			return &hviRefusal{Op: op, Code: r.Code, Message: r.Error}, ""
		}
	}
	// hvi's lines are printable text; anything else is a frame's bytes.
	if !utf8.Valid(line) || bytes.ContainsFunc(line, unicode.IsControl) {
		return nil, ""
	}
	return nil, string(line)
}

// hviStreamEndError is hvi closing a streaming op's connection before its
// reply, as it does when the stream fails once it has begun, with the
// refusal it wrote first if hull found it at the end of the stream, whole
// or as it arrived.
type hviStreamEndError struct {
	op      string
	refusal *hviRefusal
	partial string
	err     error
}

func (e *hviStreamEndError) Error() string {
	switch {
	case e.refusal != nil:
		return fmt.Sprintf("hvi failed its %s stream and closed it (%v): %v", e.op, e.err, e.refusal)
	case e.partial != "":
		return fmt.Sprintf("hvi failed its %s stream and closed it (%v): its refusal, cut short: %q",
			e.op, e.err, e.partial)
	}
	return fmt.Sprintf("hvi closed its %s stream, as it does when the stream fails, and hull read no "+
		"refusal (%v)", e.op, e.err)
}

func (e *hviStreamEndError) Unwrap() []error {
	if e.refusal != nil {
		return []error{e.err, e.refusal}
	}
	return []error{e.err}
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(time.Duration(c.idle.Load())))
	return c.Conn.Write(p)
}

// hviStreamOp sends one request on sock and returns the connection and a
// reader for what hvi streams back. The caller closes the connection.
func hviStreamOp(sock string, req map[string]any, idle time.Duration) (net.Conn, *bufio.Reader, error) {
	return hviStreamOpSize(sock, req, idle, 1<<20)
}

// hviStreamOpSize is hviStreamOp with a reader of size bytes.
func hviStreamOpSize(sock string, req map[string]any, idle time.Duration, size int) (net.Conn, *bufio.Reader, error) {
	msg := map[string]any{"v": hviControlVersion}
	for k, v := range req {
		msg[k] = v
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, nil, err
	}
	raw, err := net.DialTimeout("unix", sock, hviControlTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to hvi's control socket %s: %w", sock, err)
	}
	conn := newIdleConn(raw, idle)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		_ = raw.Close()
		return nil, nil, fmt.Errorf("send %v to hvi: %w", req["op"], err)
	}
	return conn, bufio.NewReaderSize(conn, size), nil
}

// readHviLine reads one line of at most max bytes, without its newline.
func readHviLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return nil, fmt.Errorf("hvi sent a line longer than %d bytes", max)
		}
		if err == nil {
			return bytes.TrimRight(line, "\r\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("hvi closed the connection before its reply")
			}
			return nil, err
		}
	}
}

// readHviComponents reads hvi's components from br, handing each to fn,
// which must read body in full or fail, and returns hvi's reply line once
// it comes. A reply that refuses is returned with an *hviRefusal.
func readHviComponents(op string, br *bufio.Reader,
	fn func(line []byte, hdr hviComponentHeader, body io.Reader) error) (*hviReply, json.RawMessage, error) {
	for {
		line, err := readHviLine(br, migrateMaxHeader)
		if err != nil {
			return nil, nil, fmt.Errorf("hvi's %s stream: %w", op, err)
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, nil, fmt.Errorf("hvi's %s stream has a line that is not JSON: %w", op, err)
		}
		if _, isReply := probe["v"]; isReply {
			var reply hviReply
			if err := json.Unmarshal(line, &reply); err != nil {
				return nil, nil, fmt.Errorf("hvi's reply to %s: %w", op, err)
			}
			if reply.V != hviControlVersion {
				return nil, nil, fmt.Errorf("hvi replied to %s with protocol version %d", op, reply.V)
			}
			if !reply.OK {
				return &reply, line, &hviRefusal{Op: op, Code: reply.Code, Message: reply.Error}
			}
			return &reply, line, nil
		}
		hdr, err := parseHviComponentHeader(line)
		if err != nil {
			return nil, nil, err
		}
		body := io.LimitReader(br, hdr.Size)
		if err := fn(line, hdr, body); err != nil {
			return nil, nil, err
		}
		if n, _ := io.Copy(io.Discard, body); n != 0 {
			return nil, nil, fmt.Errorf("hull left %d bytes of hvi's %s unread", n, hdr.Component)
		}
	}
}
