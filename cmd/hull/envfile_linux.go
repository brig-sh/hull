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

//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// runEnv returns the environment a run adds to the image's: the variables
// of the --env-file files, in order, then the --env entries, with the last
// value given for a variable winning, as docker run has it.
//
// The duplicates are dropped here because oci.WithEnv appends every entry
// for a variable the image does not set, so a variable set in a file and
// again with --env would reach the guest twice.
func runEnv(files, entries []string, lookup func(string) (string, bool)) ([]string, error) {
	var env []string
	for _, path := range files {
		vars, err := readEnvFile(path, lookup)
		if err != nil {
			return nil, err
		}
		env = append(env, vars...)
	}
	vars, err := resolveEnvEntries(entries, lookup)
	if err != nil {
		return nil, err
	}
	return lastEnvWins(append(env, vars...)), nil
}

// readEnvFile reads an --env-file with docker run's rules. A line that is
// empty, or whose first character other than white space is #, is skipped.
// KEY=VALUE is taken as written once its leading white space is gone:
// VALUE keeps its quotes and its trailing white space. A bare KEY is
// inherited from lookup, and dropped when lookup has no value for it, as a
// bare --env KEY is, so a file can name a secret without holding it.
//
// A key with white space in it, a line with no key, and a line that is not
// UTF-8 are refused, with the line's number, as docker refuses them.
func readEnvFile(path string, lookup func(string) (string, bool)) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--env-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	var vars []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Bytes()
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("--env-file %s: line %d is not UTF-8", path, n)
		}
		if n == 1 {
			raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		}
		line := strings.TrimLeftFunc(string(raw), unicode.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		if key == "" {
			return nil, fmt.Errorf("--env-file %s: line %d has no variable name", path, n)
		}
		if strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("--env-file %s: line %d: variable %q contains white space", path, n, key)
		}
		if hasValue {
			vars = append(vars, key+"="+value)
			continue
		}
		value, ok := lookup(key)
		if ok {
			vars = append(vars, key+"="+value)
		}
	}
	err = sc.Err()
	if err != nil {
		return nil, fmt.Errorf("--env-file %s: %w", path, err)
	}
	return vars, nil
}

// lastEnvWins keeps one KEY=VALUE entry per variable: the last one, at the
// place of the first.
func lastEnvWins(entries []string) []string {
	at := make(map[string]int, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		i, seen := at[key]
		if seen {
			out[i] = entry
			continue
		}
		at[key] = len(out)
		out = append(out, entry)
	}
	return out
}
