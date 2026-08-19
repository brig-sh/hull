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

// The switch has to actually switch, and a typo must not disable the filter.
func TestTerminalFilterSwitch(t *testing.T) {
	for _, tc := range []struct {
		value    string
		disabled bool
	}{
		{"", false},
		{"off", true},
		{"OFF", true},
		{"0", true},
		{"none", true},
		{"false", true},
		{"  off  ", true},
		{"on", false},
		{"1", false},
		{"yes", false},
		{"of", false},   // a typo leaves it ON
		{"offf", false}, // so does this
		{"true", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(TerminalFilterEnv, tc.value)
			if got := terminalFilterDisabled(); got != tc.disabled {
				t.Errorf("%s=%q disabled=%v, want %v", TerminalFilterEnv, tc.value, got, tc.disabled)
			}
			// And the text helper must agree, so nobody ends up with half the
			// filter running.
			raw := "boom \x1b]52;c;?\x07 done"
			out := sanitizeGuestTextIfEnabled(raw)
			if tc.disabled && out != raw {
				t.Errorf("the filter was off but text was still sanitised: %q", out)
			}
			if !tc.disabled && out == raw {
				t.Errorf("the filter was on but text passed through unchanged: %q", out)
			}
		})
	}
}
