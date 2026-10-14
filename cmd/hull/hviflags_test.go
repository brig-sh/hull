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
	"os"
	"path/filepath"
	"testing"
)

// The pinned hvi (890f9e0) refuses --checkpoint-root as an unknown boot
// argument and so would refuse the whole boot; run must leave the three
// control flags off for it and pass them to an hvi that takes them.
func TestHviBootTakesControlFlags(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// What the hvi at 890f9e0 and at bf3b1aa print, verbatim.
	pinned := write("pinned", `echo 'hvi: unknown boot arg "--checkpoint-root"' >&2; exit 1`)
	current := write("current", `echo 'hvi: --checkpoint-root needs a directory' >&2; exit 1`)
	// The probe must ask about --checkpoint-root alone, with nothing that
	// could boot a guest; any other argv reads here as an hvi without it.
	asked := write("asked", `[ "$1 $2 $#" = "boot --checkpoint-root 2" ] || { echo 'hvi: unknown boot arg "--checkpoint-root"' >&2; exit 1; }
echo 'hvi: --checkpoint-root needs a directory' >&2; exit 1`)

	if takes, err := hviBootTakesControlFlags(pinned); err != nil || takes {
		t.Errorf("the pinned hvi: takes=%v err=%v", takes, err)
	}
	if takes, err := hviBootTakesControlFlags(current); err != nil || !takes {
		t.Errorf("an hvi with the flags: takes=%v err=%v", takes, err)
	}
	if takes, err := hviBootTakesControlFlags(asked); err != nil || !takes {
		t.Errorf("probe argv: takes=%v err=%v", takes, err)
	}
	if _, err := hviBootTakesControlFlags(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing binary was not reported")
	}
}
