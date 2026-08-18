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

package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A record must never be observable in a half-written state, BY ANOTHER
// PROCESS.
//
// os.WriteFile opens O_TRUNC and writes in place, so the file spent a moment
// empty on every save. A reader landing there got "unexpected end of JSON
// input", ListInstances silently skipped the instance, and `hull ps` stopped
// showing a VM that was still running -- after which stop and inspect said "not
// found" and rm deleted the directory out from under it. Measured on the
// shipped binary: the instance dropped from ps 86 times in 62,053 probes.
//
// This has to be cross-process. Writers take the store mutex and an on-disk
// lock; READERS take neither, and within one process the mutex hides the whole
// race -- an in-process version of this test passed against the non-atomic
// write, which makes it worthless. The writer is this same test binary,
// re-executed.
func TestSaveInstanceIsNeverObservedHalfWrittenAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("HULL_TEST_STORE_WRITER"); dir != "" {
		// Child: hammer the record until killed.
		s, err := New(dir)
		if err != nil {
			os.Exit(1)
		}
		for i := 0; ; i++ {
			st := &InstanceState{
				ID: "racy", Status: "running", PID: 1000 + i,
				ImageDigest: strings.Repeat("x", 1+(i%512)),
			}
			if err := s.SaveInstance(st); err != nil {
				os.Exit(1)
			}
		}
	}

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("racy"); err != nil {
		t.Fatal(err)
	}
	// Seed one save, so a read that races the FIRST write is not counted as a
	// torn record. Without this the test reports "instance not found" for its
	// own reasons, which is how a genuine failure gets waved through.
	if err := s.SaveInstance(&InstanceState{ID: "racy", Status: "running", PID: 1}); err != nil {
		t.Fatal(err)
	}

	writer := exec.Command(os.Args[0],
		"-test.run", "TestSaveInstanceIsNeverObservedHalfWrittenAcrossProcesses")
	writer.Env = append(os.Environ(), "HULL_TEST_STORE_WRITER="+dir)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Process.Kill()
		_, _ = writer.Process.Wait()
	})

	var reads, misses int
	var firstMiss string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reads++
		if _, err := s.GetInstance("racy"); err != nil {
			misses++
			if firstMiss == "" {
				firstMiss = err.Error()
			}
			continue
		}
		list, lerr := s.ListInstances()
		if lerr != nil {
			misses++
			continue
		}
		found := false
		for _, in := range list {
			if in.ID == "racy" {
				found = true
			}
		}
		if !found {
			misses++
			if firstMiss == "" {
				firstMiss = "the instance vanished from ListInstances"
			}
		}
	}

	if firstMiss != "" {
		t.Logf("first miss: %s", firstMiss)
	}
	t.Logf("reads=%d misses=%d", reads, misses)
	if reads < 500 {
		t.Fatalf("only %d reads; the test is not exercising the race", reads)
	}
	if misses != 0 {
		t.Errorf("a record written by another process was observed half-written or "+
			"missing %d times out of %d reads", misses, reads)
	}
}

// Nothing may be left behind when a save is interrupted, and the record on disk
// must always parse.
func TestSaveInstanceLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("tidy"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := s.SaveInstance(&InstanceState{ID: "tidy", Status: "running", PID: i}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "instances", "tidy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary state file was left behind: %s", e.Name())
		}
	}
}

// The id is a directory name, a ps column and part of an argv. Everything
// admitted here has bitten somebody once already.
func TestValidateInstanceIDRejectsWhatItUsedTo(t *testing.T) {
	bad := map[string]string{
		"a\nb":                   "a newline forges a row in `hull ps`, which brig parses line by line",
		"a\rb":                   "a carriage return rewrites the ps line in place",
		"a\x1b[31mb":             "an escape reaches the terminal on a path the guest filter does not cover",
		"a b":                    "a space splits urunc's qemu argv",
		"a:b":                    "a colon splits urunc's qemu argv",
		"a\tb":                   "a tab breaks the ps table",
		"$(id)":                  "shell metacharacters in something that becomes an argv",
		"`id`":                   "the same",
		"-flag":                  "a leading dash becomes a flag",
		".hidden":                "a leading dot hides the instance directory",
		"a\u202eb":               "a bidi override reorders what the operator reads",
		strings.Repeat("x", 256): "longer than a path component may be",
	}
	for id, why := range bad {
		if err := ValidateInstanceID(id); err == nil {
			t.Errorf("ValidateInstanceID(%q) accepted it: %s", id, why)
		} else if !errors.Is(err, ErrInvalidInstanceID) {
			t.Errorf("ValidateInstanceID(%q) refused with the wrong error: %v", id, err)
		}
	}

	// And the ordinary names still work, including the 200-character one an
	// existing test pins.
	for _, id := range []string{"web", "web-1", "proj_web", "a", "0", "v1.2.3",
		strings.Repeat("x", 200)} {
		if err := ValidateInstanceID(id); err != nil {
			t.Errorf("ValidateInstanceID(%q) refused a legitimate name: %v", id, err)
		}
	}
}

// GetInstance must not read a record from outside the store.
//
// `hull restore '../../../../tmp/outside/evil'` found an attacker-placed
// state.json and hull executed its cmdLine[0].
func TestGetInstanceRefusesAPathInsteadOfAName(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	planted := filepath.Join(outside, "state.json")
	if err := os.WriteFile(planted,
		[]byte(`{"id":"evil","status":"running","pid":1,"cmdLine":["/tmp/payload-vz-runner"]}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	rel, err := filepath.Rel(filepath.Join(dir, "instances"), outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{rel, "../../etc", "/etc/passwd", "a/b"} {
		if st, err := s.GetInstance(id); err == nil {
			t.Errorf("GetInstance(%q) read a record from outside the store: %+v", id, st)
		} else if !errors.Is(err, ErrInvalidInstanceID) && !errors.Is(err, ErrInstanceNotFound) {
			t.Errorf("GetInstance(%q) failed for the wrong reason: %v", id, err)
		}
	}
}

// The path accessors cannot report an error, so they must clamp rather than
// escape.
func TestInstancePathsCannotEscapeTheStore(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "instances")
	for _, id := range []string{"../../evil", "a/b", "/etc", "..", "."} {
		for _, got := range []string{
			s.InstanceDir(id), s.InstanceBundleDir(id), s.InstanceLogFile(id),
		} {
			if !strings.HasPrefix(filepath.Clean(got), root+string(filepath.Separator)) {
				t.Errorf("a path for id %q escaped the store: %s", id, got)
			}
		}
	}
}
