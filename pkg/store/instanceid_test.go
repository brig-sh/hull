// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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

func TestValidateInstanceID(t *testing.T) {
	valid := []string{"web", "web-1", "proj_web", "a", "0", strings.Repeat("x", 200)}
	for _, id := range valid {
		if err := ValidateInstanceID(id); err != nil {
			t.Errorf("ValidateInstanceID(%q) = %v, want nil", id, err)
		}
	}
	invalid := []string{"", ".", "..", "../pwn", "../../pwn", "a/b", "/abs",
		"sub/dir", `back\slash`, "trailing/", "nul\x00byte"}
	for _, id := range invalid {
		if err := ValidateInstanceID(id); err == nil {
			t.Errorf("ValidateInstanceID(%q) = nil, want an error", id)
		} else if !errors.Is(err, ErrInvalidInstanceID) {
			t.Errorf("ValidateInstanceID(%q) error = %v, want ErrInvalidInstanceID", id, err)
		}
	}
}

// The containment regression: `run --name ../../pwn` used to create a whole
// instance (bundle, rootfs, state, log) outside the store, where ps cannot
// see it. The store owns its layout, so it refuses the id itself.
func TestCreateInstanceRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "..", "escaped-instance")
	if _, err := s.CreateInstance("../escaped-instance"); err == nil {
		t.Fatal("CreateInstance accepted a traversing id")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("a directory was created outside the store at %s", outside)
	}
}

// DeleteInstance recurses, so it must refuse a traversing id even though
// callers resolve the record first.
func TestDeleteInstanceRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "..", "precious-"+filepath.Base(root))
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(victim) }()
	keep := filepath.Join(victim, "keepme")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	rel, err := filepath.Rel(filepath.Join(root, "instances"), victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInstance(rel); err == nil {
		t.Fatal("DeleteInstance accepted a traversing id")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("DeleteInstance removed a file outside the store: %v", err)
	}
}
