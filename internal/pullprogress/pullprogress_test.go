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

package pullprogress

import (
	"bytes"
	"testing"
)

// Into a log, each draw is a line of its own, and draws closer together than
// Interval are dropped unless forced.
func TestLineInALog(t *testing.T) {
	var out bytes.Buffer
	l := New(&out)
	if l.Interval() != logRefresh {
		t.Errorf("Interval = %v, want %v for a log", l.Interval(), logRefresh)
	}
	l.Draw("first", false)
	l.Draw("dropped", false)
	l.Draw("forced", true)
	l.Done("final")
	want := "first\nforced\nfinal\n"
	if out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

// A line that never drew prints no final line either.
func TestLineDoneWithoutDraw(t *testing.T) {
	var out bytes.Buffer
	New(&out).Done("final")
	if out.Len() != 0 {
		t.Errorf("Done printed %q with nothing drawn", out.String())
	}
}

func TestNilLine(t *testing.T) {
	l := New(nil)
	if l != nil {
		t.Fatal("New(nil) returned a line")
	}
	l.Draw("x", true)
	l.Done("x")
	if l.Interval() != logRefresh {
		t.Errorf("nil Interval = %v", l.Interval())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		1023:                   "1023 B",
		1024:                   "1.0 KB",
		43256521:               "41.3 MB",
		5 * 1024 * 1024 * 1024: "5.0 GB",
	}
	for n, want := range cases {
		got := HumanBytes(n)
		if got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
