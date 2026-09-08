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
// Release-gate fuzzing. Panic-freedom on arbitrary input comes for free with
// any fuzz target; every target here also asserts a property of the value the
// function under test returned:
//
//   - compose exec argv: the parse must agree, field for field, with an
//     independent statement of the same contract;
//   - restart policy: String() must re-parse to the value it rendered;
//   - env entries: every resolved entry stays well-formed, and explicit
//     KEY=VALUE entries are never rewritten or reordered;
//   - project and service names: sanitizeName yields one safe path element,
//     idempotently;
//   - service volumes: a named volume resolves under the volumes root, and the
//     guest target passes through unchanged;
//   - mem_limit: a value validateProject accepts never renders --mem <= 0.

package main

import (
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// FuzzParseRestartPolicy pins the round-trip restartPolicy.String() promises:
// "renders the policy in the form the parser accepts, so 'compose config'
// round-trips." A regression that breaks String() for some accepted Mode, or
// makes the parser reject its own rendering, shows up here even though
// neither function alone would panic.
func FuzzParseRestartPolicy(f *testing.F) {
	for _, s := range []string{"", "no", "always", "unless-stopped", "on-failure", "on-failure:3", "on-failure:-1", "on-failure:99999999999999999999", "on-failure:x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := parseRestartPolicy(s)
		if err != nil {
			return
		}
		rendered := p.String()
		p2, err2 := parseRestartPolicy(rendered)
		if err2 != nil {
			t.Fatalf("parseRestartPolicy(%q) = %+v, but String() rendered %q which fails to re-parse: %v", s, p, rendered, err2)
		}
		if p2.Mode != p.Mode || p2.MaxAttempts != p.MaxAttempts {
			t.Fatalf("round-trip broke: parseRestartPolicy(%q) = %+v, String() = %q, re-parse = %+v", s, p, rendered, p2)
		}
	})
}

// composeExecFlagArity states, independently of parseComposeExecArgs, the
// flag set `compose exec` accepts and how many argv elements each flag eats.
// Restating it here is deliberate: a new production flag has to be added here
// too, and the fuzz target below then proves the new flag is parsed the way
// this spec says rather than merely proving it does not panic.
var composeExecFlagArity = map[string]int{
	"-T": 0, "--no-tty": 0,
	"-u": 1, "--user": 1,
	"-w": 1, "--workdir": 1,
	"-e": 1, "--env": 1,
}

// composeExecSpec is a second, independent implementation of the contract
// parseComposeExecArgs documents: scan flags left to right, stop at the first
// element that is not a flag or just past a lone "--", and treat everything
// from there on as SERVICE COMMAND [ARGS...] with one "--" removed between
// SERVICE and COMMAND. It returns the expected options, the index the flag
// scan stops at, whether the separator strip applies, and whether the input
// parses at all.
//
// It reads argv and never writes to it. parseComposeExecArgs writes through
// its caller's backing array (see FuzzParseComposeExecArgs), so the spec must
// be fed a snapshot taken before that call, never the array the parser
// touched.
func composeExecSpec(argv []string) (want composeExecOpts, stop int, stripped, ok bool) {
scan:
	for stop = 0; stop < len(argv); stop++ {
		a := argv[stop]
		switch {
		case a == "-h" || a == "--help":
			return composeExecOpts{}, 0, false, false // usage is returned as an error
		case a == "--":
			stop++ // the separator itself is consumed
			break scan
		case a == "-" || !strings.HasPrefix(a, "-"):
			break scan // the first positional: SERVICE
		}
		arity, known := composeExecFlagArity[a]
		if !known {
			return composeExecOpts{}, 0, false, false // unknown flag
		}
		if arity == 0 {
			want.noTTY = true
			continue
		}
		if stop+1 >= len(argv) {
			return composeExecOpts{}, 0, false, false // flag needs a value
		}
		v := argv[stop+1] // taken verbatim, even when it looks like a flag or "--"
		switch a {
		case "-u", "--user":
			want.user = v
		case "-w", "--workdir":
			want.workdir = v
		case "-e", "--env":
			want.env = append(want.env, v)
		}
		stop++
	}
	tail := argv[stop:]
	if len(tail) >= 2 && tail[1] == "--" {
		stripped = true
		want.rest = append([]string{tail[0]}, tail[2:]...)
	} else {
		want.rest = slices.Clone(tail)
	}
	return want, stop, stripped, true
}

// FuzzParseComposeExecArgs pins the whole parse of `compose exec` argv --
// every field, plus accept/reject -- against composeExecSpec above.
//
// The check this replaces only asked whether o.rest was *some* tail of argv,
// which is vacuously satisfiable. Two mutations of exec_compose.go were
// observed passing it before this change, and both fail now:
//   - `o.rest = argv[i:]` replaced by `o.rest = nil`: the empty tail is a
//     tail, so the old check was green while the parser returned nothing;
//   - an off-by-one that slices one element late and silently drops the
//     SERVICE name: a shorter tail is still a tail. This is the exact
//     "mis-slice on adversarial argv" the target exists to catch.
//
// Three further checks ride along:
//   - reconstruction: argv must rebuild from the flag prefix, the stripped
//     separator and rest, so nothing can go missing between them;
//   - SERVICE identity: rest[0] must be the exact argv element the flag scan
//     stopped on, byte for byte;
//   - blast radius of a known bug: parseComposeExecArgs strips the separator
//     in place (o.rest = append(o.rest[:1], o.rest[2:]...)), so it writes
//     through the caller's backing array. For argv ["web","--","ls","-al"]
//     the caller's array becomes ["web","ls","-al","-al"] -- verified, not
//     assumed. That is a real latent bug; it is reported, not fixed here,
//     because this target may not change production code. So the target
//     snapshots argv before the call (comparing against the mutated array
//     would compare against corrupted data, not against the input as given)
//     and asserts only the bound that does hold today: the in-place write
//     never reaches back over the flag prefix or the SERVICE name. It
//     deliberately does not assert that argv is unmodified, which is false.
func FuzzParseComposeExecArgs(f *testing.F) {
	for _, s := range []string{
		"web|/bin/sh",
		"-T|web|--|ls|-al",
		"-u|root|-e|A=1|-w|/tmp|web|env",
		"-e",  // dangling value flag
		"--",  // separator with nothing after it
		"web", // SERVICE and no command: parses here, composeExec rejects it
		"",    // one empty element is a positional, not a flag
		"-",   // bare "-" is a SERVICE name, not a flag
		"--|--|ls",
		"-u|--|web|ls", // a value-taking flag swallows "--" verbatim
		"-u|-T|web|ls", // ... and swallows a flag-shaped value verbatim
		"-u|root",      // flags only, empty rest
		"web|--|--|ls", // only ONE separator is stripped
		"--|-T|web",    // after "--", "-T" is the SERVICE, not a flag
		"web|--|ls|-al",
		"web|--",                       // strip leaves a one-element rest
		"-T|-T|-T|web|x",               // repeated boolean flag
		"-e|A=1|-e|B=2|web|--|env",     // -e accumulates, in order
		"-u|a|-u|b|-w|/x|-w|/y|web|ls", // last wins for -u and -w
		"--no-tty|--user|root|--workdir|/tmp|--env|A=1|web|--|sh", // long forms
		"-h",
		"-z|web",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, joined string) {
		argv := strings.Split(joined, "|")
		orig := slices.Clone(argv)

		got, err := parseComposeExecArgs(argv)
		want, stop, stripped, ok := composeExecSpec(orig)

		if (err == nil) != ok {
			t.Fatalf("parseComposeExecArgs(%q): err=%v, but the spec says accepted=%v", orig, err, ok)
		}
		if err != nil {
			return // rejected by both; the partly filled opts are not a promise
		}
		if got.noTTY != want.noTTY || got.user != want.user || got.workdir != want.workdir ||
			!slices.Equal(got.env, want.env) || !slices.Equal(got.rest, want.rest) {
			t.Fatalf("parseComposeExecArgs(%q) = {noTTY:%v user:%q workdir:%q env:%q rest:%q}, spec says {noTTY:%v user:%q workdir:%q env:%q rest:%q}",
				orig,
				got.noTTY, got.user, got.workdir, got.env, got.rest,
				want.noTTY, want.user, want.workdir, want.env, want.rest)
		}
		if len(got.rest) > 0 && got.rest[0] != orig[stop] {
			t.Fatalf("parseComposeExecArgs(%q): rest[0] = %q but the flag scan stopped on %q: the SERVICE name was not preserved",
				orig, got.rest[0], orig[stop])
		}
		// Nothing may be dropped silently: argv has to come back out of the
		// pieces the parse claims to have split it into.
		recon := make([]string, 0, len(orig))
		recon = append(recon, orig[:stop]...)
		if stripped {
			recon = append(recon, got.rest[0], "--")
			recon = append(recon, got.rest[1:]...)
		} else {
			recon = append(recon, got.rest...)
		}
		if !slices.Equal(recon, orig) {
			t.Fatalf("parseComposeExecArgs(%q): flag prefix %q + rest %q (separator stripped=%v) rebuilds to %q, not the input",
				orig, orig[:stop], got.rest, stripped, recon)
		}
		// The in-place strip must not reach back over the flags or the
		// SERVICE name. See the blast-radius note on this function.
		bound := stop + 1
		if bound > len(orig) {
			bound = len(orig)
		}
		if !slices.Equal(argv[:bound], orig[:bound]) {
			t.Fatalf("parseComposeExecArgs(%q) overwrote the caller's argv at or before the SERVICE name: argv[:%d] = %q, was %q",
				orig, bound, argv[:bound], orig[:bound])
		}
	})
}

// FuzzResolveEnvEntries covers the bare --env KEY inheritance path. Beyond
// "every resolved entry has an '=' " (a malformed entry would reach the
// guest's exec.Cmd.Env broken), it asserts resolveEnvEntries never rewrites
// or reorders an explicit KEY=VALUE entry: only bare keys are ever filled in
// (from lookup) or dropped.
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
		var explicit []string
		for _, e := range entries {
			if strings.Contains(e, "=") {
				explicit = append(explicit, e)
			}
		}
		j := 0
		for _, o := range out {
			if j < len(explicit) && o == explicit[j] {
				j++
			}
		}
		if j != len(explicit) {
			t.Fatalf("resolveEnvEntries(%q) = %v: explicit KEY=VALUE entries %v are not preserved in order", entries, out, explicit)
		}
	})
}

// FuzzSanitizeName pins the property the store depends on: whatever comes
// in, what comes out is one safe, non-empty path element, and re-sanitizing
// that output is a no-op (projectName can feed an already-sanitized name
// back in, e.g. via --project-name on a second invocation).
func FuzzSanitizeName(f *testing.F) {
	for _, s := range []string{"web", "WEB", "../../etc", "a/b", "", "..", "\x00", "ünïcødé", strings.Repeat("x", 4096)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := sanitizeName(s)
		if got == "" || got != filepath.Base(got) || strings.ContainsAny(got, "/\\") || got == ".." || got == "." {
			t.Fatalf("sanitizeName(%q) = %q: not a safe path element", s, got)
		}
		if again := sanitizeName(got); again != got {
			t.Fatalf("sanitizeName(%q) = %q is not idempotent: sanitizeName(%q) = %q", s, got, got, again)
		}
	})
}

// FuzzResolveServiceVolume asserts the containment promise: a named volume
// must resolve under the volumes root, never outside it, and the guest
// Target passes through unchanged. Unlike the bespoke resolveVolumeEntry
// this replaces, the source is no longer a raw string to split —
// compose-go's schema already restricts a declared volume name to
// ^[a-zA-Z0-9._-]+$ before resolveServiceVolume ever sees it — so the fuzz
// target exercises that safety property directly over an arbitrary Source.
func FuzzResolveServiceVolume(f *testing.F) {
	for _, s := range []string{"pgdata", "my-vol.2", "../esc", "a/b", "", "~", ".", "a", "_", ".."} {
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
		if !strings.HasSuffix(got, ":/data") {
			t.Fatalf("named volume %q resolved to %q: guest target /data was not passed through unchanged", source, got)
		}
	})
}

// FuzzServiceMemLimit fuzzes the mem_limit scalar through the parser
// compose-go now uses in its place: types.UnitBytes.DecodeMapstructure is
// the same parseString that backs UnmarshalYAML on the real compose.Load
// path, so this exercises the production parser directly rather than
// round-tripping a YAML file per iteration. It then runs the result through
// hull's own gate: validateProject's floor check, and serviceRunArgs's
// byte-to-MB conversion for the guest VM request.
//
// This replaces the orphaned FuzzParseMemLimit corpus (no such target has
// existed since before this file's own history): its one seed,
// "9007199270000000g", is migrated below. Exercised through the real
// compose.Load + validateProject path on this host (darwin/arm64), that
// value produces no error and no warning; svc.MemLimit saturates to
// math.MaxInt64 (float64->int64 conversion overflow in
// github.com/docker/go-units's RAMInBytes, which compose-go's UnitBytes
// delegates to). validateProject's floor check (compose.go:590) only
// rejects a mem_limit BELOW 1 MiB ("a value under 1 MiB floors to 0 ...
// Reject it here"); there is no ceiling check, so this seed exercises an
// accepted-but-extreme path, not a rejected one. That gap is reported
// separately, not asserted here: this target does not claim an upper bound
// the production code never promises.
func FuzzServiceMemLimit(f *testing.F) {
	for _, s := range []string{
		"", "0", "-1", "1", "512k", "1048575", "1048576", "1048577",
		"1g", "1G", "1Gi", "1GiB", "abc", " ", "1.5g", "-0.5g",
		"9007199270000000g", // migrated from the orphan FuzzParseMemLimit corpus
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, memLimit string) {
		var u types.UnitBytes
		if err := u.DecodeMapstructure(memLimit); err != nil {
			return // compose-go's own loader rejects the file at this point
		}
		svc := types.ServiceConfig{Name: "web", Image: "img:1", MemLimit: u}
		p := &types.Project{Name: "proj", Services: types.Services{"web": svc}}
		if err := validateProject(p, "", io.Discard); err != nil {
			return // the floor check rejected it: nothing left to check
		}
		got := p.Services["web"]
		if got.MemLimit <= 0 {
			return // serviceRunArgs never emits --mem for a non-positive limit
		}
		args, err := serviceRunArgs("web", "web", got, serviceLaunch{
			gatewaySock: "/tmp/gw.sock", ip: "10.0.2.2", maskBits: 24,
		})
		if err != nil {
			t.Fatalf("serviceRunArgs(%q) errored after validateProject accepted MemLimit=%d: %v", memLimit, got.MemLimit, err)
		}
		for i, a := range args {
			if a != "--mem" {
				continue
			}
			mb, convErr := strconv.Atoi(args[i+1])
			if convErr != nil {
				t.Fatalf("mem_limit %q: --mem value %q is not an integer", memLimit, args[i+1])
			}
			if mb <= 0 {
				// Exactly the failure validateProject's floor check exists to
				// prevent (compose.go:579-591): a mem_limit that passed
				// validation must never produce a 0-or-negative-MB --mem,
				// which run.go turns into MemSizeB: uint64(mb)*1024*1024, a
				// zero-memory VM request Virtualization.framework rejects
				// with an opaque error far from the compose file that caused
				// it.
				t.Fatalf("mem_limit %q passed validateProject (MemLimit=%d bytes) but rendered --mem %d", memLimit, got.MemLimit, mb)
			}
		}
	})
}
