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
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// fakeHviProgram returns an executable named hvi that answers the probes
// hull makes of an hvi binary: --version, the boot flag probe, restore
// (present or not), and checkpoint-verify (passes a directory with a
// manifest.json).
//
// One of each kind per test binary, run once before it is handed out: macOS
// scans an executable the first time it runs, which on a busy machine has
// taken over 30 s, longer than hull's 5 s bound on a probe.
func fakeHviProgram(t *testing.T, hasRestore bool) string {
	t.Helper()
	fakeHviOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hull-fake-hvi-")
		if err != nil {
			fakeHviErr = err
			return
		}
		fakeHviDir = dir
		for _, has := range []bool{false, true} {
			restore := `echo 'error: unknown subcommand "restore"' >&2; exit 2`
			if has {
				restore = `echo 'error: restore needs --from' >&2; exit 2`
			}
			script := fmt.Sprintf(`#!/bin/sh
case "$1" in
--version) echo "hvi 0.0.0-test" ;;
boot) echo "error: --checkpoint-root needs a directory" >&2; exit 2 ;;
restore) %s ;;
checkpoint-verify) [ -f "$2/manifest.json" ] || { echo "no manifest in $2" >&2; exit 1; }; echo "ok: test bundle" ;;
*) echo "unknown" >&2; exit 2 ;;
esac
`, restore)
			path := filepath.Join(dir, fmt.Sprintf("restore-%v", has), "hvi")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				fakeHviErr = err
				return
			}
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				fakeHviErr = err
				return
			}
			_ = exec.Command(path, "--version").Run()
		}
	})
	if fakeHviErr != nil {
		t.Fatal(fakeHviErr)
	}
	return filepath.Join(fakeHviDir, fmt.Sprintf("restore-%v", hasRestore), "hvi")
}

var (
	fakeHviOnce sync.Once
	fakeHviDir  string
	fakeHviErr  error
)

// shortStore is a store under /tmp, so the destination's socket paths fit
// in sun_path.
func shortStore(t *testing.T) *store.Store {
	t.Helper()
	dir := shortSocketDir(t)
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The destination's verbs are idempotent and keep to their record.
func TestDestinationVerbs(t *testing.T) {
	s := shortStore(t)
	hvi := fakeHviProgram(t, true)
	txid := strings.Repeat("ab", 16)
	req := migrateReserveRequest{Epoch: 4, Spec: migrateSpec{SourceHost: "src", Net: "none", Hvi: hvi}}
	if r := destReserve(s, txid, "vm1", req); !r.OK || r.Record.State != dstReserved {
		t.Fatalf("reserve: %+v", r)
	}
	if r := destReserve(s, txid, "vm1", req); !r.OK {
		t.Errorf("a second reserve of the same move: %+v", r)
	}
	if r := destReserve(s, strings.Repeat("cd", 16), "vm1", req); r.OK || r.Code != codeNameTaken {
		t.Errorf("another move for the same name: %+v", r)
	}
	if r := destProbe(s, "vm1", hvi, ""); !r.OK || !r.Probe.NameTaken || r.Probe.NameTxID != txid ||
		!r.Probe.HviRestore || !r.Probe.HviControlFlags {
		t.Errorf("probe: %+v", r.Probe)
	}
	// The gateway the source names is dialled, not just looked for.
	gw := filepath.Join(s.RootDir(), "gw.sock")
	if r := destProbe(s, "", hvi, gw); r.Probe.GatewayOK || r.Probe.GatewayDetail == "" {
		t.Errorf("probe of a gateway that is not there: %+v", r.Probe)
	}
	ln, err := net.Listen("unix", gw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if r := destProbe(s, "", hvi, gw); !r.Probe.GatewayOK {
		t.Errorf("probe of a listening gateway: %+v", r.Probe)
	}
	if r := destCommit(s, txid, 4); r.OK || r.Code != codeNotReady {
		t.Errorf("commit of a reserved move: %+v", r)
	}
	if r := destCommit(s, txid, 3); r.OK || r.Code != codeWrongEpoch {
		t.Errorf("commit with the wrong epoch: %+v", r)
	}
	if r := destStatus(s, strings.Repeat("ef", 16)); r.OK || r.Code != codeUnknownTxID {
		t.Errorf("status of an unknown move: %+v", r)
	}
	if r := destAbort(s, strings.Repeat("ef", 16)); !r.OK || r.Code != codeUnknownTxID {
		t.Errorf("abort of an unknown move: %+v", r)
	}
	// A stream that is not one is refused, and the record says why.
	if r := destReceive(s, txid, "vm1", strings.NewReader("garbage")); r.OK {
		t.Errorf("receive of garbage: %+v", r)
	}
	rec, _, _ := findMigrationByTxID(s, txid)
	if rec.State != dstReceiving || rec.Error == "" {
		t.Errorf("after a bad stream: %s %q", rec.State, rec.Error)
	}
	if r := destAbort(s, txid); !r.OK || r.Record.State != migAborted {
		t.Errorf("abort: %+v", r)
	}
	if r := destAbort(s, txid); !r.OK {
		t.Errorf("a second abort: %+v", r)
	}
	if r := destReserve(s, txid, "vm1", req); r.OK || r.Code != codeAborted {
		t.Errorf("reserve after the abort: %+v", r)
	}
	if _, err := os.Stat(s.InstanceDir("vm1")); !os.IsNotExist(err) {
		t.Errorf("the aborted instance directory: %v", err)
	}
}

// removeFakeHvi removes the fake hvi programs once every test has run.
func removeFakeHvi() {
	if fakeHviDir != "" {
		_ = os.RemoveAll(fakeHviDir)
	}
}
