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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"
)

// The streamed move's wire format: what `hull migrate` writes into one ssh
// session and `hull migrate-receive --stream` reads (docs/migration.md,
// "The streamed move"). It carries hvi's migrate stream as hvi wrote it:
// the disk rounds (when the guest's disk has a base), the RAM rounds, the
// machine's state and the manifest, which covers every one of them. hull
// adds a header naming the move and the base, and a trailer.
//
//	"HULLMIG3"
//	'H' u32 len, JSON moveStreamHeader
//	'C' u32 len, hvi's component header line, then size bytes
//	'T' u32 len, JSON moveStreamTrailer
//	'K' u32 len, JSON moveStreamCommit (a session only, see below)
//
// Integers are big-endian. Every component's bytes are checked against the
// sha256 in hvi's header line as they are relayed, and hvi checks them
// again, and the manifest over them, as it restores. The trailer carries
// the number of records and a running sha256 over every record's kind and
// header, so a record lost, added, reordered or altered fails the move, and
// a stream that ends without its trailer is refused.
//
// In a session (moveCapSession) the stream does not end at its trailer:
// once the destination has said the move is verified, the source records
// its commit point and sends the commit as one 'K' record, or ends the
// stream to leave the guest restored paused there. A reader that is not in
// a session refuses anything after the trailer.
//
// HULLMIG2 (stream-v2) also carried disk blocks hull read itself, in 'D'
// records; a hull of this version neither sends nor reads it, and a move
// between the two takes the whole-bundle path (moveCapStream).
const moveStreamMagic = "HULLMIG3"

// Limits a reader holds the stream to before it allocates or writes.
const (
	moveMaxRecords = 1 << 22
	// moveMaxComponents bounds hvi's frames: a RAM round comes in frames
	// of about 1 MiB, so a 64 GiB guest's first round alone is 65536.
	moveMaxComponents = 1 << 21
)

// hviComponentName is a name an hvi component may carry.
var hviComponentName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// moveStreamHeader is the 'H' record.
type moveStreamHeader struct {
	TxID  string `json:"txid"`
	Name  string `json:"name"`
	Epoch uint64 `json:"epoch"`
	// BaseSHA256 is the base hvi's disk rounds apply to, which the
	// destination copies to make the guest's disk, and DiskSize the size
	// of the guest's disk, which the base's must equal.
	BaseSHA256 string `json:"baseSha256"`
	DiskSize   int64  `json:"diskSize"`
	// Replay says the hvi components come from the source's own copy of a
	// stream hvi already sealed, sent again after a failed attempt.
	Replay bool `json:"replay,omitempty"`
	// Attempt names this attempt of the move; the destination records it
	// as accepted once it has taken the header and copied the base.
	Attempt string `json:"attempt,omitempty"`
}

// hviComponentHeader is what hull reads of an hvi component's header line;
// the line itself goes on as hvi wrote it.
type hviComponentHeader struct {
	Component string `json:"component"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// moveStreamTrailer is the 'T' record.
type moveStreamTrailer struct {
	Records int64  `json:"records"`
	Chain   string `json:"chain"`
	// HviReply is hvi's reply to the migrate op, after its manifest.
	HviReply json.RawMessage `json:"hviReply,omitempty"`
}

// moveStreamCommit is the 'K' record: the source's commit of the move it
// has recorded committing, sent in the session the stream came in.
type moveStreamCommit struct {
	TxID  string `json:"txid"`
	Epoch uint64 `json:"epoch"`
}

// moveChain is the running hash of the records' kinds and headers.
type moveChain struct {
	h hash.Hash
	n int64
}

func newMoveChain() *moveChain { return &moveChain{h: sha256.New()} }

func (c *moveChain) add(kind byte, hdr []byte) {
	var b [5]byte
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(hdr)))
	c.h.Write(b[:])
	c.h.Write(hdr)
	c.n++
}

func (c *moveChain) sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

// moveStreamWriter writes one streamed move.
type moveStreamWriter struct {
	cw    *countingWriter
	bw    *bufio.Writer
	chain *moveChain
}

func newMoveStreamWriter(w io.Writer) *moveStreamWriter {
	return newMoveStreamWriterSize(w, 1<<20)
}

// newMoveStreamWriterSize is newMoveStreamWriter with a buffer of size.
func newMoveStreamWriterSize(w io.Writer, size int) *moveStreamWriter {
	cw := &countingWriter{w: w}
	return &moveStreamWriter{cw: cw, bw: bufio.NewWriterSize(cw, size), chain: newMoveChain()}
}

// written is the bytes of the stream so far.
func (sw *moveStreamWriter) written() int64 { return sw.cw.n + int64(sw.bw.Buffered()) }

// flush hands what is buffered to the underlying writer.
func (sw *moveStreamWriter) flush() error { return sw.bw.Flush() }

func (sw *moveStreamWriter) record(kind byte, hdr []byte) error {
	if len(hdr) > migrateMaxHeader {
		return fmt.Errorf("a %q record header of %d bytes is over the limit", kind, len(hdr))
	}
	var b [5]byte
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(hdr)))
	if _, err := sw.bw.Write(b[:]); err != nil {
		return err
	}
	if _, err := sw.bw.Write(hdr); err != nil {
		return err
	}
	if kind != 'T' {
		sw.chain.add(kind, hdr)
	}
	return nil
}

func (sw *moveStreamWriter) header(h moveStreamHeader) error {
	if _, err := sw.bw.WriteString(moveStreamMagic); err != nil {
		return err
	}
	js, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return sw.record('H', js)
}

// component relays one hvi component: line is its header line as hvi wrote
// it (without the newline), body its bytes. The bytes are checked against
// the line's sha256 as they pass, and a component that does not match fails
// the stream: a source never sends what hvi did not seal.
func (sw *moveStreamWriter) component(line []byte, hdr hviComponentHeader, body io.Reader, also io.Writer) error {
	if err := sw.record('C', line); err != nil {
		return err
	}
	h := sha256.New()
	dst := io.MultiWriter(sw.bw, h)
	if also != nil {
		dst = io.MultiWriter(sw.bw, h, also)
	}
	n, err := io.Copy(dst, io.LimitReader(body, hdr.Size))
	if err != nil {
		return fmt.Errorf("relay %s: %w", hdr.Component, err)
	}
	if n != hdr.Size {
		return fmt.Errorf("hvi's %s ended at %d of its %d bytes", hdr.Component, n, hdr.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hdr.SHA256 {
		return fmt.Errorf("hvi's %s has sha256 %s, and its header says %s", hdr.Component, got, hdr.SHA256)
	}
	return nil
}

// trailer seals the stream and flushes it.
func (sw *moveStreamWriter) trailer(hviReply json.RawMessage) error {
	js, err := json.Marshal(moveStreamTrailer{Records: sw.chain.n, Chain: sw.chain.sum(), HviReply: hviReply})
	if err != nil {
		return err
	}
	if err := sw.record('T', js); err != nil {
		return err
	}
	return sw.bw.Flush()
}

// commit sends the 'K' record after the trailer and flushes it.
func (sw *moveStreamWriter) commit(c moveStreamCommit) error {
	js, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := sw.record('K', js); err != nil {
		return err
	}
	return sw.bw.Flush()
}

// moveStreamHandler is what a reader hands each record to, in order.
type moveStreamHandler interface {
	// header is the 'H' record.
	header(h moveStreamHeader) error
	// component is an hvi component: line is hvi's header line, body its
	// bytes, which the handler reads in full. They are checked against the
	// line's sha256 once the handler returns, and a mismatch fails the
	// stream: the handler must not act on a component before then except
	// to relay it to an hvi that checks it too.
	component(line []byte, hdr hviComponentHeader, body io.Reader) error
	// componentVerified follows component once its bytes matched.
	componentVerified(hdr hviComponentHeader) error
}

// moveStreamResult is what a reader received.
type moveStreamResult struct {
	Header  moveStreamHeader
	Trailer moveStreamTrailer
	Bytes   int64
	// Components are the hvi components by name, in order.
	Components []hviComponentHeader
}

// readMoveStream reads a streamed move from r, handing each record to h in
// order. Nothing in the stream is trusted: every length, name, index and
// count is held to the limits before anything is allocated or handed on,
// and a stream with no trailer, a trailer that disagrees, or bytes after it
// is refused.
func readMoveStream(r io.Reader, h moveStreamHandler) (*moveStreamResult, error) {
	res, _, err := readMoveStreamOpen(r, h, false)
	return res, err
}

// readMoveStreamOpen is readMoveStream for a session when open is set: the
// stream may go on after its trailer, and the reader it returns holds what
// follows, for readMoveCommit.
func readMoveStreamOpen(r io.Reader, h moveStreamHandler, open bool) (*moveStreamResult, *bufio.Reader, error) {
	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, 1<<20)
	res, err := readMoveRecords(cr, br, h, open)
	return res, br, err
}

// readMoveCommit reads what follows a session's trailer: the source's
// commit, or nil at the end of the stream, which leaves the move verified.
func readMoveCommit(br *bufio.Reader) (*moveStreamCommit, error) {
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return nil, nil
	}
	kind, js, err := readStreamRecord(br)
	if err != nil {
		return nil, err
	}
	if kind != 'K' {
		return nil, fmt.Errorf("the stream goes on after its trailer with a %q record", kind)
	}
	var c moveStreamCommit
	if err := strictUnmarshal(js, &c); err != nil {
		return nil, fmt.Errorf("the commit: %w", err)
	}
	return &c, nil
}

func readMoveRecords(cr *countingReader, br *bufio.Reader, h moveStreamHandler, open bool) (*moveStreamResult, error) {
	magic := make([]byte, len(moveStreamMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, fmt.Errorf("the stream ended before it began: %w", err)
	}
	if string(magic) != moveStreamMagic {
		return nil, fmt.Errorf("the stream does not start with %s", moveStreamMagic)
	}
	res := &moveStreamResult{}
	chain := newMoveChain()
	kind, js, err := readStreamRecord(br)
	if err != nil {
		return nil, err
	}
	if kind != 'H' {
		return nil, fmt.Errorf("the stream starts with a %q record, not its header", kind)
	}
	if err := strictUnmarshal(js, &res.Header); err != nil {
		return nil, fmt.Errorf("stream header: %w", err)
	}
	if err := checkMoveHeader(res.Header); err != nil {
		return nil, err
	}
	chain.add(kind, js)
	if err := h.header(res.Header); err != nil {
		return nil, err
	}
	for {
		kind, js, err := readStreamRecord(br)
		if err != nil {
			return nil, err
		}
		if chain.n >= moveMaxRecords {
			return nil, fmt.Errorf("the stream has more than %d records", moveMaxRecords)
		}
		switch kind {
		case 'C':
			c, err := parseHviComponentHeader(js)
			if err != nil {
				return nil, err
			}
			if len(res.Components) >= moveMaxComponents {
				return nil, fmt.Errorf("the stream carries more than %d hvi components", moveMaxComponents)
			}
			chain.add(kind, js)
			sum := sha256.New()
			counted := &countingReader{r: io.LimitReader(br, c.Size)}
			body := io.TeeReader(counted, sum)
			herr := h.component(js, c, body)
			// Whatever the handler left unread is read here, so the
			// stream stays in step and the hash covers every byte.
			_, cerr := io.Copy(io.Discard, body)
			if cerr == nil && counted.n != c.Size {
				cerr = io.ErrUnexpectedEOF
			}
			if cerr != nil {
				return nil, fmt.Errorf("the stream ended in hvi's %s: %w", c.Component, cerr)
			}
			if herr != nil {
				return nil, herr
			}
			if got := hex.EncodeToString(sum.Sum(nil)); got != c.SHA256 {
				return nil, fmt.Errorf("hvi's %s arrived corrupted: sha256 %s, its header says %s", c.Component, got, c.SHA256)
			}
			if err := h.componentVerified(c); err != nil {
				return nil, err
			}
			res.Components = append(res.Components, c)
		case 'T':
			var t moveStreamTrailer
			if err := strictUnmarshal(js, &t); err != nil {
				return nil, fmt.Errorf("stream trailer: %w", err)
			}
			if t.Records != chain.n || t.Chain != chain.sum() {
				return nil, fmt.Errorf("the trailer counts %d records with chain %s; %d arrived with chain %s",
					t.Records, t.Chain, chain.n, chain.sum())
			}
			res.Trailer = t
			res.Bytes = cr.n - int64(br.Buffered())
			if open {
				return res, nil
			}
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return nil, errors.New("the stream goes on after its trailer")
			}
			return res, nil
		default:
			return nil, fmt.Errorf("the stream has a record of unknown kind %q", kind)
		}
	}
}

// checkMoveHeader holds the 'H' record to what a move can be.
func checkMoveHeader(h moveStreamHeader) error {
	switch {
	case checkTxID(h.TxID) != nil:
		return fmt.Errorf("the stream names move %q", h.TxID)
	case h.DiskSize <= 0 || h.DiskSize > migrateMaxDiskSize:
		return fmt.Errorf("the stream says the disk is %d bytes", h.DiskSize)
	case checkDiskDigest(h.BaseSHA256) != nil:
		return fmt.Errorf("the stream names base %q", h.BaseSHA256)
	}
	return nil
}

// parseHviComponentHeader reads an hvi component's header line and holds it
// to the limits. Fields hull does not know are left to hvi, which reads the
// line again on the destination.
func parseHviComponentHeader(line []byte) (hviComponentHeader, error) {
	var c hviComponentHeader
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("an hvi component header: %w", err)
	}
	if dec.More() {
		return c, errors.New("an hvi component header has more than one object")
	}
	switch {
	case !hviComponentName.MatchString(c.Component) || strings.Contains(c.Component, ".."):
		return c, fmt.Errorf("an hvi component is named %q", c.Component)
	case c.Size < 0 || c.Size > migrateMaxFileSize:
		return c, fmt.Errorf("hvi's %s says %d bytes", c.Component, c.Size)
	case len(c.SHA256) != 64:
		return c, fmt.Errorf("hvi's %s has no sha256", c.Component)
	}
	return c, nil
}

// sha256Hex is the sha256 of b, as lowercase hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
