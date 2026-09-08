//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// The checkpoint directory as vz-runner leaves it, per
// docs/checkpoint-restore.md:36-43 and vz-runner/Sources/main.swift:1059-1067.
// stateFile/diskImage carry the artifact names; savedAt and durationMs are
// there because the runner writes them and the CLI has to tolerate them.
func putCkptManifest(t *testing.T, dir, stateFile, diskImage string) {
	t.Helper()
	body := fmt.Sprintf(`{"diskImage":%q,"durationMs":412,"savedAt":"2026-09-09T01:00:00Z","stateFile":%q}`,
		diskImage, stateFile)
	putRawCkptManifest(t, dir, body)
}

func putRawCkptManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ckptManifest), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func putCkptArtifact(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setCkptMtime(t *testing.T, dir, name string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
		t.Fatal(err)
	}
}

// A whole checkpoint: both artifacts, then the manifest that names them.
func completeCheckpointDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptArtifact(t, dir, ckptDiskFile, "cloned rootfs")
	putCkptManifest(t, dir, ckptStateFile, ckptDiskFile)
	return dir
}

// docs/checkpoint-restore.md:43 -- "| `latest.json`| manifest, written last --
// its mtime marks checkpoint completion |". Written last is not the same as
// written only when the checkpoint worked: vz-runner publishes the manifest
// whenever the machine state saved (main.swift:1057 gates the write on
// `saveError == nil` and on nothing else) and records a failed disk clone by
// naming no disk image (main.swift:1052-1061). Hull read the manifest's
// presence as proof and printed "Checkpoint of X saved". The runner has
// already deleted the previous clone by then (main.swift:1049), so the
// operator is left with no restorable checkpoint at all and a success message
// saying otherwise. Catches the return of that silent success.
func TestCheckpointFailsWhenTheManifestRecordsNoDiskClone(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptManifest(t, dir, ckptStateFile, "")

	err := waitForCheckpoint(dir, "demo", os.Getpid(), time.Now().Add(-time.Second), 2*time.Second)
	if err == nil {
		t.Fatal("a manifest naming no disk clone describes a checkpoint that cannot be restored; it must not be reported as saved")
	}
}

// The same contract from the other side: the manifest names rootfs.img but the
// file is not there. Catches hull accepting a manifest on its own word instead
// of on the artifacts it names -- the state the directory is left in when the
// clone is removed (main.swift:1049) and its replacement never lands.
func TestCheckpointNeedsTheDiskImageItsManifestNames(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptManifest(t, dir, ckptStateFile, ckptDiskFile)

	err := waitForCheckpoint(dir, "demo", os.Getpid(), time.Now().Add(-time.Second), 2*time.Second)
	if err == nil {
		t.Fatal("a manifest naming a disk image that is absent must not be reported as a saved checkpoint")
	}
	if !strings.Contains(err.Error(), ckptDiskFile) {
		t.Fatalf("the error must name the artifact that is missing, got %q", err)
	}
}

// An artifact with no bytes in it is not an artifact. A zero-length
// vm.vzstate restores nothing, so reporting it as a saved checkpoint is the
// same lie as reporting a missing one. Catches a completeness check that only
// stats for existence.
func TestCheckpointRejectsAnEmptyStateFile(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "")
	putCkptArtifact(t, dir, ckptDiskFile, "cloned rootfs")
	putCkptManifest(t, dir, ckptStateFile, ckptDiskFile)

	err := waitForCheckpoint(dir, "demo", os.Getpid(), time.Now().Add(-time.Second), 2*time.Second)
	if err == nil {
		t.Fatal("an empty machine state file must not be reported as a saved checkpoint")
	}
	if !strings.Contains(err.Error(), ckptStateFile) {
		t.Fatalf("the error must name the artifact at fault, got %q", err)
	}
}

// The success path, so the completeness check cannot be satisfied by refusing
// everything: a manifest whose named artifacts are all present and non-empty
// is a checkpoint, and hull must say so.
func TestCheckpointWithEveryNamedArtifactPresentSucceeds(t *testing.T) {
	dir := completeCheckpointDir(t)

	if err := waitForCheckpoint(dir, "demo", os.Getpid(), time.Now().Add(-time.Second), 2*time.Second); err != nil {
		t.Fatalf("a complete checkpoint must be reported as saved, got %v", err)
	}
}

// The checkpoint directory outlives a single attempt, so the manifest a
// previous checkpoint left behind must not end this one -- it names artifacts
// that are all present, which is exactly what makes it convincing. This is the
// freshness half of docs/checkpoint-restore.md:43 ("its mtime marks checkpoint
// completion"). Catches a completeness check that forgets to also be a
// freshness check.
func TestStaleCheckpointManifestDoesNotEndThisAttempt(t *testing.T) {
	dir := completeCheckpointDir(t)
	setCkptMtime(t, dir, ckptManifest, time.Now().Add(-time.Hour))

	err := waitForCheckpoint(dir, "demo", os.Getpid(), time.Now(), 300*time.Millisecond)
	if err == nil {
		t.Fatal("a manifest from an earlier checkpoint must not be reported as this one completing")
	}
}

// Checkpointing the same instance twice is ordinary use (a golden checkpoint
// gets retaken), and each request has to stand on its own manifest. Drives the
// three answers in sequence with no sleeping: attempt one's manifest completes
// attempt one, does not complete attempt two, and attempt two completes when
// the runner rewrites it.
func TestRepeatedCheckpointRequestsEachNeedTheirOwnManifest(t *testing.T) {
	dir := completeCheckpointDir(t)

	firstAttempt := time.Now().Add(-time.Hour)
	setCkptMtime(t, dir, ckptManifest, firstAttempt.Add(time.Minute))
	if done, err := checkpointCompleted(dir, firstAttempt); err != nil || !done {
		t.Fatalf("the manifest of the attempt in progress must complete it (done=%v err=%v)", done, err)
	}

	secondAttempt := time.Now()
	if done, err := checkpointCompleted(dir, secondAttempt); err != nil || done {
		t.Fatalf("the previous attempt's manifest must not complete this one (done=%v err=%v)", done, err)
	}

	setCkptMtime(t, dir, ckptManifest, secondAttempt.Add(time.Millisecond))
	if done, err := checkpointCompleted(dir, secondAttempt); err != nil || !done {
		t.Fatalf("the rewritten manifest must complete the second attempt (done=%v err=%v)", done, err)
	}
}

// vz-runner writes latest.json non-atomically (main.swift:1066 calls
// Data.write with no .atomic option), so a manifest with a fresh mtime can be
// one that is still being written. Half of one says nothing about the
// checkpoint, and reading it as a verdict would fail a checkpoint that is
// about to succeed. Keep waiting instead: no verdict, no error.
func TestHalfWrittenCheckpointManifestIsNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptArtifact(t, dir, ckptDiskFile, "cloned rootfs")
	putRawCkptManifest(t, dir, `{"stateFile":"vm.vzs`)

	done, err := checkpointCompleted(dir, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("a manifest that does not parse yet must not fail the checkpoint, got %v", err)
	}
	if done {
		t.Fatal("a manifest that does not parse must not complete the checkpoint either")
	}
}

// The manifest is machine-written, so a name that walks out of the checkpoint
// directory is not a checkpoint artifact whatever it points at. The file
// planted next door exists, so this fails if the guard is dropped and the
// name is simply joined and stat'ed.
func TestCheckpointArtifactOutsideTheCheckpointDirIsRejected(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptArtifact(t, filepath.Dir(dir), ckptDiskFile, "a rootfs somewhere else")

	err := verifyCheckpointArtifacts(dir, checkpointManifest{
		StateFile: ckptStateFile,
		DiskImage: "../" + ckptDiskFile,
	})
	if err == nil {
		t.Fatal("an artifact name that leaves the checkpoint directory must be refused")
	}
}

// restore reads vm.vzstate and nothing else, and vz-runner restores the
// machine state onto whatever rootfs it finds: with no clone in the checkpoint
// it prints "no disk image in checkpoint, keeping current rootfs" and carries
// on (main.swift:1091-1094). The guest wakes with memory from the checkpoint
// moment against a disk that has moved on, which is the inconsistency the
// block-rootfs requirement exists to prevent (docs/checkpoint-restore.md:45-47).
// Catches restore accepting a saved machine state whose rootfs.img is gone.
func TestRestoreRefusesACheckpointWhoseDiskImageIsGone(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptManifest(t, dir, ckptStateFile, ckptDiskFile)

	err := requireRestorableCheckpoint(dir)
	if err == nil {
		t.Fatal("restoring a machine state with no rootfs to go with it must be refused")
	}
	if !strings.Contains(err.Error(), ckptDiskFile) {
		t.Fatalf("the error must name the artifact that is missing, got %q", err)
	}
}

// The other shape of the same hole: the clone failed, so the manifest names no
// disk image at all (main.swift:1061), and the previous clone is already gone
// (main.swift:1049). Restore must refuse rather than resume the guest against
// the live rootfs.
func TestRestoreRefusesACheckpointThatRecordedNoDiskClone(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptManifest(t, dir, ckptStateFile, "")

	if err := requireRestorableCheckpoint(dir); err == nil {
		t.Fatal("a checkpoint whose manifest records no disk clone must not be restorable")
	}
}

// A checkpoint with no manifest is one no runner ever finished: latest.json is
// written last (docs/checkpoint-restore.md:43), so a directory holding
// vm.vzstate alone is a save that never became a checkpoint.
func TestRestoreRefusesACheckpointWithNoManifest(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptArtifact(t, dir, ckptDiskFile, "cloned rootfs")

	if err := requireRestorableCheckpoint(dir); err == nil {
		t.Fatal("a checkpoint directory with no manifest must not be restorable")
	}
}

// And the accepting case, so the refusals above are a contract rather than a
// blanket no: a complete checkpoint restores.
func TestRestoreAcceptsACompleteCheckpoint(t *testing.T) {
	if err := requireRestorableCheckpoint(completeCheckpointDir(t)); err != nil {
		t.Fatalf("a complete checkpoint must be restorable, got %v", err)
	}
}

// A manifest that names no machine state file describes nothing that can be
// restored, whatever else is in the directory. Guards the parse against a
// manifest whose keys the runner ever renames: an unrecognised stateFile reads
// as the empty string, and silently treating that as "no artifact to check"
// would let any JSON file pass as a checkpoint.
func TestCheckpointManifestNamingNoStateFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	putCkptArtifact(t, dir, ckptStateFile, "saved machine state")
	putCkptArtifact(t, dir, ckptDiskFile, "cloned rootfs")
	putRawCkptManifest(t, dir, `{"savedAt":"2026-09-09T01:00:00Z"}`)

	if err := requireRestorableCheckpoint(dir); err == nil {
		t.Fatal("a manifest that names no machine state file must not describe a restorable checkpoint")
	}
}
