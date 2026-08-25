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

import "testing"

const (
	refTestRepo        = "ghcr.io/nofireai/urunc-ubuntu"
	refTestTag         = refTestRepo + ":aarch64"
	refTestDigest      = "sha256:c408baae42f5c74c0661fbc20a289fd23d4322988e52c88cd54108e5c4c74893"
	refTestIndexDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// The split the listing needs: repository and tag as the user wrote them, and
// the digest when the reference pins one. A digest reference used to be split
// on the colon inside `@sha256:`, which turned the repository into
// `repo@sha256` and the tag into the hex.
func TestParseReferenceSplit(t *testing.T) {
	for _, tc := range []struct {
		raw, repo, tag, digest string
	}{
		{refTestTag, refTestRepo, "aarch64", ""},
		{refTestRepo, refTestRepo, "", ""},
		{refTestRepo + "@" + refTestIndexDigest, refTestRepo, "", refTestIndexDigest},
		// A tag in front of the digest is decoration: the digest is what pins.
		{refTestRepo + ":aarch64@" + refTestDigest, refTestRepo, "aarch64", refTestDigest},
		// A registry port carries a colon that is not a tag separator.
		{"localhost:5000/urunc-ubuntu", "localhost:5000/urunc-ubuntu", "", ""},
		{"localhost:5000/urunc-ubuntu:dev", "localhost:5000/urunc-ubuntu", "dev", ""},
		{"localhost:5000/urunc-ubuntu@" + refTestDigest, "localhost:5000/urunc-ubuntu", "", refTestDigest},
		// The split is textual, so what a reference pins is reported even
		// when it is not a digest a registry would accept.
		{refTestRepo + "@sha256:short", refTestRepo, "", "sha256:short"},
		// A bare digest is a store key, not a repository reference: it pins
		// nothing and is matched as a string.
		{refTestDigest, refTestDigest, "", ""},
		{"", "", "", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			ref := ParseReference(tc.raw)
			if ref.Raw != tc.raw {
				t.Errorf("Raw = %q, want %q", ref.Raw, tc.raw)
			}
			if ref.Repository != tc.repo {
				t.Errorf("Repository = %q, want %q", ref.Repository, tc.repo)
			}
			if ref.Tag != tc.tag {
				t.Errorf("Tag = %q, want %q", ref.Tag, tc.tag)
			}
			if ref.Digest != tc.digest {
				t.Errorf("Digest = %q, want %q", ref.Digest, tc.digest)
			}
		})
	}
}

// An image records the reference it was pulled under, so a stored reference
// equal to the one asked for is the same image whatever shape it has. The
// bare store key is accepted the same way.
func TestImageAnswersItsOwnReference(t *testing.T) {
	for _, stored := range []string{
		refTestTag,
		refTestRepo + "@" + refTestIndexDigest,
		"not a reference at all",
	} {
		img := &ImageMetadata{Ref: stored, Digest: refTestDigest}
		if !img.Answers(ParseReference(stored)) {
			t.Errorf("%q must answer the reference it was pulled under", stored)
		}
		if !img.Answers(ParseReference(refTestDigest)) {
			t.Errorf("%q must answer its own store key", stored)
		}
	}
}

// A digest reference is compared against both digests the store knows: the
// manifest digest that keys the record, and the index digest a pin of a
// multi-arch image names.
func TestImageAnswersDigestReference(t *testing.T) {
	img := &ImageMetadata{Ref: refTestTag, Digest: refTestDigest, IndexDigest: refTestIndexDigest}
	for _, ref := range []string{
		refTestRepo + "@" + refTestDigest,
		refTestRepo + "@" + refTestIndexDigest,
	} {
		if !img.Answers(ParseReference(ref)) {
			t.Errorf("%q must resolve against the stored digests", ref)
		}
	}
}

// A digest names bytes, not an image, and the same manifest can be pushed to
// several repositories. A pin must not be satisfied by an image the user did
// not name.
func TestImageRefusesDigestFromAnotherRepository(t *testing.T) {
	img := &ImageMetadata{Ref: refTestTag, Digest: refTestDigest, IndexDigest: refTestIndexDigest}
	for _, ref := range []string{
		"ghcr.io/someone-else/urunc-ubuntu@" + refTestDigest,
		"ghcr.io/someone-else/urunc-ubuntu@" + refTestIndexDigest,
		"ghcr.io/nofireai/other-image@" + refTestDigest,
		refTestRepo + "@sha256:4444444444444444444444444444444444444444444444444444444444444444",
	} {
		if img.Answers(ParseReference(ref)) {
			t.Errorf("%q must not be answered by this image", ref)
		}
	}
}

// A tag that is not the stored one is a miss: there is no digest to fall
// through to, and a tag is a moving target the store cannot vouch for.
func TestImageRefusesOtherTag(t *testing.T) {
	img := &ImageMetadata{Ref: refTestTag, Digest: refTestDigest}
	for _, ref := range []string{
		refTestRepo + ":latest",
		refTestRepo,
		"ghcr.io/nofireai/does-not-exist:aarch64",
	} {
		if img.Answers(ParseReference(ref)) {
			t.Errorf("%q must not be answered by an image pulled under %q", ref, refTestTag)
		}
	}
}

// A reference that does not parse, on either side, cannot be shown to name
// the same repository as the other, so the pin misses and the image is
// re-pulled rather than trusted.
func TestUnparseableReferenceRefusesDigestPin(t *testing.T) {
	img := &ImageMetadata{Ref: "not a reference at all", Digest: refTestDigest}
	if img.Answers(ParseReference(refTestRepo + "@" + refTestDigest)) {
		t.Error("an unparseable stored reference must not satisfy a digest pin")
	}
	img = &ImageMetadata{Ref: refTestTag, Digest: refTestDigest}
	if img.Answers(ParseReference("not a reference@" + refTestDigest)) {
		t.Error("an unparseable digest reference must not be answered")
	}
}

func TestSameRepository(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{refTestTag, refTestRepo + "@" + refTestDigest, true},
		{refTestRepo, refTestRepo + ":other", true},
		{refTestTag, "ghcr.io/nofireai/other-image:aarch64", false},
		{refTestTag, "ghcr.io/someone-else/urunc-ubuntu:aarch64", false},
		{"not a reference", refTestTag, false},
		{refTestTag, "not a reference", false},
	} {
		if got := SameRepository(tc.a, tc.b); got != tc.want {
			t.Errorf("SameRepository(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Records written before the index digest was tracked carry none, so an image
// pulled under an index digest reference has only that reference to be found
// by. The string comparison is what keeps it findable.
func TestLegacyRecordAnswersItsIndexDigestReference(t *testing.T) {
	ref := refTestRepo + "@" + refTestIndexDigest
	img := &ImageMetadata{Ref: ref, Digest: refTestDigest}
	if !img.Answers(ParseReference(ref)) {
		t.Error("a record with no index digest must still answer the reference it was pulled under")
	}
}
