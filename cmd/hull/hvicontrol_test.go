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
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubHvi is a stand-in for hvi's control socket: it reads one request line
// per connection, records it, and answers with whatever reply the test's
// handler returns, after the handler's delay.
type stubHvi struct {
	path string

	mu       sync.Mutex
	requests []map[string]any
}

type stubAnswer struct {
	reply string
	delay time.Duration
}

func newStubHvi(t *testing.T, handle func(req map[string]any) stubAnswer) *stubHvi {
	t.Helper()
	stub := &stubHvi{path: filepath.Join(shortSocketDir(t), "c.sock")}
	ln, err := net.Listen("unix", stub.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				stub.mu.Lock()
				stub.requests = append(stub.requests, req)
				stub.mu.Unlock()
				ans := handle(req)
				select {
				case <-time.After(ans.delay):
				case <-done:
					return
				}
				_, _ = conn.Write([]byte(ans.reply + "\n"))
			}()
		}
	}()
	return stub
}

func (s *stubHvi) seen() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.requests...)
}

func TestHviControlSendsVersionAndReadsState(t *testing.T) {
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"paused","vcpus":2,"vcpus_parked":2}`}
	})
	reply, err := hviOp(stub.path, "pause")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if reply.State != "paused" || reply.Vcpus != 2 || reply.VcpusParked != 2 {
		t.Errorf("reply = %+v", reply)
	}
	reqs := stub.seen()
	if len(reqs) != 1 || reqs[0]["v"] != float64(1) || reqs[0]["op"] != "pause" {
		t.Errorf("requests = %v, want one {v:1, op:pause}", reqs)
	}
}

func TestHviControlRefusalCarriesTheCode(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"fenced","error":"the VM was fenced"}`}
	})
	_, err := hviOp(stub.path, "resume")
	if err == nil {
		t.Fatal("resume of a fenced VM succeeded")
	}
	if got := hviRefusalCode(err); got != "fenced" {
		t.Errorf("code = %q, want fenced (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "the VM was fenced") {
		t.Errorf("error %q does not carry hvi's reason", err)
	}
}

// A pause whose reply comes after the client's timeout must surface as
// errHviNoReply: the caller has to tell the user the VM may now be paused,
// which a plain I/O error would not prompt.
func TestHviControlSlowPauseTimesOut(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"paused"}`, delay: 4 * time.Second}
	})
	start := time.Now()
	_, err := hviControl(stub.path, map[string]any{"op": "pause"}, 3*time.Second)
	if !errors.Is(err, errHviNoReply) {
		t.Fatalf("err = %v, want errHviNoReply", err)
	}
	if took := time.Since(start); took < 3*time.Second || took > 4*time.Second {
		t.Errorf("gave up after %s, want the 3s timeout", took)
	}
}

// hvi's default client budget must stay at or above the 3 s hvi documents.
func TestHviControlTimeoutCoversHvisPauseDeadline(t *testing.T) {
	if hviControlTimeout < 3*time.Second {
		t.Errorf("hviControlTimeout = %s, hvi asks for at least 3s", hviControlTimeout)
	}
}

func TestHviControlRefusesAnotherProtocolVersion(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":2,"ok":true,"state":"running"}`}
	})
	if _, err := hviOp(stub.path, "status"); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("err = %v, want a protocol version refusal", err)
	}
}

func TestHviControlHangupWithoutReply(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	_, err = hviOp(path, "status")
	if err == nil || errors.Is(err, errHviNoReply) {
		t.Errorf("err = %v, want a closed-connection error that is not a timeout", err)
	}
}

func TestHviControlNoSocket(t *testing.T) {
	_, err := hviOp(filepath.Join(shortSocketDir(t), "absent.sock"), "status")
	if err == nil || !strings.Contains(err.Error(), "connect to hvi's control socket") {
		t.Errorf("err = %v", err)
	}
}

func TestReadHviReplyRefusesAnOverlongLine(t *testing.T) {
	long := strings.Repeat("x", hviControlMaxReply+10)
	if _, err := readHviReply(strings.NewReader(long)); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("err = %v, want a length refusal", err)
	}
}
