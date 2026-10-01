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
	"path/filepath"
	"strings"
	"testing"
)

// A name with no directory is absence. A directory whose state.json is
// missing or not JSON is a record we failed to read. Callers match the
// words "instance not found" when they decide a VM is gone, so the second
// case must not use them.
func TestGetInstanceSeparatesAbsenceFromAnUnreadableRecord(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstance("missing")
	if !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("missing name: %v", err)
	}

	if _, err := s.CreateInstance("empty"); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetInstance("empty")
	if !errors.Is(err, ErrInstanceStateUnreadable) || errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("directory without a state file: %v", err)
	}
	if strings.Contains(err.Error(), "instance not found") {
		t.Fatalf("missing state file uses the absence sentence: %v", err)
	}

	if _, err := s.CreateInstance("corrupt"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.InstanceDir("corrupt"), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetInstance("corrupt")
	if !errors.Is(err, ErrInstanceStateUnreadable) || errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("corrupt state: %v", err)
	}
	if strings.Contains(err.Error(), "instance not found") {
		t.Fatalf("corrupt record uses the absence sentence: %v", err)
	}
}

// ps lists whatever ListInstances returns. Dropping a directory here is what
// made a broken record look the same as a VM that had been removed.
func TestListInstancesKeepsAnUnreadableName(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance("good"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&InstanceState{ID: "good", Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.InstanceDir("corrupt"), "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, inst := range listed {
		got[inst.ID] = inst.Status
	}
	if got["good"] != "stopped" || got["empty"] != StatusUnreadable || got["corrupt"] != StatusUnreadable {
		t.Fatalf("listing = %v", got)
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("a name with no directory was listed")
	}
}
