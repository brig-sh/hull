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
	"fmt"
	"io"
	"strings"
	"testing"
)

// hviFrame is one component as hvi frames it, written here by hand rather
// than with hull's own types, so the tests check hull against the format
// hvi documents and not against itself.
func hviFrame(name string, body []byte) []byte {
	sum := sha256.Sum256(body)
	line := fmt.Sprintf(`{"component":%q,"size":%d,"sha256":"%s"}`+"\n", name, len(body), hex.EncodeToString(sum[:]))
	return append([]byte(line), body...)
}

// recordingHandler keeps what a reader hands it.
type recordingHandler struct {
	gotHeader  moveStreamHeader
	blocks     map[int64][]byte
	components []string
	bodies     map[string][]byte
	verified   []string
	failAt     string
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{blocks: map[int64][]byte{}, bodies: map[string][]byte{}}
}

func (h *recordingHandler) header(m moveStreamHeader) error { h.gotHeader = m; return nil }

func (h *recordingHandler) disk(d diskBatchHeader, raw []byte) error {
	for i := int64(0); i < d.Count; i++ {
		lo := i * diskBlockSize
		h.blocks[d.First+i] = bytes.Clone(raw[lo:min(lo+diskBlockSize, int64(len(raw)))])
	}
	return nil
}

func (h *recordingHandler) component(_ []byte, c hviComponentHeader, body io.Reader) error {
	if c.Component == h.failAt {
		return fmt.Errorf("the handler refuses %s", c.Component)
	}
	b, err := io.ReadAll(body)
	h.components = append(h.components, c.Component)
	h.bodies[c.Component] = b
	return err
}

func (h *recordingHandler) componentVerified(c hviComponentHeader) error {
	h.verified = append(h.verified, c.Component)
	return nil
}

// testMoveStream builds a stream: two disk batches (one that compresses,
// one that does not, the second ending at a short last block), hvi's
// components, and the trailer.
func testMoveStream(t *testing.T) ([]byte, map[int64][]byte, map[string][]byte) {
	t.Helper()
	const size = 40*diskBlockSize + 1000
	var buf bytes.Buffer
	sw := newMoveStreamWriter(&buf)
	if err := sw.header(moveStreamHeader{TxID: strings.Repeat("ab", 16), Name: "vm1", Epoch: 2,
		BaseSHA256: strings.Repeat("cd", 32), DiskSize: size, BlockSize: diskBlockSize}); err != nil {
		t.Fatal(err)
	}
	disk := map[int64][]byte{}
	a := bytes.Repeat([]byte("abcd"), 2*diskBlockSize/4)
	disk[3], disk[4] = a[:diskBlockSize], a[diskBlockSize:]
	if _, err := sw.disk(0, false, 3, 2, a); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, diskBlockSize+1000)
	for i := range noise {
		noise[i] = byte(i*7919 + i>>5)
	}
	disk[39], disk[40] = noise[:diskBlockSize], noise[diskBlockSize:]
	if _, err := sw.disk(1, true, 39, 2, noise); err != nil {
		t.Fatal(err)
	}
	comps := map[string][]byte{
		"profile.json":  []byte(`{"profile_version":1,"checkpoint_format":2}`),
		"ram-round-0":   bytes.Repeat([]byte{7}, 100000),
		"cpu.json":      []byte(`{"version":1}`),
		"manifest.json": []byte(`{"checkpoint_format":2}`),
	}
	for _, name := range []string{"profile.json", "ram-round-0", "cpu.json", "manifest.json"} {
		frame := hviFrame(name, comps[name])
		br := bufio.NewReader(bytes.NewReader(frame))
		line, _ := br.ReadBytes('\n')
		line = bytes.TrimSuffix(line, []byte("\n"))
		hdr, err := parseHviComponentHeader(line)
		if err != nil {
			t.Fatal(err)
		}
		if err := sw.component(line, hdr, br, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.trailer([]byte(`{"v":1,"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), disk, comps
}

// The demultiplexer hands back exactly what the multiplexer was given: the
// disk blocks by index and hvi's components in order, each checked.
func TestMoveStreamRoundTrip(t *testing.T) {
	stream, disk, comps := testMoveStream(t)
	h := newRecordingHandler()
	res, err := readMoveStream(bytes.NewReader(stream), h)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != int64(len(stream)) || res.Disk.Blocks != 4 || res.Disk.Batches != 2 || len(res.Components) != 4 {
		t.Errorf("result %+v", res)
	}
	if h.gotHeader.Name != "vm1" || h.gotHeader.Epoch != 2 {
		t.Errorf("header %+v", h.gotHeader)
	}
	for index, want := range disk {
		if !bytes.Equal(h.blocks[index], want) {
			t.Errorf("disk block %d differs", index)
		}
	}
	if strings.Join(h.components, ",") != "profile.json,ram-round-0,cpu.json,manifest.json" ||
		strings.Join(h.verified, ",") != strings.Join(h.components, ",") {
		t.Errorf("components %v verified %v", h.components, h.verified)
	}
	for name, want := range comps {
		if !bytes.Equal(h.bodies[name], want) {
			t.Errorf("%s differs", name)
		}
	}
	if string(res.Trailer.HviReply) != `{"v":1,"ok":true}` {
		t.Errorf("hvi reply %s", res.Trailer.HviReply)
	}
}

// moveStreamRecords returns the offset of every record in stream, after the
// magic: kind, header start, header end, payload end.
func moveStreamRecords(t *testing.T, stream []byte) [][4]int {
	t.Helper()
	var out [][4]int
	off := len(moveStreamMagic)
	for off < len(stream) {
		kind := int(stream[off])
		n := int(binary.BigEndian.Uint32(stream[off+1:]))
		hdrStart, hdrEnd := off+5, off+5+n
		end := hdrEnd
		switch kind {
		case 'D':
			var d diskBatchHeader
			if err := strictUnmarshal(stream[hdrStart:hdrEnd], &d); err != nil {
				t.Fatal(err)
			}
			end += int(d.Size)
		case 'C':
			c, err := parseHviComponentHeader(stream[hdrStart:hdrEnd])
			if err != nil {
				t.Fatal(err)
			}
			end += int(c.Size)
		}
		out = append(out, [4]int{kind, hdrStart, hdrEnd, end})
		off = end
	}
	return out
}

// A stream that was damaged, cut short or rearranged on the way is refused,
// and the refusal names what is wrong; nothing after the damage is handed
// on.
func TestMoveStreamRefusesDamage(t *testing.T) {
	stream, _, _ := testMoveStream(t)
	recs := moveStreamRecords(t, stream)
	find := func(kind byte, nth int) [4]int {
		for _, r := range recs {
			if r[0] == int(kind) {
				if nth == 0 {
					return r
				}
				nth--
			}
		}
		t.Fatalf("no record %q", kind)
		return [4]int{}
	}
	ram := find('C', 1)
	disk := find('D', 1)
	cases := []struct {
		name string
		mut  func([]byte) []byte
		want string
	}{
		{"a corrupted RAM round", func(b []byte) []byte { b[ram[2]+500] ^= 1; return b }, "ram-round-0 arrived corrupted"},
		{"a corrupted disk batch", func(b []byte) []byte { b[disk[3]-10] ^= 1; return b }, "disk batch"},
		{"a corrupted header", func(b []byte) []byte {
			h := find('C', 2)
			i := bytes.Index(b[h[1]:h[2]], []byte(`"cpu.json"`))
			b[h[1]+i+1] = 'x' // "xpu.json": a component the trailer does not know
			return b
		}, "chain"},
		{"no trailer", func(b []byte) []byte { return b[:find('T', 0)[1]-5] }, "ended without its trailer"},
		{"cut inside a RAM round", func(b []byte) []byte { return b[:ram[2]+100] }, "ram-round-0"},
		{"cut inside a disk batch", func(b []byte) []byte { return b[:disk[2]+3] }, "ended in a disk batch"},
		{"cut inside a header", func(b []byte) []byte { return b[:ram[1]+5] }, "ended inside a record header"},
		{"a record dropped", func(b []byte) []byte {
			c := find('C', 2)
			return append(b[:c[1]-5:c[1]-5], b[c[3]:]...)
		}, "trailer counts"},
		{"bytes after the trailer", func(b []byte) []byte { return append(b, 0) }, "goes on after its trailer"},
		{"another magic", func(b []byte) []byte { copy(b, "HULLMIG1"); return b }, "does not start with"},
	}
	for _, tc := range cases {
		h := newRecordingHandler()
		_, err := readMoveStream(bytes.NewReader(tc.mut(bytes.Clone(stream))), h)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	// A component the handler refuses ends the stream there.
	h := newRecordingHandler()
	h.failAt = "cpu.json"
	if _, err := readMoveStream(bytes.NewReader(stream), h); err == nil || !strings.Contains(err.Error(), "refuses cpu.json") {
		t.Errorf("handler refusal: %v", err)
	}
	if len(h.components) != 2 {
		t.Errorf("components after the refusal: %v", h.components)
	}
}

// The writer refuses to relay a component whose bytes do not match the
// sha256 hvi gave it: a source never sends what hvi did not seal.
func TestMoveStreamWriterChecksHvi(t *testing.T) {
	sw := newMoveStreamWriter(io.Discard)
	frame := hviFrame("ram-round-0", []byte("the guest's memory"))
	br := bufio.NewReader(bytes.NewReader(frame))
	line, _ := br.ReadBytes('\n')
	line = bytes.TrimSuffix(line, []byte("\n"))
	hdr, err := parseHviComponentHeader(line)
	if err != nil {
		t.Fatal(err)
	}
	damaged := bytes.NewReader([]byte("the guest's memorx"))
	if err := sw.component(line, hdr, damaged, nil); err == nil || !strings.Contains(err.Error(), "ram-round-0 has sha256") {
		t.Errorf("a damaged component was relayed: %v", err)
	}
	short := bytes.NewReader([]byte("the guest"))
	if err := sw.component(line, hdr, short, nil); err == nil || !strings.Contains(err.Error(), "ended at") {
		t.Errorf("a short component was relayed: %v", err)
	}
}

// Limits hold before anything is allocated: a disk batch outside the disk,
// a component of a name hvi would not use, a header that is not a move's.
func TestMoveStreamLimits(t *testing.T) {
	const size = 4 * diskBlockSize
	for _, d := range []diskBatchHeader{
		{First: 4, Count: 1, Codec: codecRaw, RawSize: diskBlockSize, Size: diskBlockSize, SHA256: strings.Repeat("0", 64)},
		{First: 3, Count: 2, Codec: codecRaw, RawSize: 2 * diskBlockSize, Size: 2 * diskBlockSize, SHA256: strings.Repeat("0", 64)},
		{First: 0, Count: 1, Codec: codecRaw, RawSize: 10, Size: 10, SHA256: strings.Repeat("0", 64)},
		{First: 0, Count: 1, Codec: "zstd", RawSize: diskBlockSize, Size: 10, SHA256: strings.Repeat("0", 64)},
		{First: 0, Count: 1, Codec: codecLZ4, RawSize: diskBlockSize, Size: 1 << 30, SHA256: strings.Repeat("0", 64)},
		{First: 0, Count: moveMaxBatchBlocks + 1, Codec: codecRaw, RawSize: 0, Size: 0},
		{Round: -1, First: 0, Count: 1, Codec: codecRaw, RawSize: diskBlockSize, Size: diskBlockSize, SHA256: strings.Repeat("0", 64)},
	} {
		if _, err := checkDiskBatch(d, size, diskBlocks(size)); err == nil {
			t.Errorf("batch %+v passed", d)
		}
	}
	for _, line := range []string{
		`{"component":"../ram","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}`,
		`{"component":"ram","size":-1,"sha256":"` + strings.Repeat("0", 64) + `"}`,
		`{"component":"ram","size":1}`,
		`{"component":"Ram","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}`,
		`{"component":"ram","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}{}`,
	} {
		if _, err := parseHviComponentHeader([]byte(line)); err == nil {
			t.Errorf("component header %s passed", line)
		}
	}
	for _, h := range []moveStreamHeader{
		{TxID: "x", BaseSHA256: strings.Repeat("a", 64), DiskSize: 1, BlockSize: diskBlockSize},
		{TxID: strings.Repeat("a", 32), BaseSHA256: strings.Repeat("a", 64), DiskSize: 1, BlockSize: 4096},
		{TxID: strings.Repeat("a", 32), BaseSHA256: strings.Repeat("a", 64), DiskSize: 0, BlockSize: diskBlockSize},
		{TxID: strings.Repeat("a", 32), BaseSHA256: "nope", DiskSize: 1, BlockSize: diskBlockSize},
	} {
		if err := checkMoveHeader(h); err == nil {
			t.Errorf("header %+v passed", h)
		}
	}
}
