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
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// SelfExecChildEnv and SelfExecLogEnv are the seam TestMain below uses to
// observe real selfExec calls without parsing anything they print. A test
// sets both with t.Setenv, which selfExec's os.Environ() then hands down to
// the re-exec'd child; the child, being this same test binary, notices the
// sentinel in its own TestMain before any test runs and, instead of running
// tests, records its argv and exits.
//
// Task B (order tests for composeUp's teardown path) reuses this exact seam:
// do not add a second TestMain to this package, there can be only one.
const (
	SelfExecChildEnv = "HULL_TEST_SELFEXEC_CHILD"
	SelfExecLogEnv   = "HULL_TEST_SELFEXEC_LOG"
)

func TestMain(m *testing.M) {
	if os.Getenv(SelfExecChildEnv) != "" {
		logPath := os.Getenv(SelfExecLogEnv)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(1)
		}
		if _, err := f.WriteString(strings.Join(os.Args, "\x1f") + "\n"); err != nil {
			os.Exit(1)
		}
		if err := f.Close(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ReadSelfExecTrace reads back the argv of every intercepted selfExec call,
// in the order the calls happened, one []string per call.
func ReadSelfExecTrace(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read selfExec log: %v", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	trace := make([][]string, 0, len(lines))
	for _, line := range lines {
		trace = append(trace, strings.Split(line, "\x1f"))
	}
	return trace
}

// newComposeDownFixture builds a store directory that ensureStore accepts
// without touching a disk image (a marker file, the same trick
// TestEnsureStoreShortCircuitsOnTheMarker in store_test.go uses), saves a
// project state file with the given startup order and started instances,
// and sets up this test's selfExec interception. It returns the store
// directory and the log file selfExec calls get recorded to.
func newComposeDownFixture(t *testing.T, order []string, services map[string]string) (storeDir, logFile string) {
	t.Helper()
	storeDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(storeDir, storeMarkerName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	proj := &composeProject{
		Name:     "proj",
		File:     filepath.Join(t.TempDir(), "does-not-exist.yaml"),
		Order:    order,
		Services: services,
	}
	if err := saveProject(s, proj); err != nil {
		t.Fatal(err)
	}

	logFile = filepath.Join(t.TempDir(), "selfexec.log")
	t.Setenv(SelfExecChildEnv, "1")
	t.Setenv(SelfExecLogEnv, logFile)
	return storeDir, logFile
}

// runComposeDown drives the real composeDown(ctx, cmd) entry point through
// cli flag parsing, the pattern the rest of this package's compose tests
// use (see runComposeConfigYAML in compose_test.go), so the test exercises
// what a user's 'hull compose down' actually calls.
func runComposeDown(t *testing.T, storeDir, project string) error {
	t.Helper()
	var callErr error
	root := &cli.Command{
		Name: "hull",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "store-dir"},
			&cli.StringFlag{Name: "project-name"},
			&cli.BoolFlag{Name: "debug"},
			&cli.BoolFlag{Name: "volumes"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			callErr = composeDown(ctx, cmd)
			return nil
		},
		Writer: io.Discard,
	}
	if err := root.Run(context.Background(),
		[]string{"hull", "--store-dir", storeDir, "--project-name", project}); err != nil {
		t.Fatal(err)
	}
	return callErr
}

// wantStopRm builds the argv this test expects for one service: the same
// [exe, --store-dir, storeDir, stop|rm, instance] selfExec itself builds
// (compose.go:905-916), using this test binary's own os.Executable() since
// under 'go test' that is exactly the binary selfExec re-execs.
func wantStopRm(t *testing.T, storeDir, instance string) [][]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return [][]string{
		{exe, "--store-dir", storeDir, "stop", instance},
		{exe, "--store-dir", storeDir, "rm", instance},
	}
}

// TestComposeDownStopsServicesInReverseStartupOrder pins the written
// contract at docs/storage.md:243: "`hull compose down` | `stop` then `rm`
// for each service in reverse start order, plus the gateway daemon and the
// project state file. Named volumes stay |".
//
// Regression this catches: the reverse loop at compose.go:1225 collapsing
// to (or being edited into) a forward loop. SwitchPID: 0 keeps
// stopGatewayDaemon from touching /bin/ps, and a File that does not exist
// makes reloadProject fail so the pre_stop block never runs, leaving the
// stop/rm calls as the only thing that reaches selfExec.
func TestComposeDownStopsServicesInReverseStartupOrder(t *testing.T) {
	order := []string{"db", "cache", "app"}
	services := map[string]string{
		"db":    "proj-db",
		"cache": "proj-cache",
		"app":   "proj-app",
	}
	storeDir, logFile := newComposeDownFixture(t, order, services)

	if err := runComposeDown(t, storeDir, "proj"); err != nil {
		t.Fatalf("composeDown: %v", err)
	}

	var want [][]string
	for i := len(order) - 1; i >= 0; i-- {
		want = append(want, wantStopRm(t, storeDir, services[order[i]])...)
	}
	got := ReadSelfExecTrace(t, logFile)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selfExec trace =\n%v\nwant (reverse startup order: app, cache, db)\n%v", got, want)
	}
}

// TestComposeDownSkipsServicesThatNeverStarted covers a project whose Order
// lists a service with no recorded instance (it never made it through 'up',
// e.g. a failed start left the state file's Order ahead of Services).
//
// Regression this catches: deleting the "ok" guard at compose.go:1227-1229.
// Without it, the missing map lookup still yields the zero value ("") for
// instance, and composeDown would call selfExec(cmd, "stop", "") /
// selfExec(cmd, "rm", "") for the never-started service instead of skipping
// it outright — extra entries this test's exact slice comparison catches
// that a substring check on the trace would miss.
func TestComposeDownSkipsServicesThatNeverStarted(t *testing.T) {
	order := []string{"db", "cache", "app"}
	services := map[string]string{
		"db":  "proj-db",
		"app": "proj-app",
		// "cache" deliberately has no instance: it is in Order but never started.
	}
	storeDir, logFile := newComposeDownFixture(t, order, services)

	if err := runComposeDown(t, storeDir, "proj"); err != nil {
		t.Fatalf("composeDown: %v", err)
	}

	want := append(wantStopRm(t, storeDir, "proj-app"), wantStopRm(t, storeDir, "proj-db")...)
	got := ReadSelfExecTrace(t, logFile)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selfExec trace =\n%v\nwant (cache skipped entirely, not stopped/removed with an empty instance)\n%v", got, want)
	}
}
