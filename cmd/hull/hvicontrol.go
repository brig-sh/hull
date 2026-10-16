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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// hviControlVersion is the protocol version every request and reply on hvi's
// control socket carries as "v". hvi answers a request without it with
// unsupported_version, so it is never optional here.
const hviControlVersion = 1

// hviControlTimeout bounds the control ops that change or report the VM's
// run state for a person or a move: pause, resume, status, profile, and the
// floor of a checkpoint's wait.
//
// hvi waits up to 10 s for its I/O threads at a pause, then up to 2 s for
// the vCPUs, and serves one connection at a time, so any request on another
// connection waits behind a pause for as long. It asks a client that may
// meet a pause, its own or another's, for a reply timeout of at least 13 s
// (hvi docs/cli.md, "The control socket"). A shorter one gives up on a pause
// that then succeeds, leaving a VM paused that hull reported as not paused,
// or on a resume that then runs the guest. 15 s leaves room for a loaded
// host, as 5 s did over the 3 s asked before, without letting a wedged VMM
// hang the CLI for long.
//
// Calls whose failure is safe keep a shorter bound: the status ps, exec and
// a move's checks ask (hviVMStateTimeout), and the stop and the restore's
// polls (hviShortTimeout).
//
// A variable so a test need not wait it out.
var hviControlTimeout = 15 * time.Second

// hviShortTimeout bounds the control ops whose failure hull handles without
// harm: the stop, which falls back to signals, and the status a restore
// polls, in a loop that checks its own deadline and whether the VMM died
// only between calls. It is the 5 s every op had before a pause could take
// 12 s; behind another client's pause such a call gives up, and the signals
// or the next poll take over.
const hviShortTimeout = 5 * time.Second

// hviControlMaxReply bounds a reply line. The largest is the profile, a few
// KiB; the bound keeps a misbehaving peer from growing hull's buffer without
// end.
const hviControlMaxReply = 1 << 20

// hviReply is one reply line. Fields not used by an op are left zero.
type hviReply struct {
	V              int             `json:"v"`
	OK             bool            `json:"ok"`
	State          string          `json:"state,omitempty"`
	Vcpus          int             `json:"vcpus,omitempty"`
	VcpusParked    int             `json:"vcpus_parked,omitempty"`
	Code           string          `json:"code,omitempty"`
	Error          string          `json:"error,omitempty"`
	ManifestSHA256 string          `json:"manifest_sha256,omitempty"`
	Stats          json.RawMessage `json:"stats,omitempty"`
	Profile        json.RawMessage `json:"profile,omitempty"`
}

// hviRefusal is hvi answering ok:false. Callers branch on Code, which hvi
// keeps stable (src/checkpoint/control.rs ErrorCode); Message is prose for a
// person and may change.
type hviRefusal struct {
	Op      string
	Code    string
	Message string
}

func (e *hviRefusal) Error() string {
	return fmt.Sprintf("hvi refused %s: %s (%s)", e.Op, e.Message, e.Code)
}

// hviRefusalCode returns the code of an hviRefusal in err's chain, or "".
func hviRefusalCode(err error) string {
	var r *hviRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// errHviNoReply marks a request that was sent and got no reply in time. For a
// state-changing op that is not a "no": the op may still complete in hvi.
var errHviNoReply = errors.New("no reply from hvi")

// hviControl sends one request on sock and returns hvi's reply.
//
// One connection per request: hvi serves one connection at a time, so a
// client holding one open would lock every other hull command out of the VM.
// An ok:false reply comes back as an *hviRefusal with the reply alongside.
func hviControl(sock string, req map[string]any, timeout time.Duration) (*hviReply, error) {
	op, _ := req["op"].(string)
	msg := map[string]any{"v": hviControlVersion}
	for k, v := range req {
		msg[k] = v
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect to hvi's control socket %s: %w", sock, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("send %s to hvi: %w", op, err)
	}

	reply, err := readHviReply(conn)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, fmt.Errorf("%w to %s within %s", errHviNoReply, op, timeout)
		}
		return nil, fmt.Errorf("read hvi's reply to %s: %w", op, err)
	}
	if reply.V != hviControlVersion {
		return nil, fmt.Errorf("hvi replied to %s with protocol version %d, hull speaks %d",
			op, reply.V, hviControlVersion)
	}
	if !reply.OK {
		return reply, &hviRefusal{Op: op, Code: reply.Code, Message: reply.Error}
	}
	return reply, nil
}

// readHviReply reads one newline-terminated JSON object, refusing a line
// longer than hviControlMaxReply.
func readHviReply(r io.Reader) (*hviReply, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, hviControlMaxReply+1), 4096)
	line, err := br.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) > hviControlMaxReply {
				return nil, fmt.Errorf("reply longer than %d bytes", hviControlMaxReply)
			}
			return nil, errors.New("hvi closed the connection without a reply")
		}
		return nil, err
	}
	var reply hviReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return nil, fmt.Errorf("reply is not JSON: %w", err)
	}
	return &reply, nil
}

// hviOp sends a request with no arguments under the default timeout.
func hviOp(sock, op string) (*hviReply, error) {
	return hviControl(sock, map[string]any{"op": op}, hviControlTimeout)
}
