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
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// fakeBase puts a base of digest d(n) in s, made at the time given.
func fakeBase(t *testing.T, s *store.Store, n int, at time.Time) string {
	t.Helper()
	sum := strings.Repeat(string(rune('a'+n)), 64)
	if err := os.MkdirAll(s.BasesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := s.BasePath(sum)
	if err := os.WriteFile(path, []byte("base"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return sum
}

// fakeImage puts a complete image of digest image in s, whose base is sum.
func fakeImage(t *testing.T, s *store.Store, image, sum string, complete bool) {
	t.Helper()
	if complete {
		dir := filepath.Join(s.RootDir(), "images", image)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(`{"digest":"`+image+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteUnpackSchema(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(imageBasesDir(s), 0o700); err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(imageBaseEntry{Digest: sum, Image: image, SizeMB: 32, Recipe: imageBaseRecipe})
	if err := os.WriteFile(filepath.Join(imageBasesDir(s), imageBaseKey(image, 32)+".json"), js, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeInstance records a stopped instance of s whose disk started from sum.
func fakeInstance(t *testing.T, s *store.Store, name, sum string) {
	t.Helper()
	if _, err := s.CreateInstance(name); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{ID: name, Status: "stopped", Base: &store.BaseState{SHA256: sum}}); err != nil {
		t.Fatal(err)
	}
}

// `hull rm` takes the instance's base with it only when nothing else
// needs it: another instance, an image still in the store, or a prepare
// that received it.
func TestRmReleasesTheBaseNothingElseNeeds(t *testing.T) {
	s := shortStore(t)
	old := time.Now().Add(-time.Hour)
	shared, alone, cached := fakeBase(t, s, 0, old), fakeBase(t, s, 1, old), fakeBase(t, s, 2, old)
	fakeInstance(t, s, "a", shared)
	fakeInstance(t, s, "b", shared)
	fakeInstance(t, s, "c", alone)
	fakeInstance(t, s, "d", cached)
	fakeImage(t, s, "sha256:"+strings.Repeat("1", 64), cached, true)
	for _, name := range []string{"a", "c", "d"} {
		if err := removeInstanceIn(s, name, false); err != nil {
			t.Fatal(err)
		}
	}
	if !baseHeld(s, shared) || baseHeld(s, alone) || !baseHeld(s, cached) {
		t.Errorf("held: shared %v (b uses it), alone %v (nothing does), cached %v (its image does)",
			baseHeld(s, shared), baseHeld(s, alone), baseHeld(s, cached))
	}
	if err := removeInstanceIn(s, "b", false); err != nil {
		t.Fatal(err)
	}
	if baseHeld(s, shared) {
		t.Error("the base outlived its last instance")
	}
	// A base a prepare received stays for the move it was sent for.
	received := fakeBase(t, s, 3, old)
	if err := markBaseReceived(s, received); err != nil {
		t.Fatal(err)
	}
	fakeInstance(t, s, "e", received)
	if err := removeInstanceIn(s, "e", false); err != nil {
		t.Fatal(err)
	}
	if !baseHeld(s, received) {
		t.Error("rm removed a base received ahead of a move")
	}
}

// `hull prune` removes the bases nothing needs, the entries of images gone
// from the store, and leftovers; it leaves a base a run may be copying.
// With --bases it also drops the bases kept for images and received ahead
// of a move that no instance uses.
func TestPruneRemovesBasesNothingNeeds(t *testing.T) {
	s := shortStore(t)
	old := time.Now().Add(-time.Hour)
	used, unused, young := fakeBase(t, s, 0, old), fakeBase(t, s, 1, old), fakeBase(t, s, 2, time.Now())
	cached, gone, received := fakeBase(t, s, 3, old), fakeBase(t, s, 4, old), fakeBase(t, s, 5, old)
	fakeInstance(t, s, "a", used)
	fakeImage(t, s, "sha256:"+strings.Repeat("1", 64), cached, true)
	fakeImage(t, s, "sha256:"+strings.Repeat("2", 64), gone, false)
	if err := markBaseReceived(s, received); err != nil {
		t.Fatal(err)
	}
	bad := s.BasePath(used) + ".bad-1"
	if err := os.WriteFile(bad, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	half := filepath.Join(s.BasesDir(), ".new-0123")
	if err := os.WriteFile(half, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(half, old.Add(-time.Hour), old.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := pruneStoreWith(&out, s, false, false, true, ""); err != nil {
		t.Fatal(err)
	}
	if !baseHeld(s, unused) || !strings.Contains(out.String(), "Would remove base "+digestHead(unused)) {
		t.Fatalf("a dry run: %s", out.String())
	}
	out.Reset()
	if err := pruneStoreWith(&out, s, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	held := func() string {
		var b strings.Builder
		for _, sum := range []string{used, unused, young, cached, gone, received} {
			if baseHeld(s, sum) {
				b.WriteString("1")
			} else {
				b.WriteString("0")
			}
		}
		return b.String()
	}
	if got := held(); got != "101101" {
		t.Errorf("held used,unused,young,cached,gone,received = %s, want 101101\n%s", got, out.String())
	}
	for _, path := range []string{bad, half, filepath.Join(imageBasesDir(s), imageBaseKey("sha256:"+strings.Repeat("2", 64), 32)+".json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", path, err)
		}
	}
	out.Reset()
	if err := pruneStoreWith(&out, s, false, true, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "101000" {
		t.Errorf("after --bases held = %s, want 101000\n%s", got, out.String())
	}
	if _, err := os.Stat(filepath.Join(receivedBasesDir(s), received)); !os.IsNotExist(err) {
		t.Errorf("the received mark outlived its base: %v", err)
	}
}

// `hull prune` sweeps bases/spare too: the half-written copy of a spare's
// maker that died, and the spare and lock of a base no longer held. The
// spare and lock of a held base, and a copy that may be at work, stay.
func TestPruneSweepsTheSpares(t *testing.T) {
	s := shortStore(t)
	old := time.Now().Add(-2 * time.Hour)
	held := fakeBase(t, s, 0, old)
	fakeInstance(t, s, "a", held)
	gone := strings.Repeat("f", 64)
	if err := os.MkdirAll(spareBasesDir(s), 0o700); err != nil {
		t.Fatal(err)
	}
	put := func(name string, at time.Time) string {
		path := filepath.Join(spareBasesDir(s), name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale, working := put(".new-0123", old), put(".new-4567", time.Now())
	goneSpare, goneLock := put(gone+".ext4", old), put(gone+".lock", old)
	heldSpare, heldLock := put(held+".ext4", old), put(held+".lock", old)
	var out bytes.Buffer
	if err := pruneStoreWith(&out, s, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, goneSpare, goneLock} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v\n%s", filepath.Base(path), err, out.String())
		}
	}
	for _, path := range []string{working, heldSpare, heldLock} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v\n%s", filepath.Base(path), err, out.String())
		}
	}
	if !strings.Contains(out.String(), "Removed spare/.new-0123 (left by a spare's maker that died") ||
		!strings.Contains(out.String(), "Removed the spare of base "+digestHead(gone)+" (its base is gone") {
		t.Errorf("prune said:\n%s", out.String())
	}
}
