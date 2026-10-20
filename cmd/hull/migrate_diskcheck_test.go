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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A destination base whose blocks do not hash to its name is set aside
// once the move has read it, and the move sends the base again and goes
// on from the spool.
func TestStreamedMoveResendsACorruptBase(t *testing.T) {
	f := newStreamFixture(t)
	st := mustInstance(t, f.src, f.name)
	if _, err := sendBase(t.Context(), f.src, f.peer, st.Base.SHA256); err != nil {
		t.Fatal(err)
	}
	path := f.dst.BasePath(st.Base.SHA256)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte("rot"), 4096); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if err := f.migrate(t, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.srcRecord(t)
	if rec.State != srcCommitted || rec.Stream.Mode != "replay" {
		t.Fatalf("source %s, stream %+v", rec.State, rec.Stream)
	}
	aside, _ := filepath.Glob(path + ".bad-*")
	if len(aside) != 1 || !baseHeld(f.dst, st.Base.SHA256) {
		t.Errorf("set aside %v; base held again %v", aside, baseHeld(f.dst, st.Base.SHA256))
	}
	if got, _, err := diskDigestFile(path); err != nil || got != st.Base.SHA256 {
		t.Errorf("the base sent again hashes to %s, %v", got, err)
	}
	f.checkArrived(t, rec)
}

// A base's check is recorded beside it and stands while the base is the
// same file, unchanged; a write to it, which changes its change time, has
// it hashed again.
func TestCheckBaseDigestRecordsAPassedCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base.ext4")
	writeSparseDisk(t, path, 4<<20)
	sum, _, err := diskDigestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkBaseDigest(path, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".checked"); err != nil {
		t.Fatalf("no record of the check: %v", err)
	}
	// The record is of that digest only.
	if err := checkBaseDigest(path, strings.Repeat("0", 64)); err == nil {
		t.Error("the record of one digest passed another")
	}
	if err := checkBaseDigest(path, sum); err != nil {
		t.Errorf("the recorded check: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	fh, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte("rot"), 8192); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if err := checkBaseDigest(path, sum); err == nil {
		t.Error("a changed base passed its old check")
	}
}
