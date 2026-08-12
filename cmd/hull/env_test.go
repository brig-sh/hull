// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"reflect"
	"testing"
)

func TestResolveEnvEntries(t *testing.T) {
	host := map[string]string{
		"TOKEN":     "s3cret",
		"EMPTY_SET": "",
	}
	lookup := func(k string) (string, bool) {
		v, ok := host[k]
		return v, ok
	}

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "literal is passed through untouched",
			in:   []string{"FOO=bar"},
			want: []string{"FOO=bar"},
		},
		{
			name: "bare name inherits the host value",
			in:   []string{"TOKEN"},
			want: []string{"TOKEN=s3cret"},
		},
		{
			name: "bare name that is unset is dropped, not emptied",
			in:   []string{"ABSENT"},
			want: []string{},
		},
		{
			name: "bare name set to the empty string is still forwarded",
			in:   []string{"EMPTY_SET"},
			want: []string{"EMPTY_SET="},
		},
		{
			name: "an explicitly empty literal is preserved",
			in:   []string{"FOO="},
			want: []string{"FOO="},
		},
		{
			// Only the first `=` separates name from value, so credential
			// helpers and config snippets survive intact.
			name: "value may contain equals signs and spaces",
			in:   []string{`HELPER=!f() { test "$1" = get; }; f`},
			want: []string{`HELPER=!f() { test "$1" = get; }; f`},
		},
		{
			name: "forms may be mixed and order is preserved",
			in:   []string{"A=1", "TOKEN", "ABSENT", "B=2"},
			want: []string{"A=1", "TOKEN=s3cret", "B=2"},
		},
		{
			name: "no entries yields no entries",
			in:   nil,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEnvEntries(tc.in, lookup)
			if err != nil {
				t.Fatalf("resolveEnvEntries(%q) returned error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resolveEnvEntries(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAppendEnvDefault(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		key   string
		value string
		want  []string
	}{
		{
			name: "absent key gets the default",
			in:   []string{"FOO=bar"}, key: "TERM", value: "xterm",
			want: []string{"FOO=bar", "TERM=xterm"},
		},
		{
			// The regression this guards: --env TERM=dumb used to be followed
			// by the host TERM, and the later entry wins in the guest.
			name: "an explicit value is never shadowed",
			in:   []string{"TERM=dumb"}, key: "TERM", value: "xterm",
			want: []string{"TERM=dumb"},
		},
		{
			name: "an explicitly empty value is still the caller's choice",
			in:   []string{"TERM="}, key: "TERM", value: "xterm",
			want: []string{"TERM="},
		},
		{
			name: "no default to apply",
			in:   []string{"FOO=bar"}, key: "TERM", value: "",
			want: []string{"FOO=bar"},
		},
		{
			// TERMINAL must not be mistaken for TERM.
			name: "a longer key sharing the prefix does not count",
			in:   []string{"TERMINAL=vt100"}, key: "TERM", value: "xterm",
			want: []string{"TERMINAL=vt100", "TERM=xterm"},
		},
		{
			name: "empty entries get the default",
			in:   nil, key: "TERM", value: "xterm",
			want: []string{"TERM=xterm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendEnvDefault(tc.in, tc.key, tc.value)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("appendEnvDefault(%q, %q, %q) = %q, want %q",
					tc.in, tc.key, tc.value, got, tc.want)
			}
		})
	}
}

func TestResolveEnvEntriesRejectsEmptyName(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, in := range []string{"", "=value"} {
		if _, err := resolveEnvEntries([]string{in}, lookup); err == nil {
			t.Errorf("resolveEnvEntries(%q) succeeded, want an error", in)
		}
	}
}

// A bare name must never leak the value into the returned slice's key half,
// and an inherited secret must not be reachable when the variable is absent —
// this is the property the flag exists for.
func TestResolveEnvEntriesDoesNotInventValues(t *testing.T) {
	lookup := func(string) (string, bool) { return "should-not-be-used", false }
	got, err := resolveEnvEntries([]string{"SECRET"}, lookup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %q, want nothing forwarded when the variable is unset", got)
	}
}
