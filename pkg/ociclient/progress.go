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

package ociclient

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/brig-sh/hull/internal/pullprogress"
)

// Progress reports how far a pull has got. The zero value is unusable; call
// newProgress. A nil *Progress is safe and reports nothing, so callers that
// do not want output (tests, library use) can pass nil.
type Progress struct {
	line       *pullprogress.Line
	mu         sync.Mutex
	layers     int
	totalBytes int64
	doneBytes  int64 // bytes from layers already finished
	curBytes   int64 // bytes extracted from the layer in flight
	curLayer   int
}

// newProgress returns a reporter writing to stderr, or nil when quiet.
func newProgress(quiet bool, layers int, totalBytes int64) *Progress {
	if quiet {
		return nil
	}
	return &Progress{
		line:       pullprogress.New(os.Stderr),
		layers:     layers,
		totalBytes: totalBytes,
	}
}

func (p *Progress) startLayer(index int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.curLayer = index
	p.doneBytes += p.curBytes
	p.curBytes = 0
	p.draw(true)
}

func (p *Progress) add(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.curBytes += n
	p.draw(false)
}

// done clears the in-place line so following output starts clean.
func (p *Progress) done() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.line.Done(fmt.Sprintf("pull: %d/%d layers, %s extracted",
		p.layers, p.layers, pullprogress.HumanBytes(p.doneBytes+p.curBytes)))
}

// draw must be called with the lock held. force bypasses the rate limit so
// layer transitions are always visible.
func (p *Progress) draw(force bool) {
	extracted := pullprogress.HumanBytes(p.doneBytes + p.curBytes)
	// totalBytes is the compressed size, so it is a lower bound on what gets
	// written. Reporting it as a percentage would routinely exceed 100%, so
	// show it as the download size rather than pretending to a ratio.
	p.line.Draw(fmt.Sprintf("pull: layer %d/%d, %s extracted (image %s compressed)",
		p.curLayer, p.layers, extracted, pullprogress.HumanBytes(p.totalBytes)), force)
}

// progressReader counts bytes as they are read.
type progressReader struct {
	r io.Reader
	p *Progress
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.p.add(int64(n))
	}
	return n, err
}

// retryLayer rewinds the counter for the layer being re-fetched, so the total
// reflects bytes landed rather than bytes attempted.
func (p *Progress) retryLayer() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.curBytes = 0
	p.draw(true)
}
