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
	"os"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The move stream is what `hull migrate` writes into ssh and `hull
// migrate-receive` reads on the other end: the bundle's files, then the
// disk, then a trailer with every file's sha256.
//
// It is hull's own, not tar. Go's archive/tar cannot write a sparse entry
// (its writer drops the GNU sparse records), and the disk is a mostly empty
// image of several GiB: written dense, a move would carry every hole as
// zeros. GNU tar and bsdtar each do sparse files, but not the same way on
// both OSes, and hull would be parsing their output to learn what arrived.
// So the format is a few framed records (docs/migration.md, "The stream"):
//
//	"HULLMIG1"
//	'H' u32 len, JSON {txid, manifestSha256}
//	'F' u32 len, JSON {name, size}, then size bytes       (a bundle file)
//	'S' u32 len, JSON {name, size}, then extents:          (the disk)
//	    u64 offset, u64 length, length bytes ... u64 0, u64 0
//	'T' u32 len, JSON {files: [{name, size, sha256}]}
//
// Integers are big-endian. The receiver checks the trailer against what it
// hashed itself and refuses a stream that ends without one, so a cut
// connection never passes for a short file.
const migrateStreamMagic = "HULLMIG1"

// Limits the receiver holds a stream to before it allocates or writes
// anything for a record.
const (
	migrateMaxHeader   = 64 << 10
	migrateMaxFiles    = 32
	migrateMaxFileSize = 64 << 30 // hvi's own bound on a bundle component
	migrateMaxDiskSize = 4 << 40
	// migrateChunk is the zero-detection grain: the sender leaves out a
	// chunk that is all zeros and the receiver leaves one as a hole.
	migrateChunk = 64 << 10
)

// migrateStreamName is a name a stream may carry: "disk", or a bare file
// name below "bundle/". Anything else, a path above all, is refused.
var migrateStreamName = regexp.MustCompile(`^(disk|bundle/[A-Za-z0-9_-][A-Za-z0-9._-]{0,127})$`)

// streamHeader is the 'H' record.
type streamHeader struct {
	TxID           string `json:"txid"`
	ManifestSHA256 string `json:"manifestSha256"`
}

// streamEntry is the JSON of an 'F' or 'S' record.
type streamEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// streamSum is one file's line in the trailer, and what a side computed.
type streamSum struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// DataBytes is, for the disk, the bytes its extents carried.
	DataBytes int64 `json:"dataBytes,omitempty"`
}

// streamTrailer is the 'T' record.
type streamTrailer struct {
	Files []streamSum `json:"files"`
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// migrateStreamWriter writes one stream.
type migrateStreamWriter struct {
	cw   *countingWriter
	bw   *bufio.Writer
	sums []streamSum
}

func newMigrateStreamWriter(w io.Writer) *migrateStreamWriter {
	cw := &countingWriter{w: w}
	return &migrateStreamWriter{cw: cw, bw: bufio.NewWriterSize(cw, 1<<20)}
}

// written is the bytes handed to the underlying writer so far.
func (sw *migrateStreamWriter) written() int64 { return sw.cw.n + int64(sw.bw.Buffered()) }

func (sw *migrateStreamWriter) record(kind byte, v any) error {
	js, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [5]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(js)))
	if _, err := sw.bw.Write(hdr[:]); err != nil {
		return err
	}
	_, err = sw.bw.Write(js)
	return err
}

// header writes the magic and the 'H' record.
func (sw *migrateStreamWriter) header(h streamHeader) error {
	if _, err := sw.bw.WriteString(migrateStreamMagic); err != nil {
		return err
	}
	return sw.record('H', h)
}

// file writes the regular file at path as name. The size is taken when the
// file is opened, and a file that then yields a different number of bytes
// fails the stream rather than sending a record that lies about its length.
func (sw *migrateStreamWriter) file(name, path string) (streamSum, error) {
	f, err := os.Open(path)
	if err != nil {
		return streamSum{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return streamSum{}, err
	}
	if !info.Mode().IsRegular() {
		return streamSum{}, fmt.Errorf("%s is not a regular file", path)
	}
	if err := sw.record('F', streamEntry{Name: name, Size: info.Size()}); err != nil {
		return streamSum{}, err
	}
	h := sha256.New()
	n, err := io.Copy(sw.bw, io.TeeReader(io.LimitReader(f, info.Size()), h))
	if err != nil {
		return streamSum{}, err
	}
	if n != info.Size() {
		return streamSum{}, fmt.Errorf("%s shrank from %d to %d bytes while it was sent", path, info.Size(), n)
	}
	sum := streamSum{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	sw.sums = append(sw.sums, sum)
	return sum, nil
}

// sparse writes the file at path as name, carrying only its data: the
// extents SEEK_DATA finds, less every 64 KiB chunk in them that is all
// zeros. The sha256 is over the extent records as sent (offset, length and
// bytes, then the terminator), which both ends compute without reading the
// holes; docs/migration.md says why it is not the image's plain sha256.
func (sw *migrateStreamWriter) sparse(name, path string) (streamSum, error) {
	f, err := os.Open(path)
	if err != nil {
		return streamSum{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return streamSum{}, err
	}
	if !info.Mode().IsRegular() {
		return streamSum{}, fmt.Errorf("%s is not a regular file", path)
	}
	size := info.Size()
	extents, err := dataExtents(f, size)
	if err != nil {
		return streamSum{}, err
	}
	if err := sw.record('S', streamEntry{Name: name, Size: size}); err != nil {
		return streamSum{}, err
	}
	h := sha256.New()
	var data int64
	buf := make([]byte, 1<<20)
	for _, ext := range extents {
		for off := ext.off; off < ext.end; {
			n := min(int64(len(buf)), ext.end-off)
			if _, err := f.ReadAt(buf[:n], off); err != nil {
				return streamSum{}, fmt.Errorf("read %s at %d: %w", path, off, err)
			}
			for c := int64(0); c < n; c += migrateChunk {
				chunk := buf[c:min(c+migrateChunk, n)]
				if isZero(chunk) {
					continue
				}
				if err := sw.extent(h, off+c, chunk); err != nil {
					return streamSum{}, err
				}
				data += int64(len(chunk))
			}
			off += n
		}
	}
	if err := sw.extent(h, 0, nil); err != nil {
		return streamSum{}, err
	}
	// A disk that changed size while it was read was not the fenced one.
	if after, err := f.Stat(); err != nil || after.Size() != size {
		return streamSum{}, fmt.Errorf("%s changed size while it was sent", path)
	}
	sum := streamSum{Name: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), DataBytes: data}
	sw.sums = append(sw.sums, sum)
	return sum, nil
}

// extent writes one extent record, data nil being the terminator.
func (sw *migrateStreamWriter) extent(h hash.Hash, off int64, data []byte) error {
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[:8], uint64(off))
	binary.BigEndian.PutUint64(hdr[8:], uint64(len(data)))
	h.Write(hdr[:])
	h.Write(data)
	if _, err := sw.bw.Write(hdr[:]); err != nil {
		return err
	}
	_, err := sw.bw.Write(data)
	return err
}

// finish writes the trailer and flushes.
func (sw *migrateStreamWriter) finish() error {
	if err := sw.record('T', streamTrailer{Files: sw.sums}); err != nil {
		return err
	}
	return sw.bw.Flush()
}

// isZero reports whether b is all zero bytes.
func isZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.NativeEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// extent is a byte range [off, end) of a file.
type extent struct{ off, end int64 }

// dataExtents lists the ranges of f that hold data, by SEEK_DATA and
// SEEK_HOLE (APFS and ext4 both answer them). A filesystem that does not
// know them gets the whole file as one extent, and the zero-chunk test
// still keeps the holes out of the stream; it just has to read them.
func dataExtents(f *os.File, size int64) ([]extent, error) {
	fd := int(f.Fd())
	var out []extent
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				break // only a hole is left
			}
			if errors.Is(err, syscall.EINVAL) && off == 0 {
				return []extent{{0, size}}, nil
			}
			return nil, fmt.Errorf("find data in %s: %w", f.Name(), err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("find a hole in %s: %w", f.Name(), err)
		}
		end = min(end, size)
		if end <= start {
			break
		}
		out = append(out, extent{start, end})
		off = end
	}
	return out, nil
}

// streamSink tells the reader where each named file goes. It returns an open
// file the reader writes into and closes.
type streamSink func(name string, size int64) (*os.File, error)

// migrateStreamResult is what a reader received.
type migrateStreamResult struct {
	Header streamHeader
	Files  []streamSum
	// Bytes is the whole stream's length, magic and trailer included.
	Bytes int64
}

// countingReader counts what passes through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readMigrateStream reads a stream from r, writing each file through sink,
// and checks the trailer against what it computed. Nothing in the stream is
// trusted: names, sizes and extents are checked against the limits above
// before anything is written, and a stream with no trailer, a trailer that
// disagrees, or bytes after it is refused.
func readMigrateStream(r io.Reader, sink streamSink) (*migrateStreamResult, error) {
	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, 1<<20)
	magic := make([]byte, len(migrateStreamMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, fmt.Errorf("the stream ended before it began: %w", err)
	}
	if string(magic) != migrateStreamMagic {
		return nil, fmt.Errorf("the stream does not start with %s; is the other side a hull of this version?", migrateStreamMagic)
	}
	res := &migrateStreamResult{}
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
	seen := map[string]bool{}
	for {
		kind, js, err := readStreamRecord(br)
		if err != nil {
			return nil, err
		}
		switch kind {
		case 'F', 'S':
			var e streamEntry
			if err := strictUnmarshal(js, &e); err != nil {
				return nil, fmt.Errorf("stream entry: %w", err)
			}
			if err := checkStreamEntry(e, kind, seen); err != nil {
				return nil, err
			}
			out, err := sink(e.Name, e.Size)
			if err != nil {
				return nil, err
			}
			var sum streamSum
			if kind == 'F' {
				sum, err = receiveFile(br, out, e)
			} else {
				sum, err = receiveSparse(br, out, e)
			}
			if cerr := out.Close(); err == nil && cerr != nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
			res.Files = append(res.Files, sum)
		case 'T':
			var t streamTrailer
			if err := strictUnmarshal(js, &t); err != nil {
				return nil, fmt.Errorf("stream trailer: %w", err)
			}
			if err := compareTrailer(t.Files, res.Files); err != nil {
				return nil, err
			}
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return nil, errors.New("the stream goes on after its trailer")
			}
			res.Bytes = cr.n
			return res, nil
		default:
			return nil, fmt.Errorf("the stream has a record of unknown kind %q", kind)
		}
	}
}

// readStreamRecord reads one record's kind and JSON.
func readStreamRecord(br *bufio.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("the stream ended without its trailer: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > migrateMaxHeader {
		return 0, nil, fmt.Errorf("a stream record header of %d bytes is over the %d limit", n, migrateMaxHeader)
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(br, js); err != nil {
		return 0, nil, fmt.Errorf("the stream ended inside a record header: %w", err)
	}
	return hdr[0], js, nil
}

// strictUnmarshal decodes js into v, refusing fields v does not have.
func strictUnmarshal(js []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// checkStreamEntry holds one entry to the name and size limits.
func checkStreamEntry(e streamEntry, kind byte, seen map[string]bool) error {
	if !migrateStreamName.MatchString(e.Name) || strings.Contains(e.Name, "..") {
		return fmt.Errorf("the stream names a file %q, which is not a bundle file or the disk", e.Name)
	}
	if seen[e.Name] {
		return fmt.Errorf("the stream carries %s twice", e.Name)
	}
	if len(seen) >= migrateMaxFiles {
		return fmt.Errorf("the stream carries more than %d files", migrateMaxFiles)
	}
	seen[e.Name] = true
	limit := int64(migrateMaxFileSize)
	if e.Name == "disk" {
		if kind != 'S' {
			return errors.New("the stream sends the disk as a plain file")
		}
		limit = migrateMaxDiskSize
	} else if kind != 'F' {
		return fmt.Errorf("the stream sends %s as a disk", e.Name)
	}
	if e.Size < 0 || e.Size > limit {
		return fmt.Errorf("the stream says %s is %d bytes, outside 0 to %d", e.Name, e.Size, limit)
	}
	return nil
}

// receiveFile copies e.Size bytes into out, hashing them.
func receiveFile(br *bufio.Reader, out *os.File, e streamEntry) (streamSum, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(br, e.Size))
	if err != nil {
		return streamSum{}, fmt.Errorf("receive %s: %w", e.Name, err)
	}
	if n != e.Size {
		return streamSum{}, fmt.Errorf("the stream ended %d bytes into %s, which is %d", n, e.Name, e.Size)
	}
	if err := out.Sync(); err != nil {
		return streamSum{}, err
	}
	return streamSum{Name: e.Name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// receiveSparse writes the extents of e into out, which it first sizes to
// e.Size so that everything no extent covers is a hole. Extents must come in
// order and stay inside the file. A zero chunk inside an extent is skipped
// too, so the copy stays sparse even if the sender's filesystem reported
// zeros as data.
func receiveSparse(br *bufio.Reader, out *os.File, e streamEntry) (streamSum, error) {
	if err := out.Truncate(e.Size); err != nil {
		return streamSum{}, fmt.Errorf("size %s: %w", e.Name, err)
	}
	h := sha256.New()
	var data, next int64
	buf := make([]byte, 1<<20)
	for {
		var hdr [16]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return streamSum{}, fmt.Errorf("the stream ended inside %s: %w", e.Name, err)
		}
		h.Write(hdr[:])
		off := int64(binary.BigEndian.Uint64(hdr[:8]))
		length := int64(binary.BigEndian.Uint64(hdr[8:]))
		if length == 0 {
			if off != 0 {
				return streamSum{}, fmt.Errorf("%s: a malformed end of extents", e.Name)
			}
			break
		}
		if off < next || off < 0 || length < 0 || length > e.Size || off > e.Size-length {
			return streamSum{}, fmt.Errorf("%s: an extent of %d bytes at %d is out of order or outside its %d bytes",
				e.Name, length, off, e.Size)
		}
		for done := int64(0); done < length; {
			n := min(int64(len(buf)), length-done)
			if _, err := io.ReadFull(br, buf[:n]); err != nil {
				return streamSum{}, fmt.Errorf("the stream ended inside %s: %w", e.Name, err)
			}
			h.Write(buf[:n])
			for c := int64(0); c < n; c += migrateChunk {
				chunk := buf[c:min(c+migrateChunk, n)]
				if isZero(chunk) {
					continue
				}
				if _, err := out.WriteAt(chunk, off+done+c); err != nil {
					return streamSum{}, fmt.Errorf("write %s: %w", e.Name, err)
				}
			}
			done += n
		}
		data += length
		next = off + length
	}
	if err := out.Sync(); err != nil {
		return streamSum{}, err
	}
	return streamSum{Name: e.Name, Size: e.Size, SHA256: hex.EncodeToString(h.Sum(nil)), DataBytes: data}, nil
}

// compareTrailer checks the sender's sums against the receiver's.
func compareTrailer(sent, got []streamSum) error {
	if len(sent) != len(got) {
		return fmt.Errorf("the trailer lists %d files, the stream carried %d", len(sent), len(got))
	}
	for i := range sent {
		s, g := sent[i], got[i]
		if s.Name != g.Name || s.Size != g.Size || s.SHA256 != g.SHA256 {
			return fmt.Errorf("%s: the sender sent %d bytes with sha256 %s, %d bytes with sha256 %s arrived",
				g.Name, s.Size, s.SHA256, g.Size, g.SHA256)
		}
	}
	return nil
}
