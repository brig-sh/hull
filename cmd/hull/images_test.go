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
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

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
