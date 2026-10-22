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

// Package pullprogress draws the progress line of an image pull.
//
// Pulling a multi-gigabyte image can take minutes during which nothing is
// printed, which is indistinguishable from a hang. A pull reports progress the
// way docker pull does: a line that refreshes in place on a terminal, and
// periodic one-line updates when the output is a log file or CI.
//
// The package only draws. Each pull counts its own progress and writes the
// text: the macOS pull counts bytes as hull extracts them, and the Linux pull
// counts what containerd has downloaded.
package pullprogress

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

const (
	ttyRefresh = 100 * time.Millisecond
	logRefresh = 5 * time.Second
)

// Line is a progress line on w. A nil *Line is safe and draws nothing, so
// callers that do not want output (tests, library use) can pass nil.
//
// A Line is not safe for concurrent use; a caller drawing from more than one
// goroutine serializes its calls.
type Line struct {
	w        io.Writer
	tty      bool
	lastDraw time.Time
	drawn    bool
}

// New returns a Line drawing to w, or nil for a nil w. The line refreshes in
// place when w is a terminal.
func New(w io.Writer) *Line {
	if w == nil {
		return nil
	}
	l := &Line{w: w}
	f, ok := w.(*os.File)
	if ok {
		l.tty = term.IsTerminal(int(f.Fd()))
	}
	return l
}

// Interval is how often the line is redrawn at most: often on a terminal,
// where the line is rewritten in place, and rarely in a log, where every
// draw adds a line.
func (l *Line) Interval() time.Duration {
	if l != nil && l.tty {
		return ttyRefresh
	}
	return logRefresh
}

// Draw shows text as the current progress. It does nothing if the line was
// drawn less than Interval ago, unless force is set, so a caller can mark a
// step such as a layer change that must always be visible.
func (l *Line) Draw(text string, force bool) {
	if l == nil {
		return
	}
	if !force && time.Since(l.lastDraw) < l.Interval() {
		return
	}
	l.lastDraw = time.Now()
	l.drawn = true
	if l.tty {
		_, _ = fmt.Fprintf(l.w, "\r\033[K%s", text)
		return
	}
	_, _ = fmt.Fprintln(l.w, text)
}

// Done clears the in-place line and prints text as the final state, so the
// output that follows starts on a clean line. A pull that never drew, such
// as one that found the image already present, prints nothing.
func (l *Line) Done(text string) {
	if l == nil || !l.drawn {
		return
	}
	if l.tty {
		_, _ = fmt.Fprint(l.w, "\r\033[K")
	}
	_, _ = fmt.Fprintln(l.w, text)
}

// HumanBytes formats n with a binary unit, as in "41.3 MB".
func HumanBytes(n int64) string {
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
