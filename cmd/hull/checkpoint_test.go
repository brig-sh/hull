//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A checkpoint that vz-runner has already refused should be reported as soon
// as it writes the reason down, not waited out. Before the marker existed a
// save denied in 200ms surfaced a minute later as "did not complete", which
// reads like a slow machine rather than a refusal.
func TestFreshCheckpointFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()
	write(t, dir, "state save failed: permission denied\n")

	reason, ok := freshCheckpointFailure(dir, started)
	if !ok {
		t.Fatal("a marker written after the attempt began must be reported")
	}
	if reason != "state save failed: permission denied" {
		t.Fatalf("reason should be trimmed and verbatim, got %q", reason)
	}
}

// The checkpoint directory outlives a single attempt, so a marker from an
// earlier one must not turn a slow checkpoint into a phantom failure.
func TestStaleCheckpointFailureIsIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "state save failed: permission denied")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, ckptError), old, old); err != nil {
		t.Fatal(err)
	}

	if _, ok := freshCheckpointFailure(dir, time.Now()); ok {
		t.Fatal("a marker predating the attempt describes an older attempt")
	}
}

// No marker is the ordinary case: the checkpoint is simply still running.
func TestNoCheckpointFailureMeansKeepWaiting(t *testing.T) {
	if _, ok := freshCheckpointFailure(t.TempDir(), time.Now()); ok {
		t.Fatal("an absent marker must not be read as a failure")
	}
}

// An empty marker says nothing, and "checkpoint failed: " helps nobody.
func TestEmptyCheckpointFailureIsIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "   \n")

	if _, ok := freshCheckpointFailure(dir, time.Now().Add(-time.Second)); ok {
		t.Fatal("a marker with no reason in it must not be reported as one")
	}
}

func write(t *testing.T, dir, reason string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ckptError), []byte(reason), 0o600); err != nil {
		t.Fatal(err)
	}
}
