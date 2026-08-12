// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"time"

	"golang.org/x/term"
)

// Pulling a multi-gigabyte image can take minutes during which nothing is
// printed, which is indistinguishable from a hang. This reports progress the
// way docker pull does: a line that refreshes in place on a terminal, and
// periodic one-line updates when the output is a log file or CI.

const (
	ttyRefresh = 100 * time.Millisecond
	logRefresh = 5 * time.Second
)

// Progress reports how far a pull has got. The zero value is unusable; call
// newProgress. A nil *Progress is safe and reports nothing, so callers that
// do not want output (tests, library use) can pass nil.
type Progress struct {
	w          io.Writer
	tty        bool
	mu         sync.Mutex
	layers     int
	totalBytes int64
	doneBytes  int64 // bytes from layers already finished
	curBytes   int64 // bytes extracted from the layer in flight
	curLayer   int
	lastDraw   time.Time
	drawn      bool
}

// newProgress returns a reporter writing to stderr, or nil when quiet.
func newProgress(quiet bool, layers int, totalBytes int64) *Progress {
	if quiet {
		return nil
	}
	return &Progress{
		w:          os.Stderr,
		tty:        term.IsTerminal(int(os.Stderr.Fd())),
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
	if !p.drawn {
		return
	}
	if p.tty {
		_, _ = fmt.Fprint(p.w, "\r\033[K")
	}
	_, _ = fmt.Fprintf(p.w, "pull: %d/%d layers, %s extracted\n",
		p.layers, p.layers, humanBytes(p.doneBytes+p.curBytes))
}

// draw must be called with the lock held. force bypasses the rate limit so
// layer transitions are always visible.
func (p *Progress) draw(force bool) {
	interval := logRefresh
	if p.tty {
		interval = ttyRefresh
	}
	if !force && time.Since(p.lastDraw) < interval {
		return
	}
	p.lastDraw = time.Now()
	p.drawn = true

	extracted := humanBytes(p.doneBytes + p.curBytes)
	// totalBytes is the compressed size, so it is a lower bound on what gets
	// written. Reporting it as a percentage would routinely exceed 100%, so
	// show it as the download size rather than pretending to a ratio.
	line := fmt.Sprintf("pull: layer %d/%d, %s extracted (image %s compressed)",
		p.curLayer, p.layers, extracted, humanBytes(p.totalBytes))
	if p.tty {
		_, _ = fmt.Fprintf(p.w, "\r\033[K%s", line)
		return
	}
	_, _ = fmt.Fprintln(p.w, line)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < 3; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
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
