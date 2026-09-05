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
//
// Release-gate fuzzing. Every target asserts two things: the parser must not
// panic on arbitrary input, and where the code makes a safety promise (an
// instance name is a directory name; a resolved volume stays under the
// volumes root) the promise must hold for every input that parses.

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func FuzzParseRestartPolicy(f *testing.F) {
	for _, s := range []string{"", "no", "always", "unless-stopped", "on-failure", "on-failure:3", "on-failure:-1", "on-failure:99999999999999999999", "on-failure:x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseRestartPolicy(s)
	})
}

// FuzzParseComposeExecArgs: a hand-rolled docker-style parser, the shape
// most likely to mis-slice on adversarial argv.
func FuzzParseComposeExecArgs(f *testing.F) {
	f.Add("web|/bin/sh")
	f.Add("-T|web|--|ls|-al")
	f.Add("-u|root|-e|A=1|-w|/tmp|web|env")
	f.Add("-e")
	f.Add("--")
	f.Fuzz(func(t *testing.T, joined string) {
		argv := strings.Split(joined, "|")
		_, _ = parseComposeExecArgs(argv)
	})
}

// FuzzResolveEnvEntries covers the bare --env KEY inheritance path.
func FuzzResolveEnvEntries(f *testing.F) {
	f.Add("A=1|BARE|=novalue|")
	f.Add("|")
	f.Add("A=b=c")
	lookup := func(k string) (string, bool) {
		if k == "BARE" {
			return "inherited", true
		}
		return "", false
	}
	f.Fuzz(func(t *testing.T, joined string) {
		entries := strings.Split(joined, "|")
		out, err := resolveEnvEntries(entries, lookup)
		if err != nil {
			return
		}
		for _, e := range out {
			if !strings.Contains(e, "=") {
				t.Fatalf("resolved entry %q has no '=': it would reach the guest malformed", e)
			}
		}
	})
}

// FuzzSanitizeName pins the property the store depends on: whatever comes
// in, what comes out is one safe, non-empty path element.
func FuzzSanitizeName(f *testing.F) {
	for _, s := range []string{"web", "WEB", "../../etc", "a/b", "", "..", "\x00", "ünïcødé", strings.Repeat("x", 4096)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := sanitizeName(s)
		if got == "" || got != filepath.Base(got) || strings.ContainsAny(got, "/\\") || got == ".." || got == "." {
			t.Fatalf("sanitizeName(%q) = %q: not a safe path element", s, got)
		}
	})
}

// FuzzResolveServiceVolume asserts the containment promise: a named volume
// must resolve under the volumes root, never outside it. Unlike the bespoke
// resolveVolumeEntry this replaces, the source is no longer a raw string to
// split — compose-go's schema already restricts a declared volume name to
// ^[a-zA-Z0-9._-]+$ before resolveServiceVolume ever sees it — so the fuzz
// target exercises that safety property directly over an arbitrary Source.
func FuzzResolveServiceVolume(f *testing.F) {
	for _, s := range []string{"pgdata", "my-vol.2", "../esc", "a/b", "", "~", "."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, source string) {
		root := "/tmp/volroot"
		got, err := resolveServiceVolume(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: source, Target: "/data"}, root, "proj")
		if err != nil {
			return
		}
		host, _, found := strings.Cut(got, ":")
		if !found || host == "" {
			return
		}
		if !namedVolumeRe.MatchString(source) {
			return
		}
		clean := filepath.Clean(host)
		if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
			t.Fatalf("named volume %q resolved to %q, outside the volumes root %q", source, clean, root)
		}
	})
}
