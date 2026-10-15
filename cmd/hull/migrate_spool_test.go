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
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// spoolOf writes data as the spooled stream of a fresh directory.
func spoolOf(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, spoolStream), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeHviStream is one whole stream as the fake hvi writes it, and the
// offset at which its manifest's frame starts.
func fakeHviStream(t *testing.T) ([]byte, int) {
	t.Helper()
	f := newStreamFixture(t, true, true)
	conn, br, err := hviStreamOp(f.hvi.path, map[string]any{"op": "migrate", "then": "fence"}, hviStreamIdle)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(br)
	_ = conn.Close()
	f.hvi.mu.Lock()
	defer f.hvi.mu.Unlock()
	data := append([]byte{}, f.hvi.emitted...)
	return data, bytes.LastIndex(data, []byte(`{"component":"manifest.json"`))
}

// A spool is whole only when every frame checks and the manifest it ends
// with lists exactly what the frames add up to.
func TestVerifySpool(t *testing.T) {
	data, man := fakeHviStream(t)
	c, err := verifySpool(spoolOf(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if c.manifest == nil || c.files["cpu.json"] == nil || c.files["ram-round-0"] != nil {
		t.Fatalf("contents: manifest %d bytes, files %d", len(c.manifest), len(c.files))
	}
	// The first frame of ram-round-0 ends where its second header begins.
	second := bytes.Index(data[1:], []byte(`{"component":"ram-round-0"`)) + 1
	second = bytes.Index(data[second+1:], []byte(`{"component":"ram-round-0"`)) + second + 1
	flipped := append([]byte{}, data...)
	flipped[man-10] ^= 0xff
	for name, bad := range map[string][]byte{
		"cut in the manifest":   data[:len(data)-3],
		"cut before it":         data[:man],
		"a corrupted frame":     flipped,
		"bytes after the end":   append(append([]byte{}, data...), 'x'),
		"a frame dropped":       append(append([]byte{}, data[:second]...), data[bytes.Index(data[second+1:], []byte(`{"component"`))+second+1:]...),
		"empty":                 nil,
		"a header with no body": []byte("{\"component\":\"profile.json\",\"size\":9,\"sha256\":\"" + strings.Repeat("0", 64) + "\"}\n"),
	} {
		if _, err := verifySpool(spoolOf(t, bad)); err == nil {
			t.Errorf("%s: a spool that is not whole checked out", name)
		}
	}
	// openSpool seals a whole spool, and refuses one that is not.
	dir := spoolOf(t, data)
	if err := openSpool(dir); err != nil || !spoolIsSealed(dir) {
		t.Fatalf("a whole spool was not sealed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Error(err)
	}
	dir = spoolOf(t, data[:man])
	if err := openSpool(dir); err == nil || spoolIsSealed(dir) {
		t.Fatalf("a cut spool was sealed: %v", err)
	}
}

// The environment a crashing move's child reads its fixture from.
const (
	crashEnvSrc  = "HULL_TEST_CRASH_SRC"
	crashEnvDst  = "HULL_TEST_CRASH_DST"
	crashEnvName = "HULL_TEST_CRASH_NAME"
	crashEnvHvi  = "HULL_TEST_CRASH_HVI"
)

// TestStreamedMoveCrashChild is not a test of its own: run by
// crashMoveAfterSeal in a child process, it moves the parent's instance and
// dies, with no deferred call run, as soon as hvi has sealed the stream and
// fenced the guest, before hull records anything: a hull killed in that
// window.
func TestStreamedMoveCrashChild(t *testing.T) {
	src := os.Getenv(crashEnvSrc)
	if src == "" {
		t.Skip("run by the crash tests")
	}
	s, err := store.New(src)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.New(os.Getenv(crashEnvDst))
	if err != nil {
		t.Fatal(err)
	}
	(&fakeDestRestore{}).install(t)
	streamTestHook = func(stage string) {
		if stage == "relayed" {
			os.Exit(3)
		}
	}
	opt := migrateOptions{to: "pm@dest", remote: migrateRemote{Target: "pm@dest", Hvi: os.Getenv(crashEnvHvi)},
		out: io.Discard}
	err = sourceMigrate(context.Background(), s, os.Getenv(crashEnvName), opt, newLocalPeer(d))
	t.Fatalf("the move ended without dying in the window: %v", err)
}

// crashMoveAfterSeal runs the move of f in a child that dies once hvi has
// fenced the guest, and returns the spool the move was writing.
func crashMoveAfterSeal(t *testing.T, f *streamFixture) string {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestStreamedMoveCrashChild$", "-test.count=1")
	c.Env = append(os.Environ(), crashEnvSrc+"="+f.src.RootDir(), crashEnvDst+"="+f.dst.RootDir(),
		crashEnvName+"="+f.name, crashEnvHvi+"="+f.moveFixture.hvi)
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the child did not die in the window: %v\n%s", err, out)
	}
	if f.hvi.state != "fenced" {
		t.Fatalf("hvi is %s after the child died", f.hvi.state)
	}
	rec := f.srcRecord(t)
	if rec.State != srcTransferring || rec.ManifestSHA256 != "" {
		t.Fatalf("the child recorded the fence: %s %q", rec.State, rec.ManifestSHA256)
	}
	root, _ := flagValue(mustInstance(t, f.src, f.name).CmdLine, "--checkpoint-root")
	return filepath.Join(root, rec.TxID)
}

// A hull killed between hvi's seal and its own leaves the guest fenced with
// an unsealed spool. The abort checks the spool frame by frame, seals it,
// and brings the guest back from it, without asking hvi again; when the
// spool was cut short, hvi, which still holds the fenced guest, streams it
// again first.
func TestStreamedMoveAbortAfterAKillInTheSealWindow(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(map[bool]string{false: "whole", true: "cut"}[cut], func(t *testing.T) {
			f := newStreamFixture(t, true, true)
			dir := crashMoveAfterSeal(t, f)
			if spoolIsSealed(dir) {
				t.Fatal("the dead hull sealed its spool")
			}
			if cut {
				info, err := os.Stat(filepath.Join(dir, spoolStream))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(filepath.Join(dir, spoolStream), info.Size()/2); err != nil {
					t.Fatal(err)
				}
			}
			var restoredFrom string
			localRestore = func(s *store.Store, name string) error {
				st, err := s.GetInstance(name)
				if err != nil {
					return err
				}
				r, err := restoreStreamSource(st, hviRestoreOptions{detach: true})
				if r != nil {
					restoredFrom = r.Name()
					_ = r.Close()
				}
				return err
			}
			var out bytes.Buffer
			if err := sourceAbort(context.Background(), f.src, f.name, &out, f.peer); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if restoredFrom != filepath.Join(dir, spoolStream) {
				t.Fatalf("restored from %q\n%s", restoredFrom, out.String())
			}
			if want := map[bool]int{false: 1, true: 2}[cut]; f.hvi.migrations != want {
				t.Errorf("hvi streamed %d times, want %d", f.hvi.migrations, want)
			}
			if _, err := verifySpool(dir); err != nil || !spoolIsSealed(dir) {
				t.Errorf("the spool the guest came back from: sealed %v, %v", spoolIsSealed(dir), err)
			}
		})
	}
}

// The same kill, then the move run again: it records the fence it finds
// and finishes the move from the spool.
func TestStreamedMoveRerunAfterAKillInTheSealWindow(t *testing.T) {
	f := newStreamFixture(t, true, true)
	dir := crashMoveAfterSeal(t, f)
	rec := f.srcRecord(t)
	if err := f.migrate(t, rec.TxID); err != nil {
		t.Fatal(err)
	}
	rec = f.srcRecord(t)
	if rec.State != srcCommitted || rec.BundleDir != dir || rec.Stream.Mode != "replay" {
		t.Fatalf("source record %s %s %+v", rec.State, rec.BundleDir, rec.Stream)
	}
	if f.hvi.migrations != 1 {
		t.Errorf("hvi streamed %d times; the whole spool needed none", f.hvi.migrations)
	}
	f.checkArrived(t, rec)
}

// With the spool cut short and an hvi that cannot stream the fenced guest
// again, nothing can bring the guest back but that hvi: the abort changes
// nothing, does not stop it, and does not tell the operator to abort.
func TestStreamedMoveAbortRefusesWithNoWholeStream(t *testing.T) {
	f := newStreamFixture(t, true, true)
	dir := crashMoveAfterSeal(t, f)
	if err := os.Truncate(filepath.Join(dir, spoolStream), 100); err != nil {
		t.Fatal(err)
	}
	f.hvi.mu.Lock()
	f.hvi.refuseFenced = true
	f.hvi.mu.Unlock()
	restored := false
	localRestore = func(*store.Store, string) error { restored = true; return nil }
	err := sourceAbort(context.Background(), f.src, f.name, io.Discard, f.peer)
	if err == nil || !strings.Contains(err.Error(), "Do not stop that hvi") || strings.Contains(err.Error(), "--abort") {
		t.Fatalf("err = %v", err)
	}
	if restored || syscall.Kill(f.vm.pid, 0) != nil || f.hvi.state != "fenced" {
		t.Errorf("restored %v; the fenced hvi was stopped or changed: %s", restored, f.hvi.state)
	}
	if rec := f.srcRecord(t); rec.State != srcTransferring {
		t.Errorf("the abort moved the record to %s", rec.State)
	}
	// A re-run says the same, and offers no abort either.
	err = f.migrate(t, f.srcRecord(t).TxID)
	if err == nil || !strings.Contains(err.Error(), "Do not stop that hvi") || strings.Contains(err.Error(), "--abort") {
		t.Fatalf("re-run: %v", err)
	}
}
