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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// --- the seam ----------------------------------------------------------------
//
// vz-runner/Sources/main.swift ports the exact policy in exec.go's
// sanitizeTerminalBytes to Swift, for the guest console `hull run` wires
// straight to the operator's terminal in foreground mode. `vz-runner
// --filter-selftest` (main.swift:1396) reads stdin, runs it through that
// Swift TerminalSanitizer, writes the result to stdout, and exits -- the
// runner is a top-level-code executable that SwiftPM cannot host a test
// target for, so this CLI seam is, in the repo's own words, "the seam that
// keeps the policy testable" (main.swift:1392-1395). Nothing tracked invoked
// it before this file.
//
// Two independent implementations of the same written policy is the point:
// a bug shipped to one side and not the other is exactly what a single-sided
// unit test cannot see. So every case below carries an expected result
// derived by hand from the policy documented above guestTerminalWriter in
// exec.go, not copied from either implementation's output. Differential
// equality is checked too, but on its own it is not enough -- a bug ported
// to both sides would still agree with itself.

// swiftFilterBinary locates a built vz-runner binary that supports
// --filter-selftest, preferring a fresh debug build over the checked-in
// dist/ copy (which lags HEAD and is not rebuilt by this test). Returns ""
// when none exists, and the caller must skip loudly rather than silently.
func swiftFilterBinary(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("HULL_VZ_RUNNER_BIN"); override != "" {
		if fi, err := os.Stat(override); err == nil && !fi.IsDir() {
			return override
		}
		t.Fatalf("HULL_VZ_RUNNER_BIN=%q does not point at a file", override)
	}
	// Paths are relative to this package's directory (cmd/hull).
	candidates := []string{
		"../../vz-runner/.build/debug/vz-runner",
		"../../vz-runner/.build/release/vz-runner",
		"../../dist/vz-runner",
	}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		return abs
	}
	return ""
}

// requireSwiftFilter returns a usable vz-runner binary path or skips the test
// with a specific, unmissable reason. This repo was bitten by two guard
// tests that skipped on every host for months (supervisor_test.go) -- a skip
// here must name exactly what is missing and how to fix it, not go quiet.
func requireSwiftFilter(t *testing.T) string {
	t.Helper()
	bin := swiftFilterBinary(t)
	if bin == "" {
		t.Skipf("SWIFT BINARY NOT FOUND: checked ../../vz-runner/.build/debug/vz-runner, "+
			"../../vz-runner/.build/release/vz-runner and ../../dist/vz-runner (all relative to %s), "+
			"and $HULL_VZ_RUNNER_BIN is unset. This test exercises the vz-runner --filter-selftest "+
			"contract seam (main.swift:1396) and cannot run without a built binary. Build one with "+
			"`cd vz-runner && swift build` and re-run.", mustGetwd(t))
	}
	return bin
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "(unknown working directory)"
	}
	return wd
}

// runSwiftFilter feeds in through one `vz-runner --filter-selftest` process
// and returns what it wrote to stdout. Each call is a fresh process, so it
// exercises the Swift TerminalSanitizer from a clean state -- exactly the
// "whole input in one shot" case.
func runSwiftFilter(t *testing.T, bin string, in []byte) []byte {
	t.Helper()
	cmd := exec.Command(bin, "--filter-selftest")
	cmd.Stdin = bytes.NewReader(in)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("vz-runner --filter-selftest failed: %v (stderr: %s)", err, errBuf.String())
	}
	return out.Bytes()
}

// filterCase is one corpus entry. want is derived by hand from the policy
// documented in exec.go, independent of what either implementation actually
// does -- see the file-level comment for why that independence matters.
type filterCase struct {
	name string
	in   []byte
	want []byte
}

// terminalContractCorpus is the shared input fed to both implementations.
// Every case states the policy rule it exercises.
func terminalContractCorpus() []filterCase {
	return []filterCase{
		{
			name: "plain text passes through unchanged",
			in:   []byte("hello world\n"),
			want: []byte("hello world\n"),
		},
		{
			name: "an ordinary BEL not part of any sequence passes through",
			in:   []byte("line1\r\nline2\t\x07"),
			want: []byte("line1\r\nline2\t\x07"),
		},
		{
			name: "OSC 52 SET is allowed (a clipboard write, needed for copy/paste)",
			in:   []byte("\x1b]52;c;aGVsbG8=\x07"),
			want: []byte("\x1b]52;c;aGVsbG8=\x07"),
		},
		{
			name: "OSC 52 QUERY is blocked (would leak the host clipboard)",
			in:   []byte("\x1b]52;c;?\x07"),
			want: nil,
		},
		{
			name: "DSR cursor position report is blocked (types into the tty input)",
			in:   []byte("\x1b[6n"),
			want: nil,
		},
		{
			name: "device attributes query (CSI c) is blocked",
			in:   []byte("\x1b[c"),
			want: nil,
		},
		{
			name: "CSI 20 t (report window title) is blocked",
			in:   []byte("\x1b[20t"),
			want: nil,
		},
		{
			name: "CSI 22 t (push title, no reply) is allowed, unlike other CSI t ops",
			in:   []byte("\x1b[22t"),
			want: []byte("\x1b[22t"),
		},
		{
			name: "plain SGR colour passes through (the filter is narrow, not a blanket strip)",
			in:   []byte("\x1b[1m"),
			want: []byte("\x1b[1m"),
		},
		{
			name: "an 8-bit CSI introducer is dropped even though its 7-bit SGR form is safe",
			// 0x9b is CSI's C1 spelling. The policy drops every C1-introduced
			// sequence outright, regardless of what the 7-bit form would have
			// been -- see the c1Introducer block comment in exec.go.
			in:   []byte("\x9b1m"),
			want: nil,
		},
		{
			name: "an 8-bit OSC introducer carrying a clipboard query is blocked",
			in:   []byte("\x9d52;c;?\x07"),
			want: nil,
		},
		{
			name: "a stray C1 byte that is not an introducer is dropped, text around it kept",
			in:   []byte("a\x85b"), // 0x85 is NEL, not one of the mapped introducers
			want: []byte("ab"),
		},
		{
			name: "ESC Z (obsolete identify-terminal) is blocked",
			in:   []byte("\x1bZ"),
			want: nil,
		},
		{
			name: "ESC % @ (leave UTF-8) is blocked",
			in:   []byte("\x1b%@"),
			want: nil,
		},
		{
			name: "ESC ( 0 (smacs box-drawing charset select) passes through",
			// Regression note in exec.go: this was once dropped by mistake,
			// which rendered every TUI border as literal "lqqqk".
			in:   []byte("\x1b(0"),
			want: []byte("\x1b(0"),
		},
		{
			name: "a clipboard query smuggled behind an aborted CSI is blocked",
			in:   []byte("\x1b[\x1b]52;c;?\x07"),
			want: nil,
		},
		{
			name: "a clipboard query smuggled behind an 8-bit CSI abort is blocked",
			in:   []byte("\x9b\x1b]52;c;?\x07"),
			want: nil,
		},
		{
			name: "a clipboard query smuggled through a tmux DCS passthrough is blocked",
			in:   []byte("\x1bPtmux;\x1b\x1b]52;c;?\x07\x1b\\"),
			want: nil,
		},
		{
			name: "a 2-byte UTF-8 rune passes through untouched",
			in:   []byte("a\xc3\xa9b"), // "aéb"
			want: []byte("a\xc3\xa9b"),
		},
		{
			name: "a 3-byte UTF-8 rune passes through untouched",
			in:   []byte("a\xe2\x82\xacb"), // "a€b"
			want: []byte("a\xe2\x82\xacb"),
		},
		{
			name: "a 4-byte UTF-8 rune passes through untouched",
			in:   []byte("a\xf0\x9f\x98\x80b"), // "a\U0001f600b"
			want: []byte("a\xf0\x9f\x98\x80b"),
		},
		{
			name: "an incomplete OSC sequence at EOF produces no output for its tail",
			in:   []byte("\x1b]52;c;?"), // no BEL/ST: never completes
			want: nil,
		},
		{
			name: "an incomplete UTF-8 rune at EOF produces no output for its tail",
			in:   []byte("a\xe2"), // lead byte of a 3-byte rune, then nothing
			want: []byte("a"),
		},
	}
}

// TestTerminalFilterMatchesSwiftBinary feeds the corpus through both
// implementations of the same written policy and checks each against its own
// hand-derived expectation as well as against each other.
//
// Regression this catches: either sanitizeTerminalBytes (Go, exec.go) or
// TerminalSanitizer (Swift, main.swift) drifting from the documented policy,
// or the two silently drifting from each other -- e.g. one side starting to
// allow a clipboard query, or dropping an OSC 52 SET and breaking copy/paste,
// or treating a C1-introduced sequence as safe because its 7-bit spelling is.
func TestTerminalFilterMatchesSwiftBinary(t *testing.T) {
	bin := requireSwiftFilter(t)
	t.Logf("using vz-runner binary: %s", bin)

	for _, tc := range terminalContractCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			goOut, _ := sanitizeTerminalBytes(tc.in)
			swiftOut := runSwiftFilter(t, bin, tc.in)

			if !bytes.Equal(goOut, tc.want) {
				t.Errorf("Go filter disagrees with the documented policy:\n  in:   % x\n  got:  % x\n  want: % x",
					tc.in, goOut, tc.want)
			}
			if !bytes.Equal(swiftOut, tc.want) {
				t.Errorf("Swift filter disagrees with the documented policy:\n  in:   % x\n  got:  % x\n  want: % x",
					tc.in, swiftOut, tc.want)
			}
			if !bytes.Equal(goOut, swiftOut) {
				// Reported on top of the two checks above, not instead of
				// them: two implementations that agree can still both be
				// wrong, which is exactly why want is independent of both.
				t.Errorf("PRODUCTION BUG: Go and Swift disagree on the same input:\n  in:        % x\n  go out:    % x\n  swift out: % x",
					tc.in, goOut, swiftOut)
			}
		})
	}
}

// --- chunk boundaries ---------------------------------------------------------
//
// A filter that is correct on a whole input and wrong on a split one is the
// realistic bug: guest output reaches the Go filter one agent-protocol frame
// at a time (exec.go's guestTerminalWriter / terminalSanitizer.Write), and
// reaches the Swift filter one virtio-console read at a time -- an escape
// sequence spanning two of either is normal, not an edge case.
//
// Forcing the Swift *subprocess* to physically split its stdin reads at a
// chosen byte offset would mean racing two writes against however fast the
// process gets around to calling read(2) -- which the test-quality bar rules
// out (no correctness that depends on timing). So the Swift binary is instead
// used the way the differential test above already validated it: as a
// second, independent implementation whose whole-input result is ground
// truth for what the reassembled output must equal. Go's own incremental
// buffering is what actually gets exercised split, via terminalSanitizer.Write
// called once per chunk -- the same API guestTerminalWriter wraps.

// writeChunked drives a fresh terminalSanitizer with each of chunks in turn,
// the way successive agent-protocol frames would, and returns everything it
// wrote out.
func writeChunked(t *testing.T, chunks ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	ts := &terminalSanitizer{w: &buf}
	for _, c := range chunks {
		if _, err := ts.Write(c); err != nil {
			t.Fatalf("terminalSanitizer.Write: %v", err)
		}
	}
	return buf.Bytes()
}

// splitCase is a whole sequence to be cut at every possible byte offset.
type splitCase struct {
	name string
	seq  []byte
	want []byte // hand-derived; also cross-checked against the Swift binary's whole-input result below
}

func splitCorpus() []splitCase {
	return []splitCase{
		{
			name: "OSC 52 SET, allowed, split at every point must still reassemble unchanged",
			seq:  []byte("\x1b]52;c;aGVsbG8=\x07"),
			want: []byte("\x1b]52;c;aGVsbG8=\x07"),
		},
		{
			name: "OSC 52 QUERY, blocked, split at every point must still be fully blocked",
			seq:  []byte("\x1b]52;c;?\x07"),
			want: nil,
		},
		{
			name: "DSR cursor query, blocked, split at every point",
			seq:  []byte("\x1b[6n"),
			want: nil,
		},
		{
			name: "CSI 22 t, allowed, split at every point must still reassemble unchanged",
			seq:  []byte("\x1b[22t"),
			want: []byte("\x1b[22t"),
		},
		{
			name: "clipboard query smuggled behind an aborted CSI, split at every point must stay blocked",
			// The sequence-smuggling case, combined with chunk splitting: this
			// is the one place a split could plausibly let the fragment and
			// the OSC be judged independently in a way that lets the query
			// through, if the held-byte reassembly were wrong.
			seq:  []byte("\x1b[\x1b]52;c;?\x07"),
			want: nil,
		},
		{
			name: "clipboard query smuggled through a tmux passthrough, split at every point must stay blocked",
			seq:  []byte("\x1bPtmux;\x1b\x1b]52;c;?\x07\x1b\\"),
			want: nil,
		},
		{
			name: "text around a 2-byte UTF-8 rune, split at every point including inside the rune",
			seq:  []byte("a\xc3\xa9b"),
			want: []byte("a\xc3\xa9b"),
		},
		{
			name: "text around a 3-byte UTF-8 rune, split at every point including inside the rune",
			seq:  []byte("a\xe2\x82\xacb"),
			want: []byte("a\xe2\x82\xacb"),
		},
		{
			name: "text around a 4-byte UTF-8 rune, split at every point including inside the rune",
			seq:  []byte("a\xf0\x9f\x98\x80b"),
			want: []byte("a\xf0\x9f\x98\x80b"),
		},
	}
}

// TestTerminalFilterChunkBoundariesReassembleCorrectly cuts each sequence in splitCorpus at
// every byte offset, feeds the two halves through terminalSanitizer as two
// separate writes, and checks the reassembled output both against the
// hand-derived expectation and against the Swift binary's whole-input result
// for the same bytes.
//
// Regression this catches: terminalSanitizer's held-byte buffer losing or
// misjudging a sequence cut at some particular offset -- e.g. holding too
// little and emitting a fragment that would make the terminal itself finish
// parsing a mode-setting or clipboard sequence out of whatever ordinary text
// comes next, or holding too much and swallowing bytes that arrived after a
// sequence that was actually already complete.
func TestTerminalFilterChunkBoundariesReassembleCorrectly(t *testing.T) {
	bin := requireSwiftFilter(t)
	t.Logf("using vz-runner binary: %s", bin)

	for _, sc := range splitCorpus() {
		t.Run(sc.name, func(t *testing.T) {
			// Ground truth for this whole sequence, computed once: an
			// independent implementation's answer for the unsplit bytes.
			swiftWhole := runSwiftFilter(t, bin, sc.seq)
			if !bytes.Equal(swiftWhole, sc.want) {
				t.Fatalf("PRODUCTION BUG: Swift's whole-input result disagrees with the documented "+
					"policy before any splitting is even tried:\n  in:   % x\n  got:  % x\n  want: % x",
					sc.seq, swiftWhole, sc.want)
			}

			for i := 0; i <= len(sc.seq); i++ {
				got := writeChunked(t, sc.seq[:i], sc.seq[i:])
				if !bytes.Equal(got, sc.want) {
					t.Errorf("split at byte %d/%d: got % x, want % x (hand-derived)",
						i, len(sc.seq), got, sc.want)
				}
				if !bytes.Equal(got, swiftWhole) {
					t.Errorf("split at byte %d/%d: got % x, want % x (Swift's whole-input result)",
						i, len(sc.seq), got, swiftWhole)
				}
			}
		})
	}
}

// --- what this does NOT cover: the Swift binary's own cross-read buffer -----
//
// Everything above proves Go's incremental buffering (terminalSanitizer.Write,
// exercised chunked) reassembles splits correctly, using the Swift binary's
// whole-input result as ground truth. It does not exercise the Swift-side
// TerminalSanitizer's own `held` field (main.swift:320, carried across
// separate calls to filter(), mirroring terminalSanitizer.held here) across
// two of its own reads, and that is not for lack of trying.
//
// The natural non-racy approach is to prime the first chunk with a plain byte
// the filter always passes through immediately, block on reading exactly that
// byte back, and only then write the second chunk -- the subprocess can only
// have written it after a read()+filter() call completed on a buffer that,
// since nothing else was written yet, could only have been the first chunk.
// That is a real happens-before edge, not a sleep.
//
// It does not work against this seam. MEASURED: `vz-runner --filter-selftest`
// given 1000 bytes of plain 'A' on stdin, left open, produces no output at
// all for over a second -- output appears only once stdin is closed. The
// seam's read loop is `while let chunk = try? FileHandle.standardInput.read(
// upToCount: 65536), !chunk.isEmpty` (main.swift:1398), and on this runtime
// that call evidently blocks until either 65536 bytes have arrived or EOF,
// rather than returning whatever is currently available the way a read(2) on
// a pipe ordinarily would. There is no reachable point at which chunk one
// alone has been read and filtered while chunk two is still unsent, so the
// priming barrier above has nothing to block on, and the naive version of
// this test (attempted and removed) simply times out on every run -- not
// flaky, not slow, structurally unable to observe the intermediate state.
//
// This is specific to the --filter-selftest CLI seam, not to vz-runner's
// actual console handling: the real guest-console path (VZManager's
// consoleOutputHandle, main.swift:673-700) drives the same TerminalSanitizer
// from a `readabilityHandler` using `handle.availableData`, which returns
// whatever is currently buffered rather than waiting to fill a fixed count.
// So the cross-read `held` behavior does run in production; this seam is
// just the wrong instrument for reaching it from outside without editing
// main.swift to add a second, streaming-shaped seam -- out of scope here (see
// PLAN.md's Swift package split, deferred). Recorded in the task report as a
// remaining risk rather than silently dropped.
