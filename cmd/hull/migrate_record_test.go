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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every transition the table allows is taken, every other one refused, and
// re-entering the current state is a no-op that still succeeds.
func TestMoveTransitions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migration.json")
	states := map[string][]string{
		roleSource: {srcPreflight, srcReserved, srcFenced, srcTransferring, srcTransferred, srcCommitting,
			srcCommitted, migAborted},
		roleDestination: {dstReserved, dstReceiving, dstReceived, dstVerified, dstRestoredPaused, dstRunning,
			migAborted},
	}
	for role, all := range states {
		for _, from := range all {
			for _, to := range all {
				rec := &migrationRecord{TxID: strings.Repeat("a", 32), Role: role, State: from}
				err := rec.advance(path, to)
				want := from == to || slices.Contains(migrateTransitions[role][from], to)
				if (err == nil) != want {
					t.Errorf("%s %s -> %s: err %v, want allowed=%v", role, from, to, err, want)
				}
				if err == nil {
					got, rerr := readMigrationRecord(path)
					if from != to && (rerr != nil || got.State != to) {
						t.Errorf("%s %s -> %s: recorded %+v %v", role, from, to, got, rerr)
					}
				}
			}
		}
	}
	// The ones that matter most, spelled out.
	for _, c := range []struct{ role, from, to string }{
		{roleSource, srcCommitted, migAborted},      // no abort after the commit
		{roleSource, srcCommitting, srcTransferred}, // no way back past the commit point
		{roleSource, srcFenced, srcReserved},        // nor past the fence
		{roleDestination, dstRunning, migAborted},   // a running destination is never aborted
		{roleDestination, migAborted, dstRunning},   // an aborted one never runs
		{roleSource, srcReserved, srcTransferring},  // no transfer without a fence
	} {
		rec := &migrationRecord{Role: c.role, State: c.from}
		if err := rec.advance(path, c.to); err == nil {
			t.Errorf("%s %s -> %s was allowed", c.role, c.from, c.to)
		}
	}
	// A streamed move fences inside its transfer: from reserved it goes
	// to transferring and nowhere else, and no further back than the
	// whole-bundle move past it.
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{srcReserved, srcTransferring, true},
		{srcReserved, srcFenced, false},
		{srcReserved, migAborted, true},
		{srcTransferring, srcReserved, false},
		{srcTransferring, srcTransferred, true},
		{srcCommitting, srcTransferred, false},
		{srcCommitted, migAborted, false},
	} {
		rec := &migrationRecord{TxID: strings.Repeat("a", 32), Role: roleSource, State: c.from,
			Transport: transportStream}
		if err := rec.advance(path, c.to); (err == nil) != c.ok {
			t.Errorf("stream %s -> %s: err %v, want allowed=%v", c.from, c.to, err, c.ok)
		}
	}
}

func TestMoveRecordJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.json")
	rec := &migrationRecord{TxID: strings.Repeat("0f", 16), Role: roleSource, Name: "vm1", Epoch: 3,
		State: srcFenced, Peer: "pm@rp2", Remote: &migrateRemote{Target: "pm@rp2", Store: "/s"},
		Disk:     &migrateDisk{Path: "/d", Size: 10, ModNs: 5, SHA256: "ab", DataBytes: 4},
		Transfer: &migrateTransfer{Bytes: 100, Seconds: 0.5, BytesPerSec: 200},
		Timings:  map[string]int64{"fence": 12},
	}
	if err := writeMigrationRecord(path, rec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "txid", "role", "name", "epoch", "state", "peer", "remote", "disk",
		"transfer", "timingsMs", "history", "createdAt", "updatedAt"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("migration.json has no %q: %s", key, data)
		}
	}
	got, err := readMigrationRecord(path)
	if err != nil || got.TxID != rec.TxID || got.Epoch != 3 || got.Disk.SHA256 != "ab" || got.Remote.Store != "/s" {
		t.Errorf("read back %+v, %v", got, err)
	}
	// Another version is refused, not misread.
	_ = os.WriteFile(path, bytes.Replace(data, []byte(`"version": 1`), []byte(`"version": 2`), 1), 0o600)
	if _, err := readMigrationRecord(path); err == nil {
		t.Error("a version 2 record was read")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the record's directory has %d entries (a temp file left behind?)", len(entries))
	}
}

func TestMoveTxID(t *testing.T) {
	a, err := newMoveTxID()
	if err != nil || checkTxID(a) != nil {
		t.Fatalf("txid %q: %v", a, err)
	}
	b, _ := newMoveTxID()
	if a == b {
		t.Error("two txids are the same")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("A", 32), strings.Repeat("a", 31) + "/", "../../x"} {
		if checkTxID(bad) == nil {
			t.Errorf("txid %q accepted", bad)
		}
	}
}
