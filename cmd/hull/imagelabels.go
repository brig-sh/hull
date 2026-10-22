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
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// writeLabels prints an image's labels after a pull, one "key: value" line
// each, sorted by key.
//
// Labels are chosen by whoever built the image, and this prints them to a
// terminal. A label printed raw can break the layout with a newline, and can
// drive the terminal with an escape sequence: OSC 52 reads and writes the
// clipboard, and CSI 21t types its reply into the shell's input.
// printableLabel keeps every key and value to one line of plain text.
func writeLabels(w io.Writer, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	_, err := fmt.Fprintln(w, "Labels:")
	if err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		_, err = fmt.Fprintf(w, "  %s: %s\n", printableLabel(k), printableLabel(labels[k]))
		if err != nil {
			return err
		}
	}
	return nil
}

// printableLabel returns s as it is when it is valid UTF-8 made only of
// printable characters and spaces, and Go-quoted otherwise. Quoting spells
// a newline as \n, ESC as \x1b, a C1 control as \u0085 and an invalid byte
// as \x9b, so nothing reaches the terminal as a control. A value that starts
// with a double quote is quoted too, so a quoted value is never mistaken
// for one printed as it is.
func printableLabel(s string) string {
	plain := utf8.ValidString(s) &&
		!strings.HasPrefix(s, `"`) &&
		!strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
	if plain {
		return s
	}
	return strconv.Quote(s)
}
