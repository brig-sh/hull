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

//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A C1 control in a string reaches the terminal as two UTF-8 bytes, which
// the guest-output sanitizer passes through as text and a UTF-8 terminal
// reads as CSI or OSC. It must leave here as a \u escape: harmless on a
// terminal, and still the same string to a JSON reader.
func TestPrintJSONEscapesC1Controls(t *testing.T) {
	in := map[string]string{
		"csi":    "a\u009b31mb",
		"osc":    "\u009d0;evil", // no terminator: dropping would eat the quote
		"esc":    "c\x1b[31md",
		"latin1": "caf\u00e9 \u00ff",
		"cjk":    "\u65e5\u672c",
	}
	var buf bytes.Buffer
	if err := printJSON(&buf, in); err != nil {
		t.Fatalf("printJSON: %v", err)
	}
	out := buf.String()

	for _, r := range out {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("U+%04X reached the output raw:\n%s", r, out)
		}
	}
	for _, want := range []string{`\u009b`, `\u009d`, `\u001b`, "caf\u00e9 \u00ff", "\u65e5\u672c"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	var back map[string]string
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	for k, v := range in {
		if back[k] != v {
			t.Errorf("%s decoded to %q, want %q", k, back[k], v)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("output must end with a newline")
	}
}

func TestEscapeC1LeavesCleanTextAlone(t *testing.T) {
	for _, s := range []string{"", "plain", "caf\u00e9", "\u65e5\u672c", `{"a": "b"}`} {
		if got := escapeC1(s); got != s {
			t.Errorf("escapeC1(%q) = %q, want it unchanged", s, got)
		}
	}
}
