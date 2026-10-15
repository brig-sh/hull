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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diskRoundsCase builds what a destination holds after a live move with
// hvi's rounds 0 and 1 before the fence and round 2 after it: the
// trailer's account, the batches applied, and hvi's disk.json.
func diskRoundsCase() (*streamDiskRounds, map[int]*diskRoundDigest, map[string]any) {
	applied := map[int]*diskRoundDigest{}
	t := &streamDiskRounds{Engine: "hvi"}
	var listed []map[string]any
	for round, batches := range [][]string{{"a", "b"}, {"c"}, {"d", "e", "f"}} {
		d := &diskRoundDigest{round: streamDiskRound{Round: round, Final: round == 2}}
		for _, b := range batches {
			d.add(2, sha256Hex([]byte(b)))
		}
		applied[round] = d
		want := d.done()
		want.From, want.Generation, want.BitmapSHA256 = "hvi", uint64(10*(round+1)), strings.Repeat("b", 64)
		t.Rounds = append(t.Rounds, want)
		if round < 2 {
			sent := want.SentSHA256
			listed = append(listed, map[string]any{"round": round, "generation": want.Generation,
				"blocks": want.Blocks, "bitmap_sha256": want.BitmapSHA256, "sent_sha256": sent})
		}
	}
	return t, applied, map[string]any{"version": 1, "generation": 30, "rounds": listed}
}

// The destination holds the disk rounds it applied to the trailer, and
// hvi's rounds to what hvi recorded in disk.json: a batch lost on the way,
// a round hvi read and the source never sent, a round sent twice, a final
// round skipped or read before the guest's last write all refuse.
func TestCheckDiskRounds(t *testing.T) {
	check := func(edit func(*streamDiskRounds, map[int]*diskRoundDigest, map[string]any)) error {
		tr, applied, doc := diskRoundsCase()
		if edit != nil {
			edit(tr, applied, doc)
		}
		js, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return checkDiskRounds(false, tr, applied, js)
	}
	if err := check(nil); err != nil {
		t.Fatalf("a whole move: %v", err)
	}
	listed := func(doc map[string]any) []map[string]any { return doc["rounds"].([]map[string]any) }
	for name, edit := range map[string]func(*streamDiskRounds, map[int]*diskRoundDigest, map[string]any){
		"a batch lost": func(_ *streamDiskRounds, a map[int]*diskRoundDigest, _ map[string]any) {
			d := &diskRoundDigest{round: streamDiskRound{Round: 0}}
			d.add(2, sha256Hex([]byte("a")))
			a[0] = d
		},
		"batches reordered": func(_ *streamDiskRounds, a map[int]*diskRoundDigest, _ map[string]any) {
			d := &diskRoundDigest{round: streamDiskRound{Round: 0}}
			d.add(2, sha256Hex([]byte("b")))
			d.add(2, sha256Hex([]byte("a")))
			a[0] = d
		},
		"a round not in the trailer": func(_ *streamDiskRounds, a map[int]*diskRoundDigest, _ map[string]any) {
			a[7] = &diskRoundDigest{round: streamDiskRound{Round: 7}}
		},
		"the final round skipped": func(tr *streamDiskRounds, a map[int]*diskRoundDigest, _ map[string]any) {
			tr.Rounds = tr.Rounds[:2]
			delete(a, 2)
		},
		"the final round read early": func(tr *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			doc["generation"] = 31
		},
		"a round hvi read, never sent": func(_ *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			doc["rounds"] = append(listed(doc), map[string]any{"round": 2, "generation": 30, "blocks": 0,
				"bitmap_sha256": strings.Repeat("b", 64), "sent_sha256": nil})
		},
		"a round hvi never streamed": func(_ *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			listed(doc)[1]["sent_sha256"] = nil
		},
		"hvi's digest differs": func(_ *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			listed(doc)[1]["sent_sha256"] = strings.Repeat("0", 64)
		},
		"a pre-copy round hvi never recorded": func(_ *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			doc["rounds"] = listed(doc)[:1]
		},
		"no disk.json rounds at all": func(_ *streamDiskRounds, _ map[int]*diskRoundDigest, doc map[string]any) {
			delete(doc, "rounds")
		},
		"two final rounds": func(tr *streamDiskRounds, a map[int]*diskRoundDigest, _ map[string]any) {
			tr.Rounds[1].Final = true
			a[1].round.Final = true
		},
	} {
		if err := check(edit); err == nil {
			t.Errorf("%s: the destination took the disk", name)
		}
	}
	// A replay sends the disk by comparison, which disk.json does not
	// describe: only the trailer's account holds.
	tr, applied, _ := diskRoundsCase()
	if err := checkDiskRounds(true, tr, applied, []byte(`{"generation":99}`)); err != nil {
		t.Errorf("a replay: %v", err)
	}
	if err := checkDiskRounds(false, nil, applied, nil); err == nil {
		t.Error("disk rounds with no account of them were taken")
	}
}

// A destination base whose blocks do not hash to its name is set aside
// once the move has read it, and the move sends the base again and goes
// on from the spool.
func TestStreamedMoveResendsACorruptBase(t *testing.T) {
	f := newStreamFixture(t, true, true)
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
