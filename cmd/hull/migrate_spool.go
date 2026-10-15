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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/brig-sh/hull/pkg/store"
)

// A streamed move keeps hvi's stream in a spool on the source, which an
// abort restores the guest from and a re-run sends again. hvi fences the
// guest once its manifest is in the socket's buffer, before hull has read
// it, so a hull that dies between that and the `sealed` marker leaves a
// fenced guest whose spool may be whole, cut short, or missing. Nothing
// here trusts the marker alone: a spool without one is checked frame by
// frame against the manifest it ends with, and one that does not check out
// is taken again from hvi, which streams a fenced guest again unchanged
// (the guest has not run since), for as long as that hvi is alive.

// spoolContents is what a spooled stream holds, once every frame checked.
type spoolContents struct {
	// files are the components that are not RAM rounds, by name.
	files    map[string][]byte
	manifest []byte
}

// hviStreamManifest is what hull reads of hvi's manifest.json: the
// components before it, each with its size, sha256 and frame count.
type hviStreamManifest struct {
	Components []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Frames int64  `json:"frames"`
	} `json:"components"`
}

// spoolComponent is one component as the spool's frames add up to it.
type spoolComponent struct {
	name   string
	size   int64
	frames int64
	h      hash.Hash
}

// verifySpool reads the spooled stream in dir and checks every frame's
// sha256, that the stream ends with manifest.json and nothing after it,
// and that the manifest lists exactly the components the frames make up,
// in order, with their sizes, digests and frame counts.
func verifySpool(dir string) (*spoolContents, error) {
	f, err := os.Open(filepath.Join(dir, spoolStream))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 1<<20)
	c := &spoolContents{files: map[string][]byte{}}
	var comps []*spoolComponent
	var kept int64
	for {
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			break
		}
		if c.manifest != nil {
			return nil, errors.New("the spooled stream goes on after its manifest")
		}
		line, err := readHviLine(br, migrateMaxHeader)
		if err != nil {
			return nil, fmt.Errorf("the spooled stream ends in a frame header: %w", err)
		}
		hdr, err := parseHviComponentHeader(line)
		if err != nil {
			return nil, fmt.Errorf("the spooled stream: %w", err)
		}
		var cur *spoolComponent
		if n := len(comps); n > 0 && comps[n-1].name == hdr.Component {
			cur = comps[n-1]
		} else if hdr.Component != "manifest.json" {
			cur = &spoolComponent{name: hdr.Component, h: sha256.New()}
			comps = append(comps, cur)
		}
		frame := sha256.New()
		dst := []io.Writer{frame}
		var keep *bytesBuffer
		if !isRAMRound(hdr.Component) {
			if kept+hdr.Size > spooledStateMax {
				return nil, fmt.Errorf("the spooled stream's %s of %d bytes is too large to be a document",
					hdr.Component, hdr.Size)
			}
			kept += hdr.Size
			keep = &bytesBuffer{}
			dst = append(dst, keep)
		}
		if cur != nil {
			dst = append(dst, cur.h)
		}
		n, err := io.Copy(io.MultiWriter(dst...), io.LimitReader(br, hdr.Size))
		if err != nil {
			return nil, fmt.Errorf("the spooled stream's %s: %w", hdr.Component, err)
		}
		if n != hdr.Size {
			return nil, fmt.Errorf("the spooled stream ends in %s, at %d of its %d bytes", hdr.Component, n, hdr.Size)
		}
		if got := hex.EncodeToString(frame.Sum(nil)); got != hdr.SHA256 {
			return nil, fmt.Errorf("a frame of %s in the spooled stream has sha256 %s, its header says %s",
				hdr.Component, got, hdr.SHA256)
		}
		if cur != nil {
			cur.size += hdr.Size
			cur.frames++
		}
		if keep != nil {
			c.files[hdr.Component] = append(c.files[hdr.Component], keep.b...)
		}
		if hdr.Component == "manifest.json" {
			c.manifest = keep.b
		}
	}
	if c.manifest == nil {
		return nil, errors.New("the spooled stream has no manifest.json: it was cut short")
	}
	var man hviStreamManifest
	if err := json.Unmarshal(c.manifest, &man); err != nil {
		return nil, fmt.Errorf("the spooled manifest.json: %w", err)
	}
	if len(man.Components) != len(comps) {
		return nil, fmt.Errorf("the spooled manifest lists %d components, the stream holds %d",
			len(man.Components), len(comps))
	}
	for i, want := range man.Components {
		got := comps[i]
		sum := hex.EncodeToString(got.h.Sum(nil))
		if want.Name != got.name || want.Size != got.size || want.SHA256 != sum ||
			(want.Frames != 0 && want.Frames != got.frames) {
			return nil, fmt.Errorf("the spooled stream's %s (%d bytes in %d frames, sha256 %s) is not the "+
				"manifest's %s (%d bytes in %d frames, sha256 %s)", got.name, got.size, got.frames, sum,
				want.Name, want.Size, want.Frames, want.SHA256)
		}
	}
	return c, nil
}

// spoolIsSealed reports whether the spool in dir was sealed: its stream
// synced, its documents written beside it and the marker last.
func spoolIsSealed(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, spoolSealed))
	return err == nil
}

// sealCheckedSpool seals a spool that verifySpool checked, whose seal hull
// did not live to write: the stream is synced, the documents written beside
// it, and the marker records that hvi's reply was lost.
func sealCheckedSpool(dir string, c *spoolContents) error {
	f, err := os.OpenFile(filepath.Join(dir, spoolStream), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	for name, data := range c.files {
		if err := writeFileSynced(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	reply, err := json.Marshal(map[string]any{"v": hviControlVersion, "ok": true, "state": store.MigrationFenced,
		"manifest_sha256": sha256Hex(c.manifest),
		"note":            "hvi's reply was lost with the hull that read it; hull checked the spooled stream frame by frame"})
	if err != nil {
		return err
	}
	if err := writeFileSynced(filepath.Join(dir, spoolSealed), append(reply, '\n')); err != nil {
		return err
	}
	return syncDir(dir)
}

// openSpool returns the sealed spool in dir, sealing it first when it was
// never sealed but checks out whole. It refuses a spool that does not.
func openSpool(dir string) error {
	if spoolIsSealed(dir) {
		return nil
	}
	c, err := verifySpool(dir)
	if err != nil {
		return fmt.Errorf("the streamed checkpoint %s was never sealed, and it is not whole: %w", dir, err)
	}
	return sealCheckedSpool(dir, c)
}

// lostStreamError is a fenced guest with no whole copy of its stream on
// this host. While its hvi is alive the guest is still there, and only
// there; once that hvi is gone, so is the guest, unless the destination
// verified the stream.
type lostStreamError struct {
	dir      string
	why      error
	hviAlive bool
	pid      int
	again    error
}

func (e *lostStreamError) Error() string {
	if e.hviAlive {
		return fmt.Sprintf("the guest is fenced and no whole copy of its stream is in %s (%v); hvi (pid %d) "+
			"still holds the guest, but streaming it again failed: %v. Do not stop that hvi: it is the only "+
			"copy of the guest. Running the move again retries", e.dir, e.why, e.pid, e.again)
	}
	return fmt.Sprintf("the guest is fenced, no whole copy of its stream is in %s (%v), and the hvi that "+
		"held it is gone: this host cannot bring the guest back. If the destination verified the stream "+
		"(`hull migrate-status`), running the move again commits it there", e.dir, e.why)
}

// ensureStreamSpool makes sure the spool dir holds a sealed, whole copy of
// the stream hvi fenced the instance's guest with for this move, and
// returns its manifest. A spool that was never sealed is checked and
// sealed; one that is not whole is taken again from hvi when hvi still
// holds the fenced guest.
func (m *moveSource) ensureStreamSpool(state *store.InstanceState, dir string) ([]byte, error) {
	if spoolIsSealed(dir) {
		return os.ReadFile(filepath.Join(dir, "manifest.json"))
	}
	c, verr := verifySpool(dir)
	if verr == nil {
		m.logf("The spooled stream %s is whole but was never sealed (hull stopped before it could); sealing it", dir)
		if err := sealCheckedSpool(dir, c); err != nil {
			return nil, fmt.Errorf("seal the spooled stream %s: %w", dir, err)
		}
		return c.manifest, nil
	}
	lost := &lostStreamError{dir: dir, why: verr, pid: state.PID}
	if hviVMState(state) != store.MigrationFenced {
		return nil, lost
	}
	lost.hviAlive = true
	m.logf("The spooled stream %s is not whole (%v); hvi still holds the fenced guest and streams it "+
		"again, unchanged, into the spool", dir, verr)
	manifest, err := m.restreamFenced(state, dir)
	if err != nil {
		lost.again = err
		return nil, lost
	}
	return manifest, nil
}

// restreamFenced has hvi stream the fenced guest again into a fresh spool
// in dir, and seals it. The guest has not run since hvi fenced it, so this
// is the same machine as the stream that fenced it.
func (m *moveSource) restreamFenced(state *store.InstanceState, dir string) ([]byte, error) {
	sp, err := newSpool(dir)
	if err != nil {
		return nil, err
	}
	ss := &streamSender{m: m, state: state, sw: newMoveStreamWriter(io.Discard), link: &softLink{w: io.Discard},
		stats: &migrateStreamStats{Mode: "restream"}, spoolDir: dir}
	reply, files, err := ss.relayHvi(sp)
	if err != nil {
		sp.close()
		return nil, err
	}
	if err := sp.seal(files, reply); err != nil {
		sp.close()
		return nil, err
	}
	sp.close()
	return ss.manifest, nil
}
