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
	"testing"
)

// ESC followed by an 8-bit C1 introducer is the same smuggling attack as
// "ESC [ ESC ]", through a different door.
//
// escapeLen treats "ESC <byte>" as a complete two-byte sequence for anything
// outside its known set, and 0x90/0x9b/0x9d are outside it. The terminal takes
// the C1 as an anywhere-transition into CSI/OSC state, so everything the filter
// then passes as ordinary text becomes the sequence body.
func TestEscapeThenC1IntroducerIsRefused(t *testing.T) {
	const payload = "52;c;aGVsbG8="
	for _, tc := range []struct{ name, in string }{
		{"ESC + OSC introducer, clipboard write", "\x1b\x9d" + payload + "\a"},
		{"ESC + OSC introducer, clipboard read", "\x1b\x9d52;c;?\a"},
		{"ESC + CSI introducer, cursor report", "\x1b\x9b6n"},
		{"ESC + CSI introducer, device attributes", "\x1b\x9bc"},
		{"ESC + DCS introducer", "\x1b\x90whatever\x1b\\"},
		{"leave UTF-8 then smuggle", "\x1b%@\x1b\x9d" + payload + "\a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := sanitizeTerminalBytes([]byte(tc.in))
			// The property is that no INTRODUCER survives. Once the C1 byte is
			// gone the rest ("52;c;aGVsbG8=") is literal text on the screen --
			// untidy, not a control, and not something to assert away.
			for i, b := range out {
				if b >= 0x80 && b <= 0x9f {
					t.Errorf("an 8-bit C1 introducer survived at %d: %q", i, out)
					break
				}
			}
			if bytes.Contains(out, []byte("\x1b%")) {
				t.Errorf("a charset switch out of UTF-8 survived: %q", out)
			}
			if bytes.Contains(out, []byte("\x1b]")) || bytes.Contains(out, []byte("\x1b[")) {
				t.Errorf("a 7-bit introducer survived: %q", out)
			}
		})
	}
}

// The regression the round-5 final-byte rule introduced.
//
// ESC ( 0 is ncurses' smacs, the box-drawing charset, and its final byte is
// '0' (0x30). The CSI final-byte range 0x40-0x7e was applied to the whole
// ESC <intermediate> family, so every border in every TUI -- dialog, mc, vim's
// window splits -- rendered as the literal letters lqqqk.
func TestCharsetDesignatorsStillReachTheTerminal(t *testing.T) {
	for _, in := range []string{
		"\x1b(0", "\x1b)0", "\x1b(1", "\x1b(2", "\x1b(B", "\x1b(<", "\x1b(>",
		"\x1b#8", "\x1b#3", "\x1b#4", "\x1b#5", "\x1b#6",
	} {
		out, consumed := sanitizeTerminalBytes([]byte(in))
		if consumed != len(in) {
			t.Errorf("%q was not fully consumed (%d of %d)", in, consumed, len(in))
		}
		if string(out) != in {
			t.Errorf("%q was dropped or altered, got %q", in, out)
		}
	}
}

// The whole line a TUI actually emits.
func TestABoxDrawingLineSurvivesIntact(t *testing.T) {
	in := "\x1b(0lqqqk\x1b(B title\r\n"
	out, _ := sanitizeTerminalBytes([]byte(in))
	if string(out) != in {
		t.Errorf("a box-drawing line was mangled:\n got %q\nwant %q", out, in)
	}
}

// And the CSI family keeps its own, stricter rule.
func TestCSIStillRequiresAProperFinalByte(t *testing.T) {
	for _, in := range []string{"\x1b[1;2\x1b", "\x1b[\x1b", "\x1b[1;2"} {
		out, _ := sanitizeTerminalBytes([]byte(in))
		if bytes.Contains(out, []byte("\x1b[")) {
			t.Errorf("an unterminated CSI reached the terminal: %q", out)
		}
	}
}
