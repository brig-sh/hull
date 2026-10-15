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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// hvi's streaming ops on the control socket, which a streamed move drives:
// `migrate` (RAM rounds, the pause, the machine's state and the manifest)
// and `disk-blocks` (the bytes of disk blocks). Each answers with a run of
// components, every one a JSON header line naming its size and sha256 and
// then its bytes, and ends with the op's reply line, the one object with a
// "v" key (hvi docs/checkpoint.md). `disk-dirty` answers with one line, as
// the other ops do, but its bitmap can outgrow hviControlMaxReply.

// hviStreamIdle is how long a streaming op may go without a byte from hvi
// before hull gives up on it.
const hviStreamIdle = 2 * time.Minute

// hviDiskDirtyMaxReply bounds a disk-dirty reply: the bitmap of a 4 TiB disk
// in 64 KiB blocks is 8 MiB, 11 MiB as base64.
const hviDiskDirtyMaxReply = 12 << 20

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
	return conn, bufio.NewReaderSize(conn, 1<<20), nil
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

// hviDiskDirtyReply is disk-dirty's answer.
type hviDiskDirtyReply struct {
	V          int    `json:"v"`
	OK         bool   `json:"ok"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	Since      string `json:"since"`
	Round      int    `json:"round"`
	BlockSize  int64  `json:"block_size"`
	Blocks     int64  `json:"blocks"`
	Dirty      int64  `json:"dirty"`
	Generation uint64 `json:"generation"`
	BitmapB64  string `json:"bitmap_b64"`
	// BitmapSHA256 is the sha256 of the bitmap's bytes, from an hvi that
	// records each round's digests.
	BitmapSHA256 string `json:"bitmap_sha256,omitempty"`
}

// dirtyBlocks decodes the bitmap into the block numbers it sets, ascending,
// held to the disk's size.
func (r *hviDiskDirtyReply) dirtyBlocks(diskSize int64) ([]int64, error) {
	if r.BlockSize != diskBlockSize {
		return nil, fmt.Errorf("hvi tracks %d-byte disk blocks, hull %d", r.BlockSize, diskBlockSize)
	}
	if r.Blocks != diskBlocks(diskSize) {
		return nil, fmt.Errorf("hvi's disk has %d blocks, the disk file %d", r.Blocks, diskBlocks(diskSize))
	}
	bits, err := base64.StdEncoding.DecodeString(r.BitmapB64)
	if err != nil {
		return nil, fmt.Errorf("hvi's dirty bitmap: %w", err)
	}
	if int64(len(bits)) != (r.Blocks+7)/8 {
		return nil, fmt.Errorf("hvi's dirty bitmap is %d bytes for %d blocks", len(bits), r.Blocks)
	}
	if r.BitmapSHA256 != "" && sha256Hex(bits) != r.BitmapSHA256 {
		return nil, fmt.Errorf("hvi's dirty bitmap of round %d has sha256 %s, its reply says %s", r.Round,
			sha256Hex(bits), r.BitmapSHA256)
	}
	var out []int64
	for i, b := range bits {
		for j := 0; b != 0; j++ {
			if b&1 != 0 {
				block := int64(i)*8 + int64(j)
				if block >= r.Blocks {
					return nil, fmt.Errorf("hvi's dirty bitmap sets block %d of %d", block, r.Blocks)
				}
				out = append(out, block)
			}
			b >>= 1
		}
	}
	if int64(len(out)) != r.Dirty {
		return nil, fmt.Errorf("hvi's dirty bitmap sets %d blocks and says %d", len(out), r.Dirty)
	}
	return out, nil
}

// hviDiskDirty asks hvi for the disk blocks written since boot or since the
// last read ("boot" or "round"), which starts a round.
func hviDiskDirty(sock, since string) (*hviDiskDirtyReply, error) {
	conn, br, err := hviStreamOp(sock, map[string]any{"op": "disk-dirty", "since": since}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	line, err := readHviLine(br, hviDiskDirtyMaxReply)
	if err != nil {
		return nil, fmt.Errorf("hvi's reply to disk-dirty: %w", err)
	}
	var reply hviDiskDirtyReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return nil, fmt.Errorf("hvi's reply to disk-dirty: %w", err)
	}
	if reply.V != hviControlVersion {
		return nil, fmt.Errorf("hvi replied to disk-dirty with protocol version %d", reply.V)
	}
	if !reply.OK {
		return &reply, &hviRefusal{Op: "disk-dirty", Code: reply.Code, Message: reply.Error}
	}
	return &reply, nil
}

// hviLacksOp reports whether err is hvi saying it has no such op: one from
// before it.
func hviLacksOp(err error) bool {
	code := hviRefusalCode(err)
	return code == "unknown_op" || code == "not_implemented"
}

// hviDiskBlocksReply is what hull reads of disk-blocks' reply.
type hviDiskBlocksReply struct {
	Round      *int   `json:"round"`
	Blocks     int64  `json:"blocks"`
	Bytes      int64  `json:"bytes"`
	Batches    int64  `json:"batches"`
	SentSHA256 string `json:"sent_sha256"`
}

// hviDiskBlocks streams the blocks of round from hvi, handing each batch to
// fn with its first block and its bytes, checked against its sha256, and
// holds hvi's reply to what came: as many blocks, bytes and batches, and,
// from an hvi that gives it, the same sent_sha256, the sha256 of the
// batches' digests in order. It returns that digest.
func hviDiskBlocks(sock string, round int, fn func(first, count int64, data []byte) error) (string, error) {
	conn, br, err := hviStreamOp(sock, map[string]any{"op": "disk-blocks", "round": round}, hviStreamIdle)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, moveMaxBatchBlocks*diskBlockSize)
	var got diskRoundDigest
	var bytes int64
	_, line, err := readHviComponents("disk-blocks", br, func(_ []byte, hdr hviComponentHeader, body io.Reader) error {
		if hdr.First == nil || hdr.Count == nil || *hdr.Count < 1 || *hdr.Count > moveMaxBatchBlocks ||
			hdr.Size > *hdr.Count*diskBlockSize {
			return fmt.Errorf("hvi sent a disk batch of %d bytes that names no blocks", hdr.Size)
		}
		data := buf[:hdr.Size]
		if _, err := io.ReadFull(body, data); err != nil {
			return fmt.Errorf("hvi's disk batch at block %d: %w", *hdr.First, err)
		}
		if got := sha256Hex(data); got != hdr.SHA256 {
			return fmt.Errorf("hvi's disk batch at block %d has sha256 %s, its header says %s", *hdr.First, got, hdr.SHA256)
		}
		got.add(*hdr.Count, hdr.SHA256)
		bytes += hdr.Size
		return fn(*hdr.First, *hdr.Count, data)
	})
	if err != nil {
		return "", err
	}
	var reply hviDiskBlocksReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return "", fmt.Errorf("hvi's reply to disk-blocks: %w", err)
	}
	sent := got.done()
	switch {
	case reply.Round != nil && *reply.Round != round:
		return "", fmt.Errorf("hvi sent the blocks of round %d, not %d", *reply.Round, round)
	case reply.Blocks != sent.Blocks || reply.Batches != sent.Batches || reply.Bytes != bytes:
		return "", fmt.Errorf("hvi says it sent %d blocks (%d bytes) in %d batches of round %d; %d (%d bytes) in "+
			"%d came", reply.Blocks, reply.Bytes, reply.Batches, round, sent.Blocks, bytes, sent.Batches)
	case reply.SentSHA256 != "" && reply.SentSHA256 != sent.SentSHA256:
		return "", fmt.Errorf("hvi's sent_sha256 of round %d is %s; the batches that came make %s", round,
			reply.SentSHA256, sent.SentSHA256)
	}
	return sent.SentSHA256, nil
}
