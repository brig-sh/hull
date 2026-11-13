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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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

// A caller asks hull what it can do before it runs anything, often from a
// fresh account or a sandbox. The answer must not cost a consent prompt or
// leave a file behind, so every spelling runs through the root command, with
// its Before hook, against an empty home and store, and both stay empty.
func TestVersionWritesNothing(t *testing.T) {
	// Telemetry left on, so a write it would make is not hidden by the
	// environment the test happens to run in. Nothing listens on the
	// endpoint.
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("HULL_TELEMETRY_DISABLED", "")
	t.Setenv("HULL_TELEMETRY_SUPPRESS", "")
	t.Setenv("HULL_TELEMETRY_ENDPOINT", "http://127.0.0.1:1")
	prevArgs := os.Args
	t.Cleanup(func() { os.Args = prevArgs })

	spellings := [][]string{
		{"version", "--json"},
		{"version"},
		{"--version"},
	}
	stores := []bool{false}
	if runtime.GOOS == "darwin" {
		// The default store keeps telemetry state in the home directory,
		// and any other --store-dir keeps it inside the store, so both
		// are places to look.
		stores = append(stores, true)
	}
	for _, spelling := range spellings {
		for _, ownStore := range stores {
			name := strings.Join(spelling, " ")
			if ownStore {
				name += " with --store-dir"
			}
			t.Run(name, func(t *testing.T) {
				runVersionInEmptyHome(t, spelling, ownStore)
			})
		}
	}
}

// runVersionInEmptyHome runs `hull <spelling>` from the root command with an
// empty home, and an empty store of its own when ownStore is set, and fails
// when either is no longer empty afterwards.
func runVersionInEmptyHome(t *testing.T, spelling []string, ownStore bool) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dirs := []string{home}
	args := []string{"hull"}
	if ownStore {
		store := t.TempDir()
		dirs = append(dirs, store)
		args = append(args, "--store-dir", store)
	}
	args = append(args, spelling...)
	// initTelemetry reads the command from os.Args, not from what Run is
	// given.
	os.Args = args
	out := captureVersionStdout(t, func() {
		if err := newApp().Run(context.Background(), args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	})
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("%v wrote %s", args, filepath.Join(dir, e.Name()))
		}
	}
	if args[len(args)-1] != "--json" {
		return
	}
	var got struct {
		Version      *string `json:"version"`
		Capabilities struct {
			Gateway map[string]bool `json:"gateway"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("hull version --json printed %q: %v", out, err)
	}
	if got.Version == nil {
		t.Errorf("hull version --json has no version: %s", out)
	}
	for _, key := range []string{"api", "egressDefault", "divert", "divertLinkLocal"} {
		if _, ok := got.Capabilities.Gateway[key]; !ok {
			t.Errorf("hull version --json has no capabilities.gateway.%s: %s", key, out)
		}
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
