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
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// printJSON writes v as indented JSON, followed by a newline, for a terminal
// that may be reading it as well as a machine.
//
// json.Marshal escapes control characters below U+0020 but leaves
// U+0080-U+009F alone, and those are the 8-bit spellings of CSI, OSC and DCS,
// which a UTF-8 terminal honours as readily as the ESC form. The guest-output
// sanitizer does not catch them here either: it drops C1 controls that arrive
// as raw single bytes, and by the time it looks json.Marshal has encoded them
// as two-byte UTF-8, which it passes through as text. An image label or an
// instance name carrying one would drive the terminal of whoever listed it.
//
// Escaping them as \u00XX keeps the output valid JSON that decodes to the
// same string, which dropping bytes would not: an unterminated OSC would
// swallow the closing quote and everything after it.
func printJSON(out io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal to JSON: %w", err)
	}
	_, err = fmt.Fprintln(out, escapeC1(string(data)))
	return err
}

// escapeC1 spells U+0080-U+009F as JSON \u escapes. Everything else,
// including other non-ASCII text, is returned as it was.
func escapeC1(s string) string {
	isC1 := func(r rune) bool { return r >= 0x80 && r <= 0x9f }
	if !strings.ContainsFunc(s, isC1) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isC1(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
