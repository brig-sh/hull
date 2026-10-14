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
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

func TestHviRestoreArgv(t *testing.T) {
	const from, cs, pp = "/s/i/checkpoint/tx", "/s/i/control.sock", "/s/i/profile.json"
	tail := []string{"--control-sock", cs, "--profile-out", pp}
	cases := []struct {
		name    string
		cmdLine []string
		want    []string
		refusal string
	}{
		{
			name: "darwin run with the stub NIC",
			cmdLine: []string{"/opt/hvi", "boot", "--kernel", "/k", "--mem-mib", "512", "--cpus", "2",
				"--fs-uid", "1000", "--fs-gid", "1000", "--initramfs", "/i", "--cmdline", "console=hvc0 x",
				"--disk", "/s/i/rootfs.img", "--net", "--net-mac", "52:54:00:aa:bb:cc",
				"--agent-sock", "/s/i/agent.sock", "--sandbox-id", "i",
				"--control-sock", "/old/control.sock", "--profile-out", "/old/profile.json"},
			want: append([]string{"/opt/hvi", "restore", "--from", from, "--disk", "/s/i/rootfs.img",
				"--net-stub", "--net-mac", "52:54:00:aa:bb:cc", "--agent-sock", "/s/i/agent.sock",
				"--sandbox-id", "i"}, tail...),
		},
		{
			name: "linux run on the gateway",
			cmdLine: []string{"/h/hvi", "boot", "--kernel", "/k", "--mem-mib", "256", "--cpus", "1",
				"--disk", "/r", "--net-gateway", "/g.sock", "--net-mac", "m", "--events", "/e.ndjson",
				"--checkpoint-root", "/s/i/checkpoint"},
			want: append([]string{"/h/hvi", "restore", "--from", from, "--disk", "/r",
				"--net-gateway", "/g.sock", "--net-mac", "m", "--events", "/e.ndjson",
				"--checkpoint-root", "/s/i/checkpoint"}, tail...),
		},
		{
			name:    "a share",
			cmdLine: []string{"hvi", "boot", "--disk", "/r", "--share-ro", "/src", "t"},
			refusal: "shared a host directory",
		},
		{
			name:    "no disk",
			cmdLine: []string{"hvi", "boot", "--kernel", "/k"},
			refusal: "no --disk",
		},
		{
			name:    "a flag hull does not know",
			cmdLine: []string{"hvi", "boot", "--disk", "/r", "--net-tap", "tap0"},
			refusal: "--net-tap",
		},
		{
			name:    "a value flag cut off at the end",
			cmdLine: []string{"hvi", "boot", "--disk"},
			refusal: "takes a value",
		},
		{
			name:    "not boot or restore",
			cmdLine: []string{"hvi", "dump-fdt", "--disk", "/r"},
			refusal: "not boot or restore",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hviRestoreArgv(tc.cmdLine, from, cs, pp)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v, want a refusal naming %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("argv\n got %q\nwant %q", got, tc.want)
			}
			// A restored instance records the restore argv as its command
			// line, and the next restore starts from that.
			again, err := hviRestoreArgv(got, from, cs, pp)
			if err != nil {
				t.Fatalf("the mapping refuses its own output: %v", err)
			}
			if !slices.Equal(again, got) {
				t.Errorf("not idempotent\nonce  %q\ntwice %q", got, again)
			}
			// Another bundle replaces --from rather than adding a second.
			other, _ := hviRestoreArgv(got, "/other", cs, pp)
			if v, _ := flagValue(other, "--from"); v != "/other" || strings.Count(strings.Join(other, " "), "--from") != 1 {
				t.Errorf("--from not replaced: %q", other)
			}
		})
	}
}

// restoring and restored-paused are restorable only because nothing has
// run the restored guest yet; TestResumeMakesARestoredGuestUnrestorable and
// the tests after it check that whatever runs it says so first.
func TestRestorableMigration(t *testing.T) {
	for state, ok := range map[string]bool{
		store.MigrationFenced:         true,
		store.MigrationRestoring:      true,
		store.MigrationRestoredPaused: true,
		store.MigrationCheckpointed:   false,
		store.MigrationRestored:       false,
		"bogus":                       false,
	} {
		err := restorableMigration(&store.MigrationState{State: state, BundleDir: "/b"})
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want restorable=%v", state, err, ok)
		}
	}
	if err := restorableMigration(nil); err == nil {
		t.Error("an instance with no checkpoint is restorable")
	}
}

func TestAwaitRestoredAndResume(t *testing.T) {
	var statusCalls atomic.Int32
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		switch req["op"] {
		case "status":
			if statusCalls.Add(1) < 3 {
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"restoring"}`}
			}
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused","vcpus":1,"vcpus_parked":1}`}
		case "resume":
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
		}
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"unknown_op","error":"?"}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	state.Migration = &store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"}
	_ = s.SaveInstance(state)

	got, err := awaitRestoredAndResume(s, "vm1", "t", stub.path, hviRestoreOptions{timeout: 10 * time.Second}, time.Time{}, nil)
	if err != nil || got != store.MigrationRestored {
		t.Fatalf("result = %q, %v", got, err)
	}
	var ops []string
	for _, r := range stub.seen() {
		ops = append(ops, r["op"].(string))
	}
	if want := []string{"status", "status", "status", "resume"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
	if m := mustInstance(t, s, "vm1").Migration; m.State != store.MigrationRestored {
		t.Errorf("migration = %+v", m)
	}
}

func TestAwaitRestoredPausedSendsNoResume(t *testing.T) {
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused"}`}
	})
	s := newCmdTestStore(t)
	state, _ := fakeHviInstance(t, s, "vm1", stub.path)
	state.Migration = &store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"}
	_ = s.SaveInstance(state)
	got, err := awaitRestoredAndResume(s, "vm1", "t", stub.path, hviRestoreOptions{paused: true, timeout: 5 * time.Second}, time.Time{}, nil)
	if err != nil || got != store.MigrationRestoredPaused {
		t.Fatalf("result = %q, %v", got, err)
	}
	if m := mustInstance(t, s, "vm1").Migration; m.State != store.MigrationRestoredPaused {
		t.Errorf("migration = %+v", m)
	}
	for _, r := range stub.seen() {
		if r["op"] == "resume" {
			t.Error("--paused sent a resume")
		}
	}
}

// Until hvi binds the socket there is nothing to dial; a VMM that is gone ends
// the wait at once instead of at the deadline.
func TestAwaitRestoredEndsWhenTheVMMIsGone(t *testing.T) {
	s := newCmdTestStore(t)
	if _, err := s.CreateInstance("vm1"); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(gone) })
	start := time.Now()
	got, err := awaitRestoredAndResume(s, "vm1", "t", filepath.Join(shortSocketDir(t), "none.sock"),
		hviRestoreOptions{timeout: time.Minute}, time.Now(), gone)
	if got != "" || err == nil || !strings.HasPrefix(err.Error(), "hvi exited") {
		t.Errorf("result = %q, %v", got, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("waited %s after the VMM was gone", took)
	}
}

func TestHviHasRestore(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// What the hvi at d80837c..b8ad111 prints, verbatim.
	old := write("old", `echo 'hvi: unknown subcommand "restore"; expected `+"`boot`, `dump-fdt`"+`' >&2; exit 1`)
	newer := write("new", `echo 'hvi: restore needs --from <dir>' >&2; exit 1`)
	if has, err := hviHasRestore(old); err != nil || has {
		t.Errorf("an hvi without restore: has=%v err=%v", has, err)
	}
	if has, err := hviHasRestore(newer); err != nil || !has {
		t.Errorf("an hvi with restore: has=%v err=%v", has, err)
	}
	if _, err := hviHasRestore(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing binary was not reported")
	}
}

// Every refusal restore makes comes before anything is launched, so each is
// checked with no command and no VMM.
func TestHviRestoreRefusals(t *testing.T) {
	binDir := t.TempDir()
	withRestore := filepath.Join(binDir, "hvi")
	if err := os.WriteFile(withRestore, []byte("#!/bin/sh\necho 'hvi: restore needs --from <dir>' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir := t.TempDir()
	withoutRestore := filepath.Join(oldDir, "hvi")
	if err := os.WriteFile(withoutRestore, []byte("#!/bin/sh\necho 'hvi: unknown subcommand \"restore\"; expected `boot`' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A short root, so the control socket path passes its length check and
	// the gateway case reaches the gateway.
	s, err := store.New(shortSocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	// A socket file whose listener is gone, as a gateway that died leaves.
	deadSocket := filepath.Join(shortSocketDir(t), "gw.sock")
	if ln, err := net.Listen("unix", deadSocket); err == nil {
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
	} else {
		t.Fatal(err)
	}
	base := func() *store.InstanceState {
		return &store.InstanceState{
			ID: "vm1", Status: "stopped", Backend: "hvi",
			CmdLine:   []string{withRestore, "boot", "--kernel", "/k", "--disk", "/r.img"},
			Migration: &store.MigrationState{State: store.MigrationFenced, BundleDir: bundle, TxID: "t1"},
		}
	}
	if _, err := s.CreateInstance("vm1"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutate  func(*store.InstanceState)
		latest  string // txid latest.json names; "" writes none
		refusal string
	}{
		{"never checkpointed", func(st *store.InstanceState) { st.Migration = nil }, "", "no hvi checkpoint"},
		{"continued after the checkpoint", func(st *store.InstanceState) {
			st.Migration.State = store.MigrationCheckpointed
		}, "", "--then continue"},
		{"already restored and run", func(st *store.InstanceState) {
			st.Migration.State = store.MigrationRestored
		}, "", "already restored"},
		{"bundle deleted", func(st *store.InstanceState) {
			st.Migration.BundleDir = filepath.Join(bundle, "gone")
		}, "", "is gone"},
		{"latest.json names a later checkpoint", nil, "t2", "never took in"},
		{"a shared directory", func(st *store.InstanceState) {
			st.CmdLine = append(st.CmdLine, "--share-ro", "/src", "fs1")
		}, "", "shared host directory"},
		{"an hvi without restore", func(st *store.InstanceState) {
			st.CmdLine[0] = withoutRestore
		}, "", "no restore subcommand"},
		{"a gateway that is not running", func(st *store.InstanceState) {
			st.CmdLine = append(st.CmdLine, "--net-gateway", filepath.Join(bundle, "gw.sock"))
		}, "t1", "not answering"},
		{"a gateway that died and left its socket", func(st *store.InstanceState) {
			st.CmdLine = append(st.CmdLine, "--net-gateway", deadSocket)
		}, "t1", "not answering"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := base()
			if tc.mutate != nil {
				tc.mutate(st)
			}
			ckpt := instanceCheckpointDir(s, "vm1")
			_ = os.Remove(filepath.Join(ckpt, "latest.json"))
			if tc.latest != "" {
				if err := writeHviCheckpointRecord(ckpt, hviCheckpointRecord{Dir: bundle, TxID: tc.latest}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.SaveInstance(st); err != nil {
				t.Fatal(err)
			}
			err := hviRestoreInstance(nil, s, st, hviRestoreOptions{timeout: time.Second})
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("err = %v, want a refusal naming %q", err, tc.refusal)
			}
		})
	}

	// A fenced source that is still alive holds the disk: restore must wait
	// for its stop, or two VMMs would write one disk.
	stub := newStubHvi(t, func(map[string]any) stubAnswer { return stubAnswer{reply: `{"v":1,"ok":true}`} })
	live, _ := fakeHviInstance(t, s, "live", stub.path)
	live.Migration = &store.MigrationState{State: store.MigrationFenced, BundleDir: bundle, TxID: "t1"}
	if err := hviRestoreInstance(nil, s, live, hviRestoreOptions{timeout: time.Second}); err == nil ||
		!strings.Contains(err.Error(), "still running") {
		t.Errorf("a running source: %v", err)
	}
}

// A source that died without `hull stop` leaves "running" and its dead pid in
// the record. Until the restore's own launch replaces them, that pid must not
// be read as the new VMM exiting.
func TestAwaitRestoredIgnoresTheSourcesStalePid(t *testing.T) {
	ready := time.Now().Add(300 * time.Millisecond)
	stub := newStubHvi(t, func(req map[string]any) stubAnswer {
		if req["op"] == "resume" {
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
		}
		if time.Now().Before(ready) {
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"restoring"}`}
		}
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused"}`}
	})
	s := newCmdTestStore(t)
	pid, done := startProcess(t, "/bin/sleep", "30")
	_ = syscall.Kill(pid, syscall.SIGKILL)
	<-done
	if _, err := s.CreateInstance("vm1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: "vm1", Status: "running", PID: pid, StartTime: time.Now().Add(-time.Hour),
		Migration: &store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"},
	}); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	// launchVMM's save of the restore's own VMM, a little later.
	newPid, _ := startProcessAs(t, "hvi", "/bin/sleep", "30")
	time.AfterFunc(200*time.Millisecond, func() {
		_ = s.SaveInstance(&store.InstanceState{
			ID: "vm1", Status: "running", PID: newPid, StartTime: time.Now(),
			Migration: &store.MigrationState{State: store.MigrationRestoring, BundleDir: "/b", TxID: "t"},
		})
	})
	got, err := awaitRestoredAndResume(s, "vm1", "t", stub.path, hviRestoreOptions{timeout: 10 * time.Second}, since, nil)
	if err != nil || got != store.MigrationRestored {
		t.Errorf("result = %q, %v, want restored: the source's dead pid ended the wait", got, err)
	}
}
