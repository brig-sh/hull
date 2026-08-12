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
	"fmt"
	"strings"
)

// resolveEnvEntries expands the values of a repeatable --env flag into the
// KEY=VALUE strings handed to the guest. Two forms are accepted, matching
// docker:
//
//	KEY=VALUE   used verbatim, including an empty VALUE
//	KEY         inherited from this process's own environment
//
// The bare form is what lets a caller pass a secret without it appearing in
// urunc-macos's argv, where any process on the host can read it out of `ps`.
// The caller exports the variable and names it; the value travels through the
// environment instead of the command line.
//
// A bare KEY that is unset is dropped rather than passed through as empty,
// again following docker: an empty value would shadow whatever the image
// configured for that variable, and "set to nothing" is not the same as "not
// set".
// appendEnvDefault adds key=value to entries unless they already define key, so
// a value the caller passed explicitly is never shadowed.
//
// Order matters here. These entries reach the guest agent, which hands them to
// exec.Cmd.Env, and Go's env dedup keeps the *last* duplicate — so appending
// unconditionally would silently beat an explicit `--env KEY=…` rather than
// defer to it. The prefix test matches urunit-agent's own defaultEnv, which
// applies its defaults the same way.
//
// An empty value is not a default worth setting, and is skipped.
func appendEnvDefault(entries []string, key, value string) []string {
	if value == "" {
		return entries
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, key+"=") {
			return entries
		}
	}
	return append(entries, key+"="+value)
}

func resolveEnvEntries(entries []string, lookup func(string) (string, bool)) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _, hasValue := strings.Cut(entry, "=")
		if name == "" {
			return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE, or KEY to inherit from the environment", entry)
		}
		if hasValue {
			out = append(out, entry)
			continue
		}
		if value, ok := lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out, nil
}
