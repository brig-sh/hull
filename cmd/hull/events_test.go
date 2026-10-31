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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCreateEventsLedgerModes(t *testing.T) {
	// A permissive umask must not widen the file or the directory.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	inst := t.TempDir()
	path, err := createEventsLedger(inst, time.Unix(0, 1790000000000000001))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(inst, "events", "1790000000000000001.ndjson"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
		t.Fatalf("ledger mode %v size %d, want an empty regular file 0600", fi.Mode(), fi.Size())
	}
	di, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !di.IsDir() || di.Mode().Perm() != 0o700 {
		t.Fatalf("events dir mode %v, want a directory 0700", di.Mode())
	}
}

func TestCreateEventsLedgerOneFilePerBoot(t *testing.T) {
	inst := t.TempDir()
	first, err := createEventsLedger(inst, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := createEventsLedger(inst, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both boots got %s", first)
	}
	// The earlier boot's ledger is kept as it was.
	if b, err := os.ReadFile(first); err != nil || string(b) != "{}\n" {
		t.Fatalf("first ledger = %q, %v", b, err)
	}
	// The same boot time twice is refused, not reused.
	if _, err := createEventsLedger(inst, time.Unix(200, 0)); err == nil {
		t.Fatal("a second ledger at the same path was accepted")
	}
}

func TestCreateEventsLedgerRefusesSymlinks(t *testing.T) {
	// A symbolic link in place of the events directory.
	inst := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(inst, "events")); err != nil {
		t.Fatal(err)
	}
	if _, err := createEventsLedger(inst, time.Unix(1, 0)); err == nil {
		t.Fatal("a symlinked events directory was accepted")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("a ledger was written through the symlink: %v", entries)
	}

	// A symbolic link planted at the ledger's own path.
	inst = t.TempDir()
	if err := os.Mkdir(filepath.Join(inst, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.Symlink(target, filepath.Join(inst, "events", "1000000000.ndjson")); err != nil {
		t.Fatal(err)
	}
	if _, err := createEventsLedger(inst, time.Unix(1, 0)); err == nil {
		t.Fatal("a symlink at the ledger path was accepted")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("the symlink's target was created")
	}
}
