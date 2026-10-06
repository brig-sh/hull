// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceLabelsStayWithTheirCreation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance("sandbox"); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"sh.brig.network.requested": "shared", "sh.brig.network.effective": "isolated"}
	state := &InstanceState{ID: "sandbox", CreationID: "first", Labels: labels, Status: StatusStarting}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	// Open a fresh Store to rule out a result held only in process memory.
	reader, err := New(s.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "stopped"} {
		got, err := reader.GetInstance("sandbox")
		if err != nil {
			t.Fatal(err)
		}
		if got.CreationID != "first" || !maps.Equal(got.Labels, labels) {
			t.Fatalf("metadata lost after %s: %+v", state.Status, got)
		}
		got.Status = status
		if err := reader.SaveInstance(got); err != nil {
			t.Fatal(err)
		}
		state = got
	}
	if err := s.DeleteInstance("sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&InstanceState{ID: "sandbox", CreationID: "second"}); err != nil {
		t.Fatal(err)
	}
	got, err := reader.GetInstance("sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreationID != "second" || len(got.Labels) != 0 {
		t.Fatalf("reused name inherited the old metadata: %+v", got)
	}
}

func TestLegacyInstanceDoesNotAcquireLabelsOrIdentity(t *testing.T) {
	s := newTestStore(t)
	dir, err := s.CreateInstance("legacy")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "state.json")
	if err := os.WriteFile(file, []byte(`{"id":"legacy","status":"stopped"}`), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetInstance("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if state.CreationID != "" || state.Labels != nil {
		t.Fatalf("legacy metadata was invented: %+v", state)
	}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"creationId", "labels"} {
		if _, ok := fields[key]; ok {
			t.Errorf("legacy record acquired %s: %s", key, data)
		}
	}
}
