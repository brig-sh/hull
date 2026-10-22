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
	"strings"
	"testing"
)

func TestPrintableLabel(t *testing.T) {
	cases := map[string]string{
		"ubuntu":             "ubuntu",
		"26.04":              "26.04",
		"two words":          "two words",
		"Ubuntu – Canonical": "Ubuntu – Canonical",
		"":                   "",
		// A newline would break the "key: value" layout.
		"line one\n\nline two": `"line one\n\nline two"`,
		"tab\there":            `"tab\there"`,
		// OSC 52 sets the clipboard; CSI 21t makes the terminal type a reply.
		"x\x1b]52;c;cHduZWQ=\x07y": `"x\x1b]52;c;cHduZWQ=\ay"`,
		"\x1b[21t":                 `"\x1b[21t"`,
		// A C1 CSI, encoded as UTF-8 and as a lone invalid byte.
		"a\u009b21tb": `"a\u009b21tb"`,
		"a\x9b21tb":   `"a\x9b21tb"`,
		// Bidi overrides reorder what the reader sees.
		"abc\u202edef": `"abc\u202edef"`,
		// Quoted, so it cannot pass for a value that was quoted.
		`"already"`: `"\"already\""`,
	}
	for in, want := range cases {
		got := printableLabel(in)
		if got != want {
			t.Errorf("printableLabel(%q) = %s, want %s", in, got, want)
		}
		if strings.ContainsAny(got, "\n\r\x1b\x07") || strings.ContainsRune(got, '\u009b') {
			t.Errorf("printableLabel(%q) = %q still carries a control", in, got)
		}
	}
}

func TestWriteLabels(t *testing.T) {
	var buf bytes.Buffer
	err := writeLabels(&buf, map[string]string{
		"org.opencontainers.image.version":     "26.04",
		"org.opencontainers.image.description": "The Ubuntu image\n\nUbuntu is Debian-based.\n",
		"org.opencontainers.image.title":       "ubuntu",
		"evil\x1b]52;c;eA==\x07":               "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Labels:\n" +
		`  "evil\x1b]52;c;eA==\a": x` + "\n" +
		`  org.opencontainers.image.description: "The Ubuntu image\n\nUbuntu is Debian-based.\n"` + "\n" +
		"  org.opencontainers.image.title: ubuntu\n" +
		"  org.opencontainers.image.version: 26.04\n"
	if buf.String() != want {
		t.Errorf("writeLabels printed:\n%s\nwant:\n%s", buf.String(), want)
	}

	buf.Reset()
	err = writeLabels(&buf, nil)
	if err != nil || buf.Len() != 0 {
		t.Errorf("no labels: printed %q, err %v; want nothing", buf.String(), err)
	}
}
