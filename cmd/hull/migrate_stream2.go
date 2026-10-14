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

	"github.com/pierrec/lz4/v4"
)

// The streamed move's wire format: what `hull migrate` writes into one ssh
// session and `hull migrate-receive --stream` reads (docs/migration.md,
// "The streamed move"). It multiplexes two sources into one stream: the
// disk blocks the guest changed since its base, which hull reads, and the
// components of hvi's migrate stream (RAM rounds, then the machine's state
// and the manifest), which hull relays as hvi wrote them.
//
//	"HULLMIG2"
//	'H' u32 len, JSON moveStreamHeader
//	'D' u32 len, JSON diskBatchHeader, then size bytes      (disk blocks)
//	'C' u32 len, hvi's component header line, then size bytes
//	'T' u32 len, JSON moveStreamTrailer
//
// Integers are big-endian. Every record's payload carries its own sha256
// in its header, checked before the payload is used (a disk batch) or as
// it is relayed (a component, which hvi checks again). The trailer carries
// the number of records and a running sha256 over every record's kind and
// header, so a record lost, added, reordered or altered fails the move, and
// a stream that ends without its trailer is refused.
const moveStreamMagic = "HULLMIG2"

// Limits a reader holds the stream to before it allocates or writes.
const (
	moveMaxRecords    = 1 << 22
	moveMaxComponents = 4096
	// moveMaxBatchBlocks is the most blocks one disk batch carries; hvi's
	// own batches are at most 16.
	moveMaxBatchBlocks = 64
)

// hviComponentName is a name an hvi component may carry.
var hviComponentName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// moveStreamHeader is the 'H' record.
type moveStreamHeader struct {
	TxID       string `json:"txid"`
	Name       string `json:"name"`
	Epoch      uint64 `json:"epoch"`
	BaseSHA256 string `json:"baseSha256"`
	DiskSize   int64  `json:"diskSize"`
	BlockSize  int64  `json:"blockSize"`
	// Replay says the hvi components come from the source's own copy of a
	// stream hvi already sealed, sent again after a failed attempt.
	Replay bool `json:"replay,omitempty"`
}

// diskBatchHeader is a 'D' record: count adjacent blocks from first, as
// rawSize bytes whose sha256 is SHA256, sent as size bytes in codec.
type diskBatchHeader struct {
	Round   int    `json:"round"`
	Final   bool   `json:"final,omitempty"`
	First   int64  `json:"first"`
	Count   int64  `json:"count"`
	Codec   string `json:"codec"`
	RawSize int64  `json:"rawSize"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// Disk batch codecs.
const (
	codecRaw = "raw"
	codecLZ4 = "lz4"
)

// hviComponentHeader is what hull reads of an hvi component's header line;
// the line itself goes on as hvi wrote it.
type hviComponentHeader struct {
	Component string `json:"component"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	// First and Count are set on hvi's disk-blocks batches.
	First *int64 `json:"first,omitempty"`
	Count *int64 `json:"count,omitempty"`
}

// moveStreamTrailer is the 'T' record.
type moveStreamTrailer struct {
	Records int64  `json:"records"`
	Chain   string `json:"chain"`
	// HviReply is hvi's reply to the migrate op, after its manifest.
	HviReply json.RawMessage `json:"hviReply,omitempty"`
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
	lz    lz4.Compressor
	comp  []byte
}

func newMoveStreamWriter(w io.Writer) *moveStreamWriter {
	cw := &countingWriter{w: w}
	return &moveStreamWriter{cw: cw, bw: bufio.NewWriterSize(cw, 1<<20), chain: newMoveChain()}
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

// disk writes count adjacent blocks from first, raw being their bytes, and
// returns the bytes it put on the wire.
func (sw *moveStreamWriter) disk(round int, final bool, first, count int64, raw []byte) (int64, error) {
	sum := sha256.Sum256(raw)
	hdr := diskBatchHeader{Round: round, Final: final, First: first, Count: count, Codec: codecRaw,
		RawSize: int64(len(raw)), Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}
	payload := raw
	if bound := lz4.CompressBlockBound(len(raw)); len(sw.comp) < bound {
		sw.comp = make([]byte, bound)
	}
	if n, err := sw.lz.CompressBlock(raw, sw.comp); err == nil && n > 0 && n < len(raw) {
		hdr.Codec, hdr.Size, payload = codecLZ4, int64(n), sw.comp[:n]
	}
	js, err := json.Marshal(hdr)
	if err != nil {
		return 0, err
	}
	before := sw.written()
	if err := sw.record('D', js); err != nil {
		return 0, err
	}
	if _, err := sw.bw.Write(payload); err != nil {
		return 0, err
	}
	return sw.written() - before, nil
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

// moveStreamHandler is what a reader hands each record to, in order.
type moveStreamHandler interface {
	// header is the 'H' record.
	header(h moveStreamHeader) error
	// disk is a disk batch, decompressed and checked against its sha256.
	disk(hdr diskBatchHeader, raw []byte) error
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
	Disk    struct{ Batches, Blocks, Bytes int64 }
	// Components are the hvi components by name, in order.
	Components []hviComponentHeader
}

// readMoveStream reads a streamed move from r, handing each record to h in
// order. Nothing in the stream is trusted: every length, name, index and
// count is held to the limits before anything is allocated or handed on,
// and a stream with no trailer, a trailer that disagrees, or bytes after it
// is refused.
func readMoveStream(r io.Reader, h moveStreamHandler) (*moveStreamResult, error) {
	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, 1<<20)
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
	blocks := diskBlocks(res.Header.DiskSize)
	raw := make([]byte, moveMaxBatchBlocks*diskBlockSize)
	var comp []byte
	for {
		kind, js, err := readStreamRecord(br)
		if err != nil {
			return nil, err
		}
		if chain.n >= moveMaxRecords {
			return nil, fmt.Errorf("the stream has more than %d records", moveMaxRecords)
		}
		switch kind {
		case 'D':
			var d diskBatchHeader
			if err := strictUnmarshal(js, &d); err != nil {
				return nil, fmt.Errorf("disk batch: %w", err)
			}
			want, err := checkDiskBatch(d, res.Header.DiskSize, blocks)
			if err != nil {
				return nil, err
			}
			chain.add(kind, js)
			data := raw[:want]
			switch d.Codec {
			case codecRaw:
				if _, err := io.ReadFull(br, data); err != nil {
					return nil, fmt.Errorf("the stream ended in a disk batch: %w", err)
				}
			case codecLZ4:
				if int64(len(comp)) < d.Size {
					comp = make([]byte, lz4.CompressBlockBound(len(raw)))
				}
				if _, err := io.ReadFull(br, comp[:d.Size]); err != nil {
					return nil, fmt.Errorf("the stream ended in a disk batch: %w", err)
				}
				n, err := lz4.UncompressBlock(comp[:d.Size], data)
				if err != nil || int64(n) != want {
					return nil, fmt.Errorf("disk batch at block %d does not decompress to its %d bytes", d.First, want)
				}
			}
			if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != d.SHA256 {
				return nil, fmt.Errorf("disk batch at block %d (round %d) arrived corrupted", d.First, d.Round)
			}
			if err := h.disk(d, data); err != nil {
				return nil, err
			}
			res.Disk.Batches++
			res.Disk.Blocks += d.Count
			res.Disk.Bytes += want
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
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return nil, errors.New("the stream goes on after its trailer")
			}
			res.Trailer = t
			res.Bytes = cr.n
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
	case h.BlockSize != diskBlockSize:
		return fmt.Errorf("the stream's disk blocks are %d bytes, not %d", h.BlockSize, diskBlockSize)
	case h.DiskSize <= 0 || h.DiskSize > migrateMaxDiskSize:
		return fmt.Errorf("the stream says the disk is %d bytes", h.DiskSize)
	case checkDiskDigest(h.BaseSHA256) != nil:
		return fmt.Errorf("the stream names base %q", h.BaseSHA256)
	}
	return nil
}

// checkDiskBatch holds a disk batch to the disk and returns its raw length.
func checkDiskBatch(d diskBatchHeader, size, blocks int64) (int64, error) {
	if d.Count < 1 || d.Count > moveMaxBatchBlocks || d.First < 0 || d.First >= blocks || d.Count > blocks-d.First {
		return 0, fmt.Errorf("a disk batch of %d blocks at %d is outside the %d blocks of the disk", d.Count, d.First, blocks)
	}
	if d.Round < 0 {
		return 0, fmt.Errorf("a disk batch of round %d", d.Round)
	}
	want := min(d.Count*diskBlockSize, size-d.First*diskBlockSize)
	if d.RawSize != want {
		return 0, fmt.Errorf("a disk batch of %d blocks at %d says %d bytes, not %d", d.Count, d.First, d.RawSize, want)
	}
	switch d.Codec {
	case codecRaw:
		if d.Size != want {
			return 0, fmt.Errorf("a raw disk batch at %d is %d bytes, not %d", d.First, d.Size, want)
		}
	case codecLZ4:
		if d.Size <= 0 || d.Size > int64(lz4.CompressBlockBound(int(want))) {
			return 0, fmt.Errorf("a compressed disk batch at %d says %d bytes", d.First, d.Size)
		}
	default:
		return 0, fmt.Errorf("a disk batch at %d has codec %q", d.First, d.Codec)
	}
	if len(d.SHA256) != 64 {
		return 0, fmt.Errorf("a disk batch at %d has no sha256", d.First)
	}
	return want, nil
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
