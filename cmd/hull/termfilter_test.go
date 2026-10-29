//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// filterGuest runs the writes through one sanitizer, as an interactive
// terminal would see them.
func filterGuest(t *testing.T, writes ...string) string {
	t.Helper()
	var out bytes.Buffer
	san := &terminalSanitizer{w: &out}
	for _, w := range writes {
		n, err := san.Write([]byte(w))
		if err != nil {
			t.Fatalf("sanitizer write: %v", err)
		}
		if n != len(w) {
			t.Fatalf("short write: %d of %d", n, len(w))
		}
	}
	return out.String()
}

// Every one of these makes the terminal write into its own input buffer, where
// the bytes are read back either by hull's stdin pump (and handed to the
// guest) or by the operator's shell once hull exits. A guest gets the host
// clipboard from the first, and a line of text of its choosing from the
// title round-trip.
func TestGuestOutputDropsRepliableSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"OSC 52 clipboard read", "\x1b]52;c;?\x07"},
		// The clipboard WRITE is deliberately allowed: it is how every TUI
		// copies, and refusing it broke copy/paste for real users. The read
		// above stays refused -- it makes the terminal hand the guest whatever
		// the operator last copied.
		{"OSC 1337 iTerm2 channel", "\x1b]1337;File=name=cGF5bG9hZA==:AAAA\x07"},
		{"OSC 11 background query", "\x1b]11;?\x07"},
		{"OSC 4 palette query", "\x1b]4;1;?\x07"},
		{"CSI primary device attributes", "\x1b[c"},
		{"CSI secondary device attributes", "\x1b[>0c"},
		{"CSI cursor position report", "\x1b[6n"},
		{"CSI device status report", "\x1b[5n"},
		{"CSI XTVERSION", "\x1b[>q"},
		{"CSI DECRQM mode report", "\x1b[?2026$p"},
		{"CSI DECREQTPARM", "\x1b[0x"},
		{"CSI report window title", "\x1b[21t"},
		{"CSI report window size", "\x1b[18t"},
		{"ESC Z identify terminal", "\x1bZ"},
		{"DCS terminfo query", "\x1bP+q544e\x1b\\"},
		{"DCS DECRQSS", "\x1bP$qm\x1b\\"},
		{"DCS tmux passthrough", "\x1bPtmux;\x1b\x1b]52;c;?\x07\x1b\\"},
		{"APC kitty payload", "\x1b_Gf=100;AAAA\x1b\\"},
		{"8-bit CSI cursor report", "\x9b6n"},
		{"8-bit OSC clipboard read", "\x9d52;c;?\x07"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterGuest(t, "before"+tc.in+"after")
			if got != "beforeafter" {
				t.Fatalf("sequence reached the terminal: %q", got)
			}
		})
	}
}

// The whole point of `hull exec` is that a coding-agent TUI works, so the
// filter has to be narrow. Anything that only paints the screen goes through
// untouched.
func TestGuestOutputKeepsTUISequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"SGR colour", "\x1b[1;31mred\x1b[0m"},
		{"cursor movement", "\x1b[10;20H\x1b[2J\x1b[K"},
		{"alternate screen", "\x1b[?1049h\x1b[?1049l"},
		{"bracketed paste", "\x1b[?2004h\x1b[200~x\x1b[201~\x1b[?2004l"},
		{"mouse tracking", "\x1b[?1000h\x1b[?1006h\x1b[?1006l"},
		{"scroll region", "\x1b[2;40r"},
		{"cursor shape", "\x1b[5 q"},
		{"soft reset", "\x1b[!p"},
		{"title stack push and pop", "\x1b[22;0t\x1b[23;0t"},
		{"set window title", "\x1b]0;agent\x07"},
		{"hyperlink with a query string", "\x1b]8;;https://example.com/a?b=c\x1b\\link\x1b]8;;\x1b\\"},
		{"shell integration marks", "\x1b]133;A\x07\x1b]133;B\x07"},
		{"save and restore cursor", "\x1b7\x1b8"},
		{"charset selection", "\x1b(B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterGuest(t, tc.in); got != tc.in {
				t.Fatalf("a display sequence was mangled:\nwant %q\ngot  %q", tc.in, got)
			}
		})
	}
}

// Escape sequences do not respect frame boundaries. A filter that judged each
// write on its own would pass the head of a split OSC 52 through and then find
// nothing left to reject.
func TestGuestOutputRejoinsSequencesSplitAcrossWrites(t *testing.T) {
	if got := filterGuest(t, "a\x1b]5", "2;c;?", "\x07b"); got != "ab" {
		t.Fatalf("split clipboard query got through: %q", got)
	}
	if got := filterGuest(t, "\x1b[1;3", "1mred"); got != "\x1b[1;31mred" {
		t.Fatalf("split colour sequence was mangled: %q", got)
	}
}

// Dropping bytes 0x80-0x9F outright would be the naive way to kill 8-bit CSI,
// and it would corrupt every multi-byte rune, since UTF-8 continuation bytes
// live in that range.
func TestGuestOutputPreservesUTF8(t *testing.T) {
	const text = "λ ελληνικά — 世界 🚀"
	if got := filterGuest(t, text); got != text {
		t.Fatalf("UTF-8 was mangled:\nwant %q\ngot  %q", text, got)
	}
	// Split in the middle of the rocket's four bytes.
	raw := []byte(text)
	if got := filterGuest(t, string(raw[:len(raw)-2]), string(raw[len(raw)-2:])); got != text {
		t.Fatalf("UTF-8 split across writes was mangled: %q", got)
	}
}

// An unterminated string sequence must not be able to buffer the session's
// whole output, nor stall it behind a terminator that never arrives.
func TestGuestOutputBoundsTheHeldBytes(t *testing.T) {
	var out bytes.Buffer
	san := &terminalSanitizer{w: &out}
	if _, err := san.Write([]byte("\x1b]52;c;" + strings.Repeat("A", maxHeldEscape+16))); err != nil {
		t.Fatal(err)
	}
	if len(san.held) > maxHeldEscape {
		t.Fatalf("held %d bytes waiting for a terminator", len(san.held))
	}
	if _, err := san.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "tail") {
		t.Fatalf("the stream never resynchronised: %q", out.String())
	}
}

// A pipe, a file or a CI log has no input buffer to write into, and rewriting
// captured output would be a bug of its own.
func TestGuestOutputIsNotFilteredWhenStdoutIsNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if w := guestTerminalWriter(f); w != f {
		t.Fatalf("a non-terminal destination was wrapped: %T", w)
	}
}

// `hull logs` replays the guest's own console bytes onto the operator's
// terminal, long after the guest wrote them; the sequences work just as well
// from a log file as they do live.
func TestLogsGoThroughTheSameFilter(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(logFile, []byte("boot ok\x1b]52;c;?\x07\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := readLogs(&terminalSanitizer{w: &out}, logFile, false, -1); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(out.String(), "\x1b]52") {
		t.Fatalf("the log replayed a clipboard query: %q", out.String())
	}
	if !strings.Contains(out.String(), "boot ok") {
		t.Fatalf("the log text itself was lost: %q", out.String())
	}
}
