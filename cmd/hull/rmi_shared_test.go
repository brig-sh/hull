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
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// A digest prefix is ambiguous when it matches more than one digest, not more
// than one record. On Linux two names of one image are two records with the
// same digest, and a prefix of it names that one image.
func TestPickRemovalsJudgesAmbiguityByDigest(t *testing.T) {
	const a = "sha256:aaaaaa1111111111111111111111111111111111111111111111111111111111"
	const b = "sha256:aaaaaa2222222222222222222222222222222222222222222222222222222222"
	keepAll := func(images []*store.ImageMetadata) []*store.ImageMetadata { return images }

	sameImage := []*store.ImageMetadata{{Ref: "example.com/x:1", Digest: a}, {Ref: "example.com/x:2", Digest: a}}
	got, err := pickRemovals("sha256:aaaaaa1", "", sameImage, true, keepAll)
	if err != nil || len(got) != 2 {
		t.Errorf("two names of one digest = %d match(es), %v; want both, no error", len(got), err)
	}

	twoImages := []*store.ImageMetadata{{Ref: "example.com/x:1", Digest: a}, {Ref: "example.com/y:1", Digest: b}}
	_, err = pickRemovals("sha256:aaaaaa", "", twoImages, true, keepAll)
	if err == nil || !strings.Contains(err.Error(), "is ambiguous: it matches "+a+", "+b) {
		t.Errorf("two digests = %v, want an ambiguity listing both once", err)
	}
}
