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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// newTeardownFixture builds a real store on a temp dir and saves a project
// state file for it, so teardownProject's final os.Remove(projectStatePath)
// has a real file to delete. SwitchPID is left 0 so stopGatewayDaemon returns
// before it touches /bin/ps (its guard is SwitchPID > 0 at compose.go), and
// File points at a path that does not exist, matching the other compose tests.
// The runner is passed in per test; nothing here re-execs a binary.
func newTeardownFixture(t *testing.T, order []string, services map[string]string) (*store.Store, *composeProject) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := &composeProject{
		Name:      "proj",
		File:      filepath.Join(t.TempDir(), "does-not-exist.yaml"),
		Order:     order,
		Services:  services,
		SwitchPID: 0,
	}
	if err := saveProject(s, proj); err != nil {
		t.Fatal(err)
	}
	return s, proj
}

// recordingRunner returns a run adapter that appends a copy of each call's
// argv to *calls and reports success, so a test can assert exactly which
// stop/rm commands teardownProject issued, in order.
func recordingRunner() (run func(args ...string) (string, error), calls *[][]string) {
	var rec [][]string
	run = func(args ...string) (string, error) {
		rec = append(rec, append([]string(nil), args...))
		return "", nil
	}
	return run, &rec
}

// TestTeardownOrderIsReverseOfStartupOrder pins the stop order of the failed-up
// unwind against the only written stop-order contract in the repo,
// docs/storage.md:243: "`hull compose down` | `stop` then `rm` for each service
// in reverse start order [...]". composeUp's unwind used to walk Order forward;
// nothing documents the order for a failed `up`, and this test freezes it to
// the same reverse order `down` uses.
//
// Regression caught: teardownOrder collapsing back to a forward walk (for i :=
// 0; i < len(proj.Order); i++). That mutation returns [db, cache, app] for the
// full case, and the reflect.DeepEqual below fails.
func TestTeardownOrderIsReverseOfStartupOrder(t *testing.T) {
	order := []string{"db", "cache", "app"}

	// All three services have a recorded instance: exact reverse by value.
	proj := &composeProject{
		Order: order,
		Services: map[string]string{
			"db":    "proj-db",
			"cache": "proj-cache",
			"app":   "proj-app",
		},
	}
	got := teardownOrder(proj)
	want := []string{"proj-app", "proj-cache", "proj-db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("teardownOrder = %v, want reverse start order %v", got, want)
	}

	// Only two of the three ever recorded an instance: the middle one is
	// absent from the result and the surviving two stay reversed.
	proj.Services = map[string]string{
		"db":  "proj-db",
		"app": "proj-app",
		// "cache" deliberately has no instance: in Order, never started.
	}
	got = teardownOrder(proj)
	want = []string{"proj-app", "proj-db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("teardownOrder = %v, want cache absent, rest reversed %v", got, want)
	}
}

// TestTeardownProjectSkipsServicesWithNoRecordedInstance drives the extracted
// unwind with a recording runner and asserts, by exact slice equality, that it
// issues stop+rm only for services that recorded an instance, newest-started
// first, and nothing for the service still in Order but never started.
//
// Regression caught: deleting the "ok" guard in teardownOrder (or in the loop),
// so a missing map lookup yields the zero value "" and teardownProject calls
// run("stop", "") / run("rm", "") for the never-started service. The exact
// slice comparison below catches those two extra empty-instance entries that a
// substring check on the trace would miss. The trailing state-file assertion
// proves the body ran to its final os.Remove, not just the loop.
func TestTeardownProjectSkipsServicesWithNoRecordedInstance(t *testing.T) {
	order := []string{"db", "cache", "app"}
	services := map[string]string{
		"db":  "proj-db",
		"app": "proj-app",
		// "cache" deliberately has no instance: in Order, never started.
	}
	s, proj := newTeardownFixture(t, order, services)

	run, calls := recordingRunner()
	teardownProject(s, proj, run, &bytes.Buffer{}, "")

	want := [][]string{
		{"stop", "proj-app"},
		{"rm", "proj-app"},
		{"stop", "proj-db"},
		{"rm", "proj-db"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("runner calls =\n%v\nwant (cache skipped, reverse order)\n%v", *calls, want)
	}

	// The unwind must run to the end and remove the project state file.
	if _, err := os.Stat(projectStatePath(s, proj.Name)); !os.IsNotExist(err) {
		t.Fatalf("project state still present, os.Stat err = %v; want IsNotExist", err)
	}
}

// TestTeardownProjectReportsAFailedRemoval drives the unwind with a runner that
// fails one specific rm call and a bytes.Buffer as the warning writer, and
// asserts the buffer names the instance whose removal failed.
//
// Why this matters: the closure discarded every result with `_, _ =`, so a
// failed unwind was silent, while composeDown reports a failed stop/rm at
// compose.go. Per docs/storage.md:250-251 ("hull rm refuses a running instance
// without --force, and removes nothing on refusal."), a failed stop leaves the
// instance running and rm then refuses it, which is exactly the case that was
// silent. teardownProject now warns on error like composeDown does.
//
// Regression caught: discarding the runner error again (reverting the rm call
// to `_, _ = run("rm", inst)`). That mutation leaves the buffer empty and the
// Contains assertion below fails.
func TestTeardownProjectReportsAFailedRemoval(t *testing.T) {
	order := []string{"db", "app"}
	services := map[string]string{"db": "proj-db", "app": "proj-app"}
	s, proj := newTeardownFixture(t, order, services)

	var warn bytes.Buffer
	run := func(args ...string) (string, error) {
		// Fail exactly the removal of proj-app; everything else succeeds.
		if len(args) == 2 && args[0] == "rm" && args[1] == "proj-app" {
			return "instance is running", os.ErrPermission
		}
		return "", nil
	}
	teardownProject(s, proj, run, &warn, "")

	out := warn.String()
	if !strings.Contains(out, "rm proj-app") {
		t.Fatalf("warning writer = %q, want it to name the failed removal of proj-app", out)
	}
}
