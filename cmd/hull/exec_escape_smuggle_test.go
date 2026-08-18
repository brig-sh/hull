// Copyright (c) 2023-2026, Nubificus LTD
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
	"bytes"
	"strings"
	"testing"
	"time"
)

// ESC is not a final byte.
//
// The CSI scanner accepted whatever byte followed the parameters and
// intermediates as the final byte, including ESC. A real terminal treats ESC
// as an "anywhere" transition: it abandons the CSI and starts a new sequence.
// So "ESC [ ESC ] 52;c;<base64> BEL" was measured as a complete, harmless CSI
// followed by ordinary text, and the clipboard write went straight through --
// while the same OSC on its own was correctly dropped.
func TestEscapeCannotBeSmuggledBehindAnAbortedSequence(t *testing.T) {
	const payload = "52;c;aGVsbG8="
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"aborted CSI then OSC 52 set", "\x1b[\x1b]" + payload + "\x07"},
		{"aborted CSI with params", "\x1b[1;2\x1b]" + payload + "\x07"},
		{"aborted charset select then OSC 52", "\x1b(\x1b]" + payload + "\x07"},
		{"aborted CSI then a title report", "\x1b[\x1b[21t"},
		{"aborted CSI then a cursor report", "\x1b[\x1b[6n"},
		{"8-bit CSI aborted then OSC 52", "\x9b\x1b]" + payload + "\x07"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := sanitizeTerminalBytes([]byte(tc.in))
			if bytes.Contains(out, []byte(payload)) {
				t.Errorf("a clipboard payload reached the terminal: %q", out)
			}
			// The reporting sequences must not survive either: their replies
			// are injected into this terminal's input.
			for _, bad := range []string{"\x1b[21t", "\x1b[6n"} {
				if bytes.Contains(out, []byte(bad)) {
					t.Errorf("a terminal query reached the terminal: %q", out)
				}
			}
		})
	}
}

// The control: the plain forms were always dropped, and still are. Without
// this, a filter that dropped everything would look like it was working.
func TestOrdinaryOutputStillGetsThrough(t *testing.T) {
	in := "hello \x1b[1mbold\x1b[0m world\r\n"
	out, _ := sanitizeTerminalBytes([]byte(in))
	if string(out) != in {
		t.Errorf("ordinary output was mangled:\n got %q\nwant %q", out, in)
	}
}

// Terminals parse the OSC code as a number, so a leading zero is the same
// sequence to them and a different string to us.
func TestOSCCodesAreComparedNumerically(t *testing.T) {
	for _, in := range []string{
		"\x1b]52;c;?\x07",
		"\x1b]052;c;?\x07",
		"\x1b]0052;c;aGk=\x07",
		"\x1b]1337;Clipboard=x\x07",
		"\x1b]01337;Clipboard=x\x07",
	} {
		out, _ := sanitizeTerminalBytes([]byte(in))
		if len(out) != 0 {
			t.Errorf("%q survived the filter as %q", in, out)
		}
	}
}

// A guest printing C1 introducers used to cost time quadratic in the frame:
// each one rebuilt a copy of the whole remaining buffer. Frames are up to a
// mebibyte, which measured at about seventeen seconds of pegged CPU each --
// enough to wedge `hull exec` and `hull logs` for as long as the guest cared
// to keep printing.
func TestC1ScanIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n = 1 << 20
	start := time.Now()
	sanitizeTerminalBytes(bytes.Repeat([]byte{0x9b}, n))
	elapsed := time.Since(start)
	t.Logf("1 MiB of 0x9b filtered in %s", elapsed)
	// Generous: the point is linear-vs-quadratic, and the old code took about
	// 17s for this input.
	if elapsed > 2*time.Second {
		t.Errorf("filtering 1 MiB of C1 introducers took %s; the scan is still "+
			"copying the remainder per byte", elapsed)
	}
}

// Guest-supplied text that hull puts into an error message reaches the
// terminal through fatal(), which does no filtering of its own.
func TestGuestSuppliedErrorTextIsSanitised(t *testing.T) {
	raw := "boom \x1b]52;c;ZXJyb3IK\x07 done"
	got := sanitizeGuestText(raw)
	if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x07") {
		t.Errorf("guest text kept its escapes: %q", got)
	}
	if !strings.Contains(got, "boom") || !strings.Contains(got, "done") {
		t.Errorf("guest text lost its readable content: %q", got)
	}
}

// A fragment must not be emitted either, which is the subtler half.
//
// The terminal does not stop parsing where our scanner stopped. If "ESC [ 1;2"
// reaches it, it keeps consuming bytes looking for a final one -- so the next
// ordinary character we pass through supplies it. "hello" completes the
// fragment as ESC[1;2h, which sets a mode the guest chose, from text the guest
// also chose.
func TestNoEscapeFragmentReachesTheTerminal(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"aborted CSI with params, then text", "\x1b[1;2\x1b]52;c;?\x07hello"},
		{"aborted charset select, then text", "\x1b(\x1b]52;c;?\x07hello"},
		{"bare aborted CSI, then text", "\x1b[\x1bhello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := sanitizeTerminalBytes([]byte(tc.in))
			// An incomplete CSI is the dangerous shape: the terminal keeps
			// consuming until something supplies its final byte. A complete
			// sequence that happens to contain ESC -- ordinary SGR, say -- is
			// not a fragment and is judged on its own merits elsewhere.
			if i := bytes.Index(out, []byte("\x1b[")); i >= 0 {
				tail := out[i+2:]
				if len(tail) == 0 || tail[len(tail)-1] < 0x40 || tail[len(tail)-1] > 0x7e {
					t.Errorf("an unterminated CSI reached the terminal: %q", out)
				}
			}
			if !bytes.Contains(out, []byte("hello")) {
				t.Errorf("the ordinary text was dropped along with it: %q", out)
			}
		})
	}
}
