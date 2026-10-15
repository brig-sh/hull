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
	"time"
)

// hvi's streaming op on the control socket, which a streamed move drives:
// `migrate` (the disk and RAM rounds, the pause, the machine's state and
// the manifest). It answers with a run of components, every one a JSON
// header line naming its size and sha256 and then its bytes, and ends with
// the op's reply line, the one object with a "v" key (hvi
// docs/checkpoint.md, "The stream format").

// hviStreamIdle is how long a streaming op may go without a byte from hvi
// before hull gives up on it. hvi cuts a reader that takes nothing for 30 s
// itself, and a final round that runs past its bound, so this only catches
// an hvi that stopped writing.
const hviStreamIdle = 2 * time.Minute

// idleConn extends the connection's deadline before every read and write,
// so a stream that keeps moving is never cut, and one that stalls is.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
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
	conn := &idleConn{Conn: raw, idle: idle}
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
