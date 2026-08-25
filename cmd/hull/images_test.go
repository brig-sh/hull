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

//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// The JSON form from #14: what the store knows, with nothing cut. The table
// truncates the manifest digest and never shows the index digest, which is
// the one a caller comparing against a registry needs.
func TestImageJSONCarriesFullDigests(t *testing.T) {
	pulled := time.Date(2026, 8, 25, 20, 1, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := writeImageJSON(&buf, []*store.ImageMetadata{
		{
			Ref:         cacheTestRef,
			Digest:      cacheTestDigest,
			IndexDigest: cacheTestIndexDigest,
			Platform:    "linux/arm64",
			Size:        583_600_000,
			PulledAt:    pulled,
		},
		// Single-arch, or pulled before the index digest was recorded.
		{Ref: cacheTestRef, Digest: cacheTestDigest, PulledAt: pulled},
	})
	if err != nil {
		t.Fatalf("writeImageJSON: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("output is not a JSON list: %v\n%s", err, buf.String())
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	with := entries[0]
	for key, want := range map[string]any{
		"ref":         cacheTestRef,
		"digest":      cacheTestDigest,
		"indexDigest": cacheTestIndexDigest,
		"platform":    "linux/arm64",
		"size":        float64(583_600_000),
		"pulledAt":    pulled.Format(time.RFC3339),
	} {
		if got := with[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}

	// The key is absent, not empty, so a caller can tell "none recorded"
	// from a record that says so.
	without := entries[1]
	if _, ok := without["indexDigest"]; ok {
		t.Errorf("indexDigest must be omitted when none was recorded, got %v", without["indexDigest"])
	}
	if _, ok := without["platform"]; ok {
		t.Errorf("platform must be omitted on a record from before the field existed, got %v", without["platform"])
	}
	if got := without["digest"]; got != cacheTestDigest {
		t.Errorf("digest = %v, want the full %q", got, cacheTestDigest)
	}
}

// A machine reading the listing gets a list either way; "No images found"
// is for people.
func TestImageJSONEmptyStoreIsAList(t *testing.T) {
	var buf bytes.Buffer
	if err := writeImageJSON(&buf, nil); err != nil {
		t.Fatalf("writeImageJSON: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("empty store printed %q, want []", got)
	}
}

// The listing split from #12. An image pulled by digest records
// `repo@sha256:<hex>` as its reference, and the split used to scan backwards
// for a colon and land on the one inside `@sha256:`, printing a repository
// ending in `@sha256` and a 64-character tag.
func TestImageTableSplitsReference(t *testing.T) {
	for _, tc := range []struct {
		ref, repo, tag string
	}{
		{cacheTestRef, cacheTestRepo, "aarch64"},
		{cacheTestRepo + "@" + cacheTestIndexDigest, cacheTestRepo, "<none>"},
		// A reference with no tag means latest, and a registry port is
		// not a tag.
		{cacheTestRepo, cacheTestRepo, "latest"},
		{"localhost:5000/urunc-ubuntu", "localhost:5000/urunc-ubuntu", "latest"},
		// Nothing checks image.json on the way in, so a record whose
		// reference pins something that is not a digest still says it
		// pins rather than claiming latest.
		{cacheTestRepo + "@sha256:short", cacheTestRepo, "<none>"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeImageTable(&buf, []*store.ImageMetadata{{
				Ref:      tc.ref,
				Digest:   cacheTestDigest,
				PulledAt: time.Now(),
			}}, time.Now())
			if err != nil {
				t.Fatalf("writeImageTable: %v", err)
			}
			lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d lines, want a header and one row:\n%s", len(lines), buf.String())
			}
			row := strings.Fields(lines[1])
			// The digest column is the manifest digest cut to nineteen
			// characters, as it always was, whatever the reference.
			want := []string{tc.repo, tc.tag, cacheTestDigest[:19]}
			for i, col := range want {
				if row[i] != col {
					t.Errorf("column %d = %q, want %q (row: %s)", i, row[i], col, lines[1])
				}
			}
		})
	}
}
