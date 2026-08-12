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

package conformance

import (
	"regexp"
	"strings"
	"testing"
)

const manifestPath = "capabilities.yaml"

// idPattern: <namespace>.<key>, where namespace is one of the known prefixes
// and key is the schema/verb/feature name (lowercase alnum plus _ . -).
// "spec" covers spec-wide features with no schema key of their own, like
// variable interpolation.
var idPattern = regexp.MustCompile(`^(toplevel|service|cli|ext|spec)\.[a-z0-9][a-z0-9_.-]*$`)

func loadForTest(t *testing.T) *Manifest {
	t.Helper()
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Capabilities) == 0 {
		t.Fatal("manifest has no capabilities")
	}
	return m
}

func TestManifestWellFormed(t *testing.T) {
	m := loadForTest(t)

	validStatus := map[Status]bool{
		StatusSupported:   true,
		StatusPartial:     true,
		StatusUnsupported: true,
		StatusOutOfScope:  true,
	}
	validTier := map[Tier]bool{
		TierStatic:  true,
		TierRuntime: true,
	}

	seen := map[string]bool{}
	for i, c := range m.Capabilities {
		where := c.ID
		if where == "" {
			where = "index " + itoa(i)
		}

		// ids unique and well-formed
		if c.ID == "" {
			t.Errorf("%s: empty id", where)
			continue
		}
		if !idPattern.MatchString(c.ID) {
			t.Errorf("%s: id not well-formed (want <namespace>.<key>)", c.ID)
		}
		if seen[c.ID] {
			t.Errorf("%s: duplicate id", c.ID)
		}
		seen[c.ID] = true

		// area present
		if strings.TrimSpace(c.Area) == "" {
			t.Errorf("%s: missing area", c.ID)
		}

		// status/tier enums valid
		if !validStatus[c.Status] {
			t.Errorf("%s: invalid status %q", c.ID, c.Status)
		}
		if !validTier[c.Tier] {
			t.Errorf("%s: invalid tier %q", c.ID, c.Tier)
		}

		// test present for every entry, and consistent with tier + id
		if strings.TrimSpace(c.Test) == "" {
			t.Errorf("%s: missing test", c.ID)
		} else {
			wantPrefix := "TestConformanceStatic/"
			if c.Tier == TierRuntime {
				wantPrefix = "TestConformanceRuntime/"
			}
			want := wantPrefix + c.ID
			if c.Test != want {
				t.Errorf("%s: test %q does not match tier %q (want %q)", c.ID, c.Test, c.Tier, want)
			}
		}

		// security mandatory for supported/partial, non-empty
		if c.Status == StatusSupported || c.Status == StatusPartial {
			if strings.TrimSpace(c.Security) == "" {
				t.Errorf("%s: %s entry must carry a non-empty security field", c.ID, c.Status)
			}
		}

		// notes mandatory for partial and unsupported
		if c.Status == StatusPartial || c.Status == StatusUnsupported {
			if strings.TrimSpace(c.Notes) == "" {
				t.Errorf("%s: %s entry must carry non-empty notes", c.ID, c.Status)
			}
		}

		// out-of-scope entries must name their ADR-0003 category, so every
		// exclusion stays auditable against the ADR's five reasons.
		if c.Status == StatusOutOfScope {
			if !hasOutOfScopeCategory(c.Notes) {
				t.Errorf("%s: out-of-scope entry's notes must name one of the ADR-0003 categories %v", c.ID, outOfScopeCategories)
			}
		}
	}
}

// outOfScopeCategories are the ADR-0003 exclusion reasons; every out-of-scope
// entry's notes must contain one of them.
var outOfScopeCategories = []string{
	"orchestration",
	"cross-VM namespace sharing",
	"Windows containers only",
	"Docker-platform machinery",
	"no device passthrough",
}

func hasOutOfScopeCategory(notes string) bool {
	for _, c := range outOfScopeCategories {
		if strings.Contains(notes, c) {
			return true
		}
	}
	return false
}

// itoa avoids pulling strconv into the test for a single conversion.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
