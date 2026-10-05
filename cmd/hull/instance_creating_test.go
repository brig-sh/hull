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

// --- review finding: the pull-time window reads as corrupt -----------------
//
// run creates the instance directory, then spends the registry pull, the
// unpack and the bundle prep -- several seconds on a cold image -- before it
// writes any record. In that window GetInstance saw a directory with no
// state.json, which is ErrInstanceStateUnreadable, so `ps` showed every
// healthy run as "unreadable": the same word a genuinely corrupt state file
// earns, right next to the documented advice that `hull rm` clears one.
//
// run now writes a pid-less StatusCreating record immediately after
// CreateInstance, before any of that. These tests hold the two halves of the
// fix: the record reads as idle rather than live or broken, and run actually
// writes it before the pull starts.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// Build a pid-less creating record for the direct state and command tests.
func newPullingInstance(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("puller"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID:        "puller",
		Status:    store.StatusCreating,
		StartTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return s, "puller"
}

// GetInstance has to succeed on a "creating" record. Before this fix the
// same directory had no state.json at all, which GetInstance reports as
// ErrInstanceStateUnreadable -- the whole point of writing the record early
// is that readers stop seeing that.
func TestCreatingRecordIsReadable(t *testing.T) {
	s, id := newPullingInstance(t)
	state, err := s.GetInstance(id)
	if err != nil {
		t.Fatalf("a creating record was unreadable: %v", err)
	}
	if state.Status != store.StatusCreating {
		t.Fatalf("status = %q, want %q", state.Status, store.StatusCreating)
	}
	if state.PID != 0 {
		t.Fatalf("a creating record carries pid %d; no VMM exists yet to have one", state.PID)
	}
}

// ps must show "creating", not "unreadable" and not "running": the first
// reads as a corrupt record, which docs/storage.md tells an operator to clear
// with `hull rm`; the second is simply false while the pull is in progress.
func TestPsShowsAPullingInstanceAsCreating(t *testing.T) {
	s, id := newPullingInstance(t)
	row := psRow(t, s, id)
	if !strings.Contains(row, store.StatusCreating) {
		t.Fatalf("pulling instance: %q, want status %q", row, store.StatusCreating)
	}
	if strings.Contains(row, store.StatusUnreadable) {
		t.Fatalf("a pulling instance read as unreadable: %q", row)
	}
	if strings.Contains(row, "running") {
		t.Fatalf("a pulling instance read as running: %q", row)
	}
	// CREATED has to be the time the pull started, not the zero value an
	// unreadable row has nothing better to print.
	if strings.Contains(row, "0001-01-01") {
		t.Fatalf("pulling instance shows the zero time: %q", row)
	}
}

// No VMM is spawned until long after this record is written -- the pull and
// the bundle prep come first -- so nothing may treat it as something that
// could have one. A false positive here is what would make `rm` refuse it
// and `store compact` block on it, exactly the regression the review flagged.
func TestCreatingRecordMayNotHaveAVMM(t *testing.T) {
	inst := &store.InstanceState{ID: "puller", Status: store.StatusCreating, PID: 0}
	if instanceMayHaveVMM(inst) {
		t.Fatal("a creating record, which precedes any spawn, was treated as possibly live")
	}
}

// `store compact` must not refuse to unmount under an instance that is only
// pulling: there is no VMM yet, so there is no root filesystem in use to
// protect.
func TestCompactIgnoresACreatingRecord(t *testing.T) {
	instances := []*store.InstanceState{{ID: "puller", Status: store.StatusCreating, PID: 0}}
	if err := refuseRunningInstances(instances, instanceMayHaveVMM); err != nil {
		t.Fatalf("compaction was refused for an instance that is only pulling: %v", err)
	}
}

// `rm` on a pulling instance has nothing to signal and nothing to protect, so
// it must succeed without --force, exactly as it does for any other idle
// instance.
func TestRemoveClearsACreatingInstanceWithoutForce(t *testing.T) {
	s, id := newPullingInstance(t)
	if err := removeInstanceIn(s, id, false); err != nil {
		t.Fatalf("rm on a pulling instance: %v", err)
	}
	if _, err := s.GetInstance(id); err == nil {
		t.Fatal("the pulling instance survived rm")
	}
}

// `stop` on a pid-less record marks it stopped and returns success. A
// creating record is that shape, and the run still going would overwrite
// the marker and boot. Refuse, and leave the record as the pull wrote it.
func TestStopRefusesACreatingInstance(t *testing.T) {
	s, id := newPullingInstance(t)
	err := stopInstanceIn(s, id, 10)
	if err == nil {
		t.Fatal("stop on a pulling instance succeeded")
	}
	if !strings.Contains(err.Error(), "still being created") {
		t.Fatalf("stop: %v", err)
	}
	state, getErr := s.GetInstance(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if state.Status != store.StatusCreating {
		t.Fatalf("status = %q, want %q", state.Status, store.StatusCreating)
	}
	if state.StoppedByUser {
		t.Fatal("stop marked a pulling instance as stopped by the user")
	}
}

// logs would otherwise open the empty LogFile and report "log file not
// found:" with a blank path.
func TestLogsRefusesACreatingInstance(t *testing.T) {
	s, id := newPullingInstance(t)
	var out bytes.Buffer
	err := logsInstance(s, id, &out, false, -1)
	if err == nil {
		t.Fatal("logs on a pulling instance succeeded")
	}
	if !strings.Contains(err.Error(), "still being created") {
		t.Fatalf("logs: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("logs wrote %q before refusing", out.String())
	}
}

// Hold a real registry request open so readers can observe runInstance before
// the pull finishes. Returning an error then exercises its pre-spawn cleanup.
func TestRunRecordsCreatingBeforePullAndCleansUpOnFailure(t *testing.T) {
	// Keep the path short enough for the instance's Unix sockets. The marker
	// avoids creating or mounting an APFS image for this scratch store.
	dir, err := os.MkdirTemp("/tmp", "hull-pull-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ignored, err := storeIgnoresOwnership(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ignored {
		t.Skip("scratch filesystem ignores ownership; the store would need remounting")
	}
	if err := os.WriteFile(filepath.Join(dir, storeMarkerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// An explicit empty config keeps the pull anonymous and prevents falling
	// back to the user's Docker or Podman credentials.
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfig)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		http.Error(w, "intentional pull failure", http.StatusBadRequest)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := &cli.Command{
		Name:     "hull-test",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir"}},
		Commands: []*cli.Command{runCommand()},
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	// Join the command even after an assertion fails, before the scratch
	// directory is removed or DOCKER_CONFIG is restored.
	defer func() {
		cancel()
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("run did not stop during cleanup")
		}
	}()
	go func() {
		defer close(finished)
		done <- cmd.Run(ctx, []string{
			"hull-test", "--store-dir", dir, "run", "--name", "puller", "--pull", "always",
			strings.TrimPrefix(srv.URL, "http://") + "/test/image:latest",
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("run ended before reaching the registry: %v", err)
	case <-ctx.Done():
		t.Fatal("run did not reach the registry")
	}

	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.GetInstance("puller")
	if err != nil {
		t.Fatalf("instance during the blocked pull: %v", err)
	}
	if state.Status != store.StatusCreating || state.PID != 0 || state.StartTime.IsZero() {
		t.Fatalf("instance during the blocked pull: %+v", state)
	}
	row := psRow(t, s, "puller")
	if !strings.Contains(row, store.StatusCreating) || strings.Contains(row, store.StatusUnreadable) {
		t.Fatalf("ps during the blocked pull: %q", row)
	}
	if err := stopInstanceIn(s, "puller", 0); err == nil || !strings.Contains(err.Error(), "still being created") {
		t.Fatalf("stop during the blocked pull: %v", err)
	}
	var out bytes.Buffer
	if err := logsInstance(s, "puller", &out, false, -1); err == nil || !strings.Contains(err.Error(), "still being created") {
		t.Fatalf("logs during the blocked pull: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("logs wrote output before refusing: %q", out.String())
	}
	state, err = s.GetInstance("puller")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != store.StatusCreating || state.PID != 0 || state.StoppedByUser {
		t.Fatalf("commands changed the creating record: %+v", state)
	}

	unblock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unexpected status code 400") {
			t.Fatalf("run after the intentional registry error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("run did not finish after the registry error")
	}
	if _, err := os.Stat(s.InstanceDir("puller")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed run left its instance directory: %v", err)
	}
	if _, err := s.GetInstance("puller"); !errors.Is(err, store.ErrInstanceNotFound) {
		t.Fatalf("failed run remains findable: %v", err)
	}
}
