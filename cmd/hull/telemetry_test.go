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
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/brig-sh/hull/pkg/telemetry"
	"github.com/urfave/cli/v3"
)

func TestSendEndOnceIsIdempotent(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &store.InstanceState{
		ID:        "end-once",
		Backend:   "vz",
		StartTime: time.Now().Add(-5 * time.Minute),
	}
	if _, err := s.CreateInstance(st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}

	// telemetryClient is nil here, so Send is a no-op; we assert the
	// persisted guard, which is the exactly-once contract.
	sendEndOnce(s, st)
	if !st.TelemetryEndSent {
		t.Fatal("first sendEndOnce must set the guard")
	}
	reloaded, err := s.GetInstance(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TelemetryEndSent {
		t.Fatal("the guard must be persisted, not just in-memory")
	}

	// A second call (retried stop, or foreground exit after stop) must
	// not re-fire: it returns before touching state.
	before := reloaded.TelemetryEndSent
	sendEndOnce(s, reloaded)
	if reloaded.TelemetryEndSent != before {
		t.Fatal("second sendEndOnce must be a no-op")
	}
}

func TestErrorClassIsCoarseAndStable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "canceled"},
		{"not found", fmt.Errorf("open x: %w", os.ErrNotExist), "not-found"},
		{"permission", fmt.Errorf("open y: %w", os.ErrPermission), "permission"},
		{"network", &net.OpError{Op: "dial", Err: fmt.Errorf("refused")}, "network"},
		{"anything else", fmt.Errorf("instance not found: /Users/someone/secret"), "other"},
	}
	for _, tc := range cases {
		if got := errorClass(tc.err); got != tc.want {
			t.Errorf("%s: errorClass = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKnownCommandNeverSendsWhatTheUserTyped(t *testing.T) {
	root := &cli.Command{Commands: []*cli.Command{
		{Name: "run"},
		{Name: "ps", Aliases: []string{"ls"}},
	}}
	cases := []struct{ token, want string }{
		{"run", "run"},
		{"ls", "ls"},
		{"", ""},
		{"ubuntu:latest", unknownCommand},
		{"ghcr.io/example/private-thing:v1", unknownCommand},
		{"rnu", unknownCommand},
	}
	for _, tc := range cases {
		if got := knownCommand(root, tc.token); got != tc.want {
			t.Errorf("knownCommand(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

// The check above is only worth something if initTelemetry uses it: the
// command an event names is what initTelemetry records.
func TestInitTelemetryNamesOnlyARealCommand(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "1")
	prevArgs, prevClient, prevName := os.Args, telemetryClient, telemetryCmdName
	t.Cleanup(func() { os.Args, telemetryClient, telemetryCmdName = prevArgs, prevClient, prevName })
	root := &cli.Command{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "store-dir", Value: t.TempDir()},
			&cli.BoolFlag{Name: "unattended"},
			&cli.BoolFlag{Name: "dnt"},
		},
		Commands: []*cli.Command{{Name: "run"}},
	}
	for token, want := range map[string]string{"run": "run", "ghcr.io/example/private-thing:v1": unknownCommand} {
		os.Args = []string{"hull", token}
		initTelemetry(root)
		if telemetryCmdName != want {
			t.Errorf("hull %s: command = %q, want %q", token, telemetryCmdName, want)
		}
	}
}

// A crash between reading --hypervisor and validating it would otherwise
// queue whatever was written there.
func TestCrashReportCarriesOnlyAKnownBackend(t *testing.T) {
	t.Setenv(telemetry.EnvDisabled, "")
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv(telemetry.EnvSuppress, "")
	dir := t.TempDir()
	prevClient, prevBackend := telemetryClient, telemetryBackend
	t.Cleanup(func() { telemetryClient, telemetryBackend = prevClient, prevBackend })
	telemetryClient = telemetry.Init(telemetry.Config{StoreDir: dir, Version: "0.0.0-test"})
	if !telemetryClient.Enabled() {
		t.Fatal("test telemetry client is disabled; it would assert nothing")
	}
	telemetryBackend = "ghcr.io/example/private-thing:v1"

	queueCrash("boom", []byte("goroutine 1 [running]:\n"))

	files, _ := filepath.Glob(filepath.Join(dir, "crashes", "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one queued report, got %v", files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private-thing") || !strings.Contains(string(body), `"backend":"unknown"`) {
		t.Fatalf("the report carries the raw backend:\n%s", body)
	}
}

// The default store is a mountpoint, so its telemetry state lives next to it.
// Any other --store-dir keeps its state inside, as it always has.
func TestTelemetryStateLivesOutsideTheDefaultStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	custom := t.TempDir()
	t.Chdir(home)
	for store, want := range map[string]string{
		filepath.Join(home, ".hull", "store"):       filepath.Join(home, ".hull"),
		filepath.Join(home, ".hull", "store") + "/": filepath.Join(home, ".hull"),
		".hull/./store": filepath.Join(home, ".hull"),
		custom:          custom,
	} {
		root := &cli.Command{Flags: []cli.Flag{&cli.StringFlag{Name: "store-dir", Value: store}}}
		if got := telemetryStateDir(root); got != want {
			t.Errorf("--store-dir %s: state in %q, want %q", store, got, want)
		}
	}
}
