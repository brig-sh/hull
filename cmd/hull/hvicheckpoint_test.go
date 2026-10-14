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
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

func TestHviCheckpointable(t *testing.T) {
	cases := []struct {
		name    string
		cmdLine []string
		refusal string // "" means eligible
	}{
		{"block root", []string{"/opt/hull/hvi", "boot", "--kernel", "k", "--disk", "/r.img"}, ""},
		{"block root with gateway", []string{"hvi", "boot", "--disk", "/r.img", "--net-gateway", "/g.sock"}, ""},
		{"restored instance", []string{"/x/hvi", "restore", "--from", "/b", "--disk", "/r.img"}, ""},
		{"owner flags without shares", []string{"hvi", "boot", "--fs-uid", "1000", "--fs-gid", "1000", "--disk", "/r"}, ""},
		{"virtiofs root", []string{"hvi", "boot", "--kernel", "k", "--share-rw", "/root", "rootfs"}, "block rootfs"},
		{"disk plus a shared dir", []string{"hvi", "boot", "--disk", "/r", "--share-ro", "/src", "fs1"}, "shared host directory"},
		{"disk plus a writable share", []string{"hvi", "boot", "--disk", "/r", "--share-rw", "/w", "fs1"}, "shared host directory"},
		{"vz", []string{"/x/vz-runner", "--rootfs", "/r"}, "hvi backend"},
		{"qemu", []string{"qemu-system-aarch64", "-drive", "file=/r"}, "hvi backend"},
		{"a path that merely mentions hvi", []string{"/hvi/bin/qemu", "--disk", "/r"}, "hvi backend"},
		{"empty", nil, "hvi backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hviCheckpointable(tc.cmdLine)
			switch {
			case tc.refusal == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
				t.Errorf("err = %v, want a refusal naming %q", err, tc.refusal)
			}
		})
	}
}

func TestWriteHviCheckpointRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	rec := hviCheckpointRecord{
		Dir: "/b1", ManifestSHA256: "aa", Then: hviThenFence,
		Stats: json.RawMessage(`{"pause_ms":3}`), SavedAt: time.Unix(1700000000, 0).UTC(), TxID: "t1",
	}
	if err := writeHviCheckpointRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("latest.json mode = %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "latest.json"))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dir", "manifestSha256", "then", "stats", "savedAt", "txid"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("latest.json lacks %s: %s", key, raw)
		}
	}

	rec.Dir, rec.TxID = "/b2", "t2"
	if err := writeHviCheckpointRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := readHviCheckpointRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stats bytes.Buffer
	_ = json.Compact(&stats, got.Stats)
	if got.Dir != "/b2" || got.TxID != "t2" || got.ManifestSHA256 != "aa" || stats.String() != `{"pause_ms":3}` {
		t.Errorf("record = %+v", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("checkpoint dir holds %d entries, want latest.json alone (a temp file left behind?)", len(entries))
	}
}

func TestHviCheckpointRecordsTheBundle(t *testing.T) {
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		dir, _ := req["dir"].(string)
		_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o600)
		return stubAnswer{reply: `{"v":1,"ok":true,"manifest_sha256":"m1","stats":{"pause_ms":4,"capture_ms":9,"ram_present_bytes":1048576,"ram_total_bytes":4194304}}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)

	if err := hviCheckpointInstance(s, state, hviThenContinue, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	first := mustInstance(t, s, "vm1").Migration
	if first == nil || first.State != store.MigrationCheckpointed || first.TxID == "" {
		t.Fatalf("migration = %+v", first)
	}
	ckptDir := instanceCheckpointDir(s, "vm1")
	root, _ := flagValue(state.CmdLine, "--checkpoint-root")
	asked, _ := stub.seen()[0]["dir"].(string)
	if first.BundleDir != filepath.Join(root, first.TxID) || asked != first.BundleDir {
		t.Errorf("bundle %s, hvi asked for %s, want %s", first.BundleDir, asked, filepath.Join(root, first.TxID))
	}
	if info, err := os.Stat(first.BundleDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("bundle dir: %v %v, want a 0700 directory", info, err)
	}
	rec, err := readHviCheckpointRecord(ckptDir)
	if err != nil || rec.Dir != first.BundleDir || rec.ManifestSHA256 != "m1" || rec.Then != "continue" || rec.TxID != first.TxID {
		t.Errorf("latest.json = %+v, %v", rec, err)
	}

	// A second checkpoint replaces hull's own previous bundle.
	if err := hviCheckpointInstance(s, mustInstance(t, s, "vm1"), hviThenFence, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	second := mustInstance(t, s, "vm1").Migration
	if second.State != store.MigrationFenced || second.TxID == first.TxID {
		t.Errorf("migration = %+v", second)
	}
	if _, err := os.Stat(first.BundleDir); !os.IsNotExist(err) {
		t.Errorf("the previous bundle %s is still there: %v", first.BundleDir, err)
	}

	// A bundle placed with --dir is never removed by a later checkpoint.
	own := filepath.Join(instanceCheckpointDir(s, "vm1"), "mine")
	st := mustInstance(t, s, "vm1")
	st.Migration = nil
	_ = s.SaveInstance(st)
	if err := hviCheckpointInstance(s, st, hviThenContinue, own, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := hviCheckpointInstance(s, mustInstance(t, s, "vm1"), hviThenContinue, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(own, "manifest.json")); err != nil {
		t.Errorf("a --dir bundle was removed: %v", err)
	}
}

func TestHviCheckpointFailureKeepsTheRecord(t *testing.T) {
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"timeout","error":"vCPUs did not park"}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	err := hviCheckpointInstance(s, state, hviThenFence, "", time.Minute)
	dir, _ := stub.seen()[0]["dir"].(string)
	if hviRefusalCode(err) != "timeout" {
		t.Fatalf("err = %v, want hvi's timeout refusal", err)
	}
	if got := mustInstance(t, s, "vm1").Migration; got != nil {
		t.Errorf("a failed checkpoint recorded %+v", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory made for the failed bundle is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(instanceCheckpointDir(s, "vm1"), "latest.json")); !os.IsNotExist(err) {
		t.Errorf("a failed checkpoint wrote latest.json: %v", err)
	}
}

func TestHviCheckpointRefusals(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer { return stubAnswer{reply: `{"v":1,"ok":true}`} })
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)

	shared := *state
	shared.CmdLine = append(slices.Clone(state.CmdLine), "--share-rw", "/w", "fs1")
	if err := hviCheckpointInstance(s, &shared, hviThenContinue, "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "shared host directory") {
		t.Errorf("shares: %v", err)
	}
	noSock := *state
	noSock.ControlSock = ""
	if err := hviCheckpointInstance(s, &noSock, hviThenContinue, "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "without a control socket") {
		t.Errorf("no socket: %v", err)
	}
	if err := hviCheckpointInstance(s, state, "later", "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "--then") {
		t.Errorf("bad --then: %v", err)
	}
	full := filepath.Join(instanceCheckpointDir(s, "vm1"), "full")
	_ = os.MkdirAll(full, 0o700)
	_ = os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	if err := hviCheckpointInstance(s, state, hviThenContinue, full, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty --dir: %v", err)
	}
	// hvi may write below its checkpoint root and nowhere else.
	if err := hviCheckpointInstance(s, state, hviThenContinue, t.TempDir(), time.Minute); err == nil ||
		!strings.Contains(err.Error(), "not below") {
		t.Errorf("--dir outside the root: %v", err)
	}
	rootless := *state
	rootless.CmdLine = []string{"/x/hvi", "boot", "--disk", "/r.img"}
	if err := hviCheckpointInstance(s, &rootless, hviThenContinue, "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "without a checkpoint root") {
		t.Errorf("no checkpoint root: %v", err)
	}
	stopped := *state
	stopped.Status = "stopped"
	if err := hviCheckpointInstance(s, &stopped, hviThenContinue, "", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "not running") {
		t.Errorf("stopped: %v", err)
	}
	if n := len(stub.seen()); n != 0 {
		t.Errorf("hvi was sent %d requests by checkpoints that should have been refused", n)
	}
}

func TestHviControlCheckpointRequest(t *testing.T) {
	stub := newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"manifest_sha256":"ab12","stats":{"pause_ms":7}}`}
	})
	reply, err := hviCheckpoint(stub.path, "/abs/b", hviThenFence, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ManifestSHA256 != "ab12" || string(reply.Stats) != `{"pause_ms":7}` {
		t.Errorf("reply = %+v", reply)
	}
	req := stub.seen()[0]
	if req["op"] != "checkpoint" || req["dir"] != "/abs/b" || req["then"] != "fence" || req["v"] != float64(1) {
		t.Errorf("request = %v", req)
	}
}

// The root hvi is given must exist, be private and be resolved: hvi compares
// bundle paths against the resolved root as given, so a root under a link
// (/tmp on macOS) would make every bundle "not below" it.
func TestHviCheckpointRootIsPrivateAndResolved(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	root, err := hviCheckpointRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(base, "checkpoint"))
	if root != want {
		t.Errorf("root = %s, want the resolved %s", root, want)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("root: %v %v, want a 0700 directory", info, err)
	}
	got, err := bundleDirBelow(root, filepath.Join(link, "checkpoint", "b1"))
	if err != nil || got != filepath.Join(root, "b1") {
		t.Errorf("a --dir reached through a link resolved to %q, %v; want %s", got, err, filepath.Join(root, "b1"))
	}
	if _, err := bundleDirBelow(root, root); err == nil {
		t.Error("the root itself was accepted as a bundle directory")
	}
}
