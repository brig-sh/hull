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

//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/brig-sh/hull/internal/buildinfo"
	"github.com/urfave/cli/v3"
)

// hull version names the build the binary came from rather than a stamped
// string: the version Go derived from the nearest tag, then the commit and
// the rest of the build in parentheses.
func TestVersionPrintsTheBuild(t *testing.T) {
	out := captureVersionStdout(t, func() {
		if err := versionCommand().Run(context.Background(), []string{"version"}); err != nil {
			t.Fatalf("hull version: %v", err)
		}
	})
	want := "hull " + buildinfo.Read().String() + "\n"
	if out != want {
		t.Fatalf("hull version printed %q, want %q", out, want)
	}
}

// --version and the subcommand answer one question, so they answer it with
// one line. urfave's own printer would write "hull version <v>", which reads
// oddly once the version carries a parenthesised build.
func TestBothVersionSpellingsAgree(t *testing.T) {
	sub := captureVersionStdout(t, func() {
		if err := versionCommand().Run(context.Background(), []string{"version"}); err != nil {
			t.Fatalf("hull version: %v", err)
		}
	})
	// Asserted before it is called. urfave's default printer dereferences the
	// command it is given, so a version.go that forgot to install ours would
	// fail this test as a segfault rather than as the thing that is wrong.
	if reflect.ValueOf(cli.VersionPrinter).Pointer() != reflect.ValueOf(printVersion).Pointer() {
		t.Fatal("`hull --version` does not go through printVersion; see the init in version.go")
	}
	flag := captureVersionStdout(t, func() { cli.VersionPrinter(nil) })
	if sub != flag {
		t.Fatalf("`hull version` printed %q and `hull --version` printed %q", sub, flag)
	}
}

// The JSON form carries the commit in full, and leaves commit and commitTime
// out rather than empty when the build had no VCS data -- so a consumer tests
// for the key instead of recognising a zero time.
func TestVersionJSONCarriesTheBuild(t *testing.T) {
	info := buildinfo.Read()
	blob, err := json.Marshal(versionData(info))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"version", "modified", "goVersion", "os", "arch"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the JSON has no %q", key)
		}
	}
	if info.Commit == "" {
		if _, ok := got["commit"]; ok {
			t.Error("a build with no VCS data still reported a commit")
		}
		if _, ok := got["commitTime"]; ok {
			t.Error("a build with no VCS data still reported a commit time")
		}
		return
	}
	if got["commit"] != info.Commit {
		t.Errorf("commit = %v, want the full revision %s", got["commit"], info.Commit)
	}
}

// captureVersionStdout runs fn with stdout redirected, and returns what it
// wrote. The version printers write to os.Stdout directly, which is what a
// person reads, so the file is swapped rather than a writer injected.
func captureVersionStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
