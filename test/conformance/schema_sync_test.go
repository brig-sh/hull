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
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

const specPath = "spec/compose-spec.json"

// composeSchema is the minimal shape of the pinned compose-spec JSON schema
// that the sync guard needs: the top-level .properties key set and the
// service key set under $defs.service.properties.
type composeSchema struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Defs       struct {
		Service struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"service"`
	} `json:"$defs"`
}

func loadSchema(t *testing.T) *composeSchema {
	t.Helper()
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s composeSchema
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	if len(s.Properties) == 0 {
		t.Fatalf("schema has no top-level properties")
	}
	if len(s.Defs.Service.Properties) == 0 {
		t.Fatalf("schema has no service properties under $defs.service")
	}
	return &s
}

// TestManifestCoversSpecSchema enforces both directions of the schema<->
// manifest mapping:
//
//	forward: every schema key (toplevel + service) has a manifest entry
//	reverse: every manifest toplevel.*/service.* id exists in the schema
//
// The cli.* and ext.* namespaces have no schema counterpart and are exempt
// from the reverse check.
func TestManifestCoversSpecSchema(t *testing.T) {
	schema := loadSchema(t)
	m := loadForTest(t)

	ids := map[string]bool{}
	for _, c := range m.Capabilities {
		ids[c.ID] = true
	}

	// Forward: schema -> manifest.
	var missing []string
	for key := range schema.Properties {
		if !ids["toplevel."+key] {
			missing = append(missing, "toplevel."+key)
		}
	}
	for key := range schema.Defs.Service.Properties {
		if !ids["service."+key] {
			missing = append(missing, "service."+key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("schema keys with no manifest entry (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	// Reverse: manifest toplevel.*/service.* -> schema.
	var stray []string
	for _, c := range m.Capabilities {
		switch {
		case strings.HasPrefix(c.ID, "toplevel."):
			key := strings.TrimPrefix(c.ID, "toplevel.")
			if _, ok := schema.Properties[key]; !ok {
				stray = append(stray, c.ID)
			}
		case strings.HasPrefix(c.ID, "service."):
			key := strings.TrimPrefix(c.ID, "service.")
			if _, ok := schema.Defs.Service.Properties[key]; !ok {
				stray = append(stray, c.ID)
			}
			// cli.*, ext.* and spec.* have no schema counterpart.
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("manifest ids in toplevel.*/service.* with no schema key (%d):\n  %s",
			len(stray), strings.Join(stray, "\n  "))
	}
}
