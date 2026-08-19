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

import "testing"

// Copying must work. Reading the operator's clipboard must not.
//
// Blocking OSC 52 outright broke copy/paste for real users, which is a bad
// trade for what it bought: the worst a SET does is overwrite what you were
// about to paste. A QUERY is a different thing entirely -- the terminal replies
// with the clipboard's current contents, delivered into the guest's stdin, so
// whatever was last copied on this machine is handed to the sandbox.
func TestOSC52SetIsAllowedAndQueryIsNot(t *testing.T) {
	allowed := []struct{ name, in string }{
		{"set clipboard, bel-terminated", "\x1b]52;c;aGVsbG8=\x07"},
		{"set clipboard, st-terminated", "\x1b]52;c;aGVsbG8=\x1b\\"},
		{"set primary selection", "\x1b]52;p;aGVsbG8=\x07"},
		{"set several targets", "\x1b]52;cp;aGVsbG8=\x07"},
		{"set with an empty selector", "\x1b]52;;aGVsbG8=\x07"},
		{"clear the clipboard", "\x1b]52;c;!\x07"},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			out, _ := sanitizeTerminalBytes([]byte(tc.in))
			if string(out) != tc.in {
				t.Errorf("a clipboard SET was dropped, which breaks copy: %q -> %q", tc.in, out)
			}
		})
	}

	refused := []struct{ name, in string }{
		{"query clipboard", "\x1b]52;c;?\x07"},
		{"query, st-terminated", "\x1b]52;c;?\x1b\\"},
		{"query primary", "\x1b]52;p;?\x07"},
		{"query with padding", "\x1b]52;c; ? \x07"},
		{"query with a leading zero code", "\x1b]052;c;?\x07"},
		{"no data field at all", "\x1b]52;c\x07"},
		{"nothing but the code", "\x1b]52\x07"},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			out, _ := sanitizeTerminalBytes([]byte(tc.in))
			if len(out) != 0 {
				t.Errorf("a clipboard QUERY reached the terminal: %q -> %q", tc.in, out)
			}
		})
	}
}

// A query must not become reachable by hiding it behind something else.
func TestOSC52QueryCannotBeSmuggled(t *testing.T) {
	for _, in := range []string{
		"\x1b[\x1b]52;c;?\x07",                // behind an aborted CSI
		"\x1b\x9d52;c;?\x07",                  // behind an 8-bit introducer
		"\x1bPtmux;\x1b\x1b]52;c;?\x07\x1b\\", // through a tmux passthrough
		"\x1b]52;c;aGk=\x07\x1b]52;c;?\x07",   // a set first, then a query
	} {
		out, _ := sanitizeTerminalBytes([]byte(in))
		// The set in the last case may legitimately survive; the query's "?"
		// data field must not reach the terminal inside an OSC 52.
		if containsClipboardQuery(out) {
			t.Errorf("a clipboard query survived: %q -> %q", in, out)
		}
	}
}

// containsClipboardQuery looks for an intact OSC 52 whose data field is "?".
func containsClipboardQuery(b []byte) bool {
	s := string(b)
	for i := 0; i+2 < len(s); i++ {
		if s[i] != 0x1b || s[i+1] != ']' {
			continue
		}
		rest := s[i+2:]
		if len(rest) < 3 {
			continue
		}
		// crude but sufficient: an OSC 52 with a "?" before its terminator
		end := len(rest)
		for j := 0; j < len(rest); j++ {
			if rest[j] == 0x07 || rest[j] == 0x1b {
				end = j
				break
			}
		}
		body := rest[:end]
		if len(body) > 2 && body[0] == '5' && body[1] == '2' &&
			body[len(body)-1] == '?' {
			return true
		}
	}
	return false
}
