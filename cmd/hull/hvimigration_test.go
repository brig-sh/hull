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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// withMigration records m on instance id.
func withMigration(t *testing.T, s *store.Store, id string, m store.MigrationState) {
	t.Helper()
	st := mustInstance(t, s, id)
	st.Migration = &m
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}
}

func TestSetRestoreStateIsOneWay(t *testing.T) {
	s := newCmdTestStore(t)
	if _, err := s.CreateInstance("vm1"); err != nil {
		t.Fatal(err)
	}
	_ = s.SaveInstance(&store.InstanceState{ID: "vm1", Status: "stopped"})
	steps := []struct{ from, to, want string }{
		{store.MigrationRestoring, store.MigrationRestoredPaused, store.MigrationRestoredPaused},
		{store.MigrationRestoredPaused, store.MigrationRestored, store.MigrationRestored},
		{store.MigrationRestored, store.MigrationRestoredPaused, store.MigrationRestored},
		{store.MigrationRestored, store.MigrationRestoring, store.MigrationRestored},
		{store.MigrationRestoredPaused, store.MigrationRestoring, store.MigrationRestoredPaused},
		// Not in a restore: left alone.
		{store.MigrationFenced, store.MigrationRestored, store.MigrationFenced},
		{store.MigrationCheckpointed, store.MigrationRestoredPaused, store.MigrationCheckpointed},
	}
	for _, step := range steps {
		withMigration(t, s, "vm1", store.MigrationState{State: step.from, BundleDir: "/b", TxID: "t"})
		setRestoreState(s, "vm1", "t", step.to)
		if got := mustInstance(t, s, "vm1").Migration.State; got != step.want {
			t.Errorf("%s -> %s: record says %s, want %s", step.from, step.to, got, step.want)
		}
	}
	// Another checkpoint's restore never writes over this one.
	withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "new"})
	setRestoreState(s, "vm1", "old", store.MigrationRestored)
	if got := mustInstance(t, s, "vm1").Migration.State; got != store.MigrationRestoring {
		t.Errorf("a stale txid moved the record to %s", got)
	}
}

// `hull restore --paused` and then `hull resume`: the restored guest runs, so
// the record must refuse another restore, whatever hvi answered.
func TestResumeMakesARestoredGuestUnrestorable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"resumed", `{"v":1,"ok":true,"state":"running"}`},
		// hvi hangs up without a word: the resume may have happened.
		{"no reply", ""},
	} {
		for _, from := range []string{store.MigrationRestoredPaused, store.MigrationRestoring} {
			t.Run(tc.name+"/"+from, func(t *testing.T) {
				stub := newStubHvi(t, func(map[string]any) stubAnswer { return stubAnswer{reply: tc.reply} })
				s := newCmdTestStore(t)
				fakeHviInstance(t, s, "vm1", stub.path)
				withMigration(t, s, "vm1", store.MigrationState{State: from, BundleDir: "/b", TxID: "t"})
				err := hviPauseResumeIn(s, "vm1", "resume")
				if (tc.reply == "") != (err != nil) {
					t.Errorf("resume: %v", err)
				}
				m := mustInstance(t, s, "vm1").Migration
				if restorableMigration(m) == nil {
					t.Errorf("after a resume the record says %s, which allows a restore", m.State)
				}
			})
		}
	}
}

// A fenced checkpoint is no restore in progress: resume leaves it as it is
// (hvi refuses that resume anyway).
func TestResumeLeavesAFencedRecord(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"fenced","error":"fenced"}`}
	})
	s := newCmdTestStore(t)
	fakeHviInstance(t, s, "vm1", stub.path)
	withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationFenced, BundleDir: "/b", TxID: "t"})
	if err := hviPauseResumeIn(s, "vm1", "resume"); err == nil {
		t.Error("a fenced resume was not refused")
	}
	if m := mustInstance(t, s, "vm1").Migration; m.State != store.MigrationFenced {
		t.Errorf("record = %s, want fenced", m.State)
	}
}

// The restore's own resume: no reply, or a hangup, still records the guest
// as having run.
func TestRestoreResumeWithoutReplyIsRecordedAsRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer stubAnswer
		noRep  bool
	}{
		{"hangup", stubAnswer{}, false},
		{"timeout", stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`, delay: hviControlTimeout + time.Second}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubHvi(t, func(req map[string]any) stubAnswer {
				if req["op"] == "resume" {
					return tc.answer
				}
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused"}`}
			})
			s := newCmdTestStore(t)
			fakeHviInstance(t, s, "vm1", stub.path)
			withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"})
			got, err := awaitRestoredAndResume(s, "vm1", "t", stub.path,
				hviRestoreOptions{timeout: 10 * time.Second}, time.Time{}, nil)
			if got != store.MigrationRestored || err == nil {
				t.Errorf("result = %q, %v; want restored and an error", got, err)
			}
			if tc.noRep && !errors.Is(err, errHviNoReply) {
				t.Errorf("err = %v, want no reply", err)
			}
			if m := mustInstance(t, s, "vm1").Migration; restorableMigration(m) == nil {
				t.Errorf("record says %s, which allows a restore", m.State)
			}
		})
	}
}

// The interleaving a foreground `hull restore --paused` meets: the wait
// records restored-paused, another terminal resumes the guest, then the VMM
// exits and launchVMM saves the record it has held since launch (migration:
// restoring), and the restore's backstop writes restored-paused. The record
// must still say the guest ran.
func TestForegroundRestoreKeepsAResumeGivenMeanwhile(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
	})
	s := newCmdTestStore(t)
	held, _ := fakeHviInstance(t, s, "vm1", stub.path)
	held.Migration = &store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"}
	_ = s.SaveInstance(held)
	stale := *held
	staleMigration := *held.Migration
	stale.Migration = &staleMigration

	setRestoreState(s, "vm1", "t", store.MigrationRestoredPaused)
	if err := hviPauseResumeIn(s, "vm1", "resume"); err != nil {
		t.Fatal(err)
	}
	// launchVMM's markStopped.
	stale.Status, stale.PID = "stopped", 0
	keepRecordedMigration(s, &stale)
	_ = s.SaveInstance(&stale)
	// hviRestoreInstance's backstop with the wait's result.
	setRestoreState(s, "vm1", "t", store.MigrationRestoredPaused)

	m := mustInstance(t, s, "vm1").Migration
	if restorableMigration(m) == nil {
		t.Errorf("record says %s after the restored guest ran", m.State)
	}
}

// A run's foreground launchVMM must not forget a fenced checkpoint taken
// from another terminal when its VMM exits.
func TestVMMExitKeepsAFenceTakenMeanwhile(t *testing.T) {
	s := newCmdTestStore(t)
	held, _ := fakeHviInstance(t, s, "vm1", "/nonexistent.sock")
	stale := *held
	withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationFenced, BundleDir: "/b", TxID: "t"})
	stale.Status, stale.PID = "stopped", 0
	keepRecordedMigration(s, &stale)
	_ = s.SaveInstance(&stale)
	if m := mustInstance(t, s, "vm1").Migration; m == nil || m.State != store.MigrationFenced {
		t.Errorf("record = %+v, want the fenced checkpoint", m)
	}
}

// `--then continue` may resume a restored guest that was never resumed, and
// a failed capture resumes it too, so the record must say it ran before
// the request goes out.
func TestCheckpointContinueMarksARestoredGuestRun(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"capture_failed","error":"disk sync failed"}`}
	})
	s := newCmdTestStore(t)
	fakeHviInstance(t, s, "vm1", stub.path)
	withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationRestoredPaused, BundleDir: "/b", TxID: "t"})
	if err := hviCheckpointInstance(s, mustInstance(t, s, "vm1"), hviThenContinue, "", time.Minute); err == nil {
		t.Fatal("a failed capture was not reported")
	}
	if m := mustInstance(t, s, "vm1").Migration; restorableMigration(m) == nil {
		t.Errorf("record says %s, which allows a restore", m.State)
	}
}

// A checkpoint that gets no reply may still be written by hvi: the
// directory stays, and the record is left as it was.
func TestHviCheckpointWithoutReplyKeepsTheDirectory(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"manifest_sha256":"m"}`, delay: hviControlTimeout + 2*time.Second}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	err := hviCheckpointInstance(s, state, hviThenFence, "", time.Second)
	if !errors.Is(err, errHviNoReply) {
		t.Fatalf("err = %v, want no reply", err)
	}
	dir, _ := stub.seen()[0]["dir"].(string)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the bundle directory hvi may still be writing was removed: %v", err)
	}
	if m := mustInstance(t, s, "vm1").Migration; m != nil {
		t.Errorf("a checkpoint with no reply recorded %+v", m)
	}
	if _, err := readHviCheckpointRecord(instanceCheckpointDir(s, "vm1")); !os.IsNotExist(err) {
		t.Errorf("a checkpoint with no reply wrote latest.json: %v", err)
	}
}

// writeBundle makes a bundle directory below root holding a manifest.json and
// returns the directory and the manifest's sha256.
func writeBundle(t *testing.T, root, name, manifest string) (string, string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(manifest))
	return dir, hex.EncodeToString(sum[:])
}

// stoppedHviInstance records a stopped hvi instance that ran binary with a
// checkpoint root, and returns it with the root.
func stoppedHviInstance(t *testing.T, s *store.Store, binary string) (*store.InstanceState, string) {
	t.Helper()
	dir, err := s.CreateInstance("vm1")
	if err != nil {
		t.Fatal(err)
	}
	root, err := hviCheckpointRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := &store.InstanceState{
		ID: "vm1", Status: "stopped", Backend: "hvi",
		CmdLine: []string{binary, "boot", "--kernel", "/k", "--disk", "/r.img", "--checkpoint-root", root},
	}
	if err := s.SaveInstance(st); err != nil {
		t.Fatal(err)
	}
	return st, root
}

func hviWithoutRestore(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hvi")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'hvi: unknown subcommand \"restore\"; expected `boot`' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A fenced checkpoint whose record update failed is named only by
// latest.json. Restore says how to adopt it, and --adopt-latest records it
// once its manifest checks out.
func TestRestoreAdoptsAFencedLatestCheckpoint(t *testing.T) {
	s, err := store.New(shortSocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	st, root := stoppedHviInstance(t, s, hviWithoutRestore(t))
	old, oldSum := writeBundle(t, root, "t1", `{"old":1}`)
	st.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: old, TxID: "t1", ManifestSHA256: oldSum}
	_ = s.SaveInstance(st)
	newer, newerSum := writeBundle(t, root, "t2", `{"new":1}`)
	if err := writeHviCheckpointRecord(root, hviCheckpointRecord{Dir: newer, TxID: "t2", Then: hviThenFence, ManifestSHA256: newerSum}); err != nil {
		t.Fatal(err)
	}

	err = hviRestoreInstance(context.Background(), nil, s, st, hviRestoreOptions{timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "--adopt-latest") {
		t.Fatalf("without --adopt-latest: %v", err)
	}
	if m := mustInstance(t, s, "vm1").Migration; m.TxID != "t1" {
		t.Errorf("a refused restore changed the record to %+v", m)
	}

	// Adopted; this hvi has no restore, so that is where it stops.
	err = hviRestoreInstance(context.Background(), nil, s, mustInstance(t, s, "vm1"), hviRestoreOptions{timeout: time.Second, adoptLatest: true})
	if err == nil || !strings.Contains(err.Error(), "no restore subcommand") {
		t.Fatalf("with --adopt-latest: %v", err)
	}
	m := mustInstance(t, s, "vm1").Migration
	if m.TxID != "t2" || m.State != store.MigrationFenced || m.BundleDir != newer || m.ManifestSHA256 != newerSum {
		t.Errorf("adopted record = %+v", m)
	}
}

func TestRestoreRefusesToAdoptAnUnsafeLatest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rec     func(dir, sum string) hviCheckpointRecord
		refusal string
	}{
		{"continued", func(dir, sum string) hviCheckpointRecord {
			return hviCheckpointRecord{Dir: dir, TxID: "t2", Then: hviThenContinue, ManifestSHA256: sum}
		}, "neither it nor"},
		{"manifest changed", func(dir, _ string) hviCheckpointRecord {
			return hviCheckpointRecord{Dir: dir, TxID: "t2", Then: hviThenFence, ManifestSHA256: strings.Repeat("0", 64)}
		}, "changed since"},
		{"no manifest hash", func(dir, _ string) hviCheckpointRecord {
			return hviCheckpointRecord{Dir: dir, TxID: "t2", Then: hviThenFence}
		}, "no manifest sha256"},
		{"outside the root", func(_, sum string) hviCheckpointRecord {
			return hviCheckpointRecord{Dir: "/etc", TxID: "t2", Then: hviThenFence, ManifestSHA256: sum}
		}, "not below"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.New(shortSocketDir(t))
			if err != nil {
				t.Fatal(err)
			}
			st, root := stoppedHviInstance(t, s, hviWithoutRestore(t))
			dir, sum := writeBundle(t, root, "t2", `{"new":1}`)
			if err := writeHviCheckpointRecord(root, tc.rec(dir, sum)); err != nil {
				t.Fatal(err)
			}
			err = hviRestoreInstance(context.Background(), nil, s, st, hviRestoreOptions{timeout: time.Second, adoptLatest: true})
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("err = %v, want a refusal naming %q", err, tc.refusal)
			}
			if m := mustInstance(t, s, "vm1").Migration; m != nil {
				t.Errorf("an unsafe latest.json was adopted: %+v", m)
			}
		})
	}
}

// latest.json is the file that would say a later checkpoint exists, in a
// directory hvi may write to: one that does not parse is refused, not
// skipped.
func TestRestoreRefusesAnUnreadableLatest(t *testing.T) {
	s, err := store.New(shortSocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	st, root := stoppedHviInstance(t, s, hviWithoutRestore(t))
	dir, sum := writeBundle(t, root, "t1", `{}`)
	st.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: dir, TxID: "t1", ManifestSHA256: sum}
	_ = s.SaveInstance(st)
	if err := os.WriteFile(filepath.Join(root, "latest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = hviRestoreInstance(context.Background(), nil, s, st, hviRestoreOptions{timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("err = %v", err)
	}
}

// The record keeps the manifest's sha256 hvi reported; a bundle whose
// manifest no longer matches it is refused before anything is launched.
func TestRestoreRefusesABundleChangedSinceTheCheckpoint(t *testing.T) {
	s, err := store.New(shortSocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	st, root := stoppedHviInstance(t, s, hviWithoutRestore(t))
	dir, sum := writeBundle(t, root, "t1", `{"a":1}`)
	if err := writeHviCheckpointRecord(root, hviCheckpointRecord{Dir: dir, TxID: "t1", Then: hviThenFence, ManifestSHA256: sum}); err != nil {
		t.Fatal(err)
	}
	st.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: dir, TxID: "t1", ManifestSHA256: sum}
	_ = s.SaveInstance(st)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = hviRestoreInstance(context.Background(), nil, s, st, hviRestoreOptions{timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("err = %v", err)
	}
}

// The record names the sha256 hvi reported, so restore can check it.
func TestHviCheckpointRecordsTheManifestHash(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"manifest_sha256":"abc"}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	if err := hviCheckpointInstance(s, state, hviThenFence, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if m := mustInstance(t, s, "vm1").Migration; m.ManifestSHA256 != "abc" {
		t.Errorf("record = %+v", m)
	}
}

// Two checkpoints, or a checkpoint and a restore, of one instance must not
// interleave: the second is refused at once, before hvi is asked anything.
func TestCheckpointAndRestoreRefuseWhileAnotherHoldsTheLock(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer { return stubAnswer{reply: `{"v":1,"ok":true}`} })
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	unlock, err := lockInstanceMigration(s, "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if err := hviCheckpointInstance(s, state, hviThenContinue, "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "in progress") {
		t.Errorf("checkpoint: %v", err)
	}
	if err := hviRestoreInstance(context.Background(), nil, s, state, hviRestoreOptions{timeout: time.Second}); err == nil ||
		!strings.Contains(err.Error(), "in progress") {
		t.Errorf("restore: %v", err)
	}
	if n := len(stub.seen()); n != 0 {
		t.Errorf("hvi was sent %d requests", n)
	}
	unlock()
	if err := hviCheckpointInstance(s, state, hviThenContinue, "", time.Minute); err != nil {
		t.Errorf("after unlock: %v", err)
	}
}

// A failed capture into a --dir that was there before empties it and keeps
// it, so the same --dir can be retried.
func TestHviCheckpointFailureEmptiesAnExistingDir(t *testing.T) {
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		dir, _ := req["dir"].(string)
		_ = os.WriteFile(filepath.Join(dir, "ram.bin"), []byte("partial"), 0o600)
		_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o700)
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"capture_failed","error":"disk sync failed"}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	own := filepath.Join(instanceCheckpointDir(s, "vm1"), "mine")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hviCheckpointInstance(s, state, hviThenContinue, own, time.Minute); hviRefusalCode(err) != "capture_failed" {
		t.Fatalf("err = %v", err)
	}
	entries, err := os.ReadDir(own)
	if err != nil || len(entries) != 0 {
		t.Errorf("--dir after a failed capture: %v entries, %v; want it kept and empty", len(entries), err)
	}
}
