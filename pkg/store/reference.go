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
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Reference is an image reference parsed once, by ParseReference, into the
// parts the store matches by and a listing prints. Build it with
// ParseReference only: Answers reads a canonical repository name that nothing
// else fills in.
//
// Repository, Tag and Digest are the text as written, split at the right
// places: the digest after `@` comes off first, then a tag after the last
// colon that follows the last slash, so a registry port is never mistaken for
// a tag.
type Reference struct {
	Raw        string
	Repository string
	Tag        string
	Digest     string

	// repository is the canonical registry/repository name, empty when Raw
	// does not parse as a reference.
	repository string
}

// ParseReference splits a reference into its parts. It never fails: an
// unparseable reference keeps its raw text and matches only a record pulled
// under that exact text.
//
// A bare `sha256:...` is the store key rather than a repository reference. It
// deliberately stays whole, with no tag and no digest, so that it is matched
// as a string: it names no repository to check, and it has always meant the
// manifest digest.
func ParseReference(raw string) Reference {
	ref := Reference{Raw: raw, Repository: raw}
	if strings.HasPrefix(raw, "sha256:") {
		return ref
	}
	if i := strings.Index(raw, "@"); i >= 0 {
		ref.Repository, ref.Digest = raw[:i], raw[i+1:]
	}
	if c := strings.LastIndex(ref.Repository, ":"); c > strings.LastIndex(ref.Repository, "/") {
		ref.Tag = ref.Repository[c+1:]
		ref.Repository = ref.Repository[:c]
	}
	ref.repository, _ = canonicalRepository(raw)
	return ref
}

// canonicalRepository is the registry/repository a reference names, in the
// form two spellings of the same repository share. A reference that does not
// parse names nothing.
func canonicalRepository(ref string) (string, bool) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", false
	}
	return parsed.Context().Name(), true
}

// SameRepository reports whether two references name the same repository. A
// reference that does not parse cannot be shown to match anything.
func SameRepository(a, b string) bool {
	first, ok := canonicalRepository(a)
	if !ok {
		return false
	}
	second, ok := canonicalRepository(b)
	return ok && first == second
}

// digestPrefixPattern matches the leading part of a digest: an algorithm, a
// colon, and at least six hex characters.
//
// Six is the shortest prefix worth accepting. It is not a claim about
// collisions: the caller reports an ambiguous prefix as ambiguous.
var digestPrefixPattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]{6,}$`)

// IsDigestPrefix reports whether raw could name an image by the front of its
// digest. A full digest matches this too; it is simply matched as a whole
// before anything gets here.
func IsDigestPrefix(raw string) bool {
	return digestPrefixPattern.MatchString(raw)
}

// HasDigestPrefix reports whether either digest this record carries starts
// with raw.
//
// The index digest is included because that is the digest a user pinning a
// multi-arch image sees in a registry, and `hull images --json` prints it. It
// keys nothing on disk, so a match there still resolves to this record's
// manifest digest, which is the directory.
func (m *ImageMetadata) HasDigestPrefix(raw string) bool {
	return strings.HasPrefix(m.Digest, raw) ||
		(m.IndexDigest != "" && strings.HasPrefix(m.IndexDigest, raw))
}

// Answers reports whether this image is what the reference asked for.
//
// The plain string comparison comes first. An image records the reference it
// was pulled under, so a stored reference equal to the one asked for is the
// same image by definition, whatever shape it has, and the bare store key is
// accepted the same way. A tag is matched only this way, as written. Records
// written before the index digest was tracked depend on the string
// comparison: one pulled under `repo@sha256:<index digest>` carries no index
// digest to compare, and without it the record would be a permanent miss,
// re-pulled on every run and refused under --pull=never.
//
// A digest reference cannot be compared as a string against either stored
// field: Ref is normally a tag, and Digest is the bare digest without the
// repository the reference carries in front of it. So the digest is compared
// against both digests the store knows. The manifest digest is the store key;
// the index digest is what a pin of a multi-arch image names, and it never
// keys anything on disk.
//
// The repository has to match as well. A digest identifies bytes, not an
// image, and the same manifest can be pushed to two repositories, so without
// this check a pin could be satisfied by an image the user did not name.
// Either side failing to parse is a miss, and the image is re-pulled.
func (m *ImageMetadata) Answers(ref Reference) bool {
	if m.Ref == ref.Raw || m.Digest == ref.Raw {
		return true
	}
	if ref.Digest == "" || ref.repository == "" {
		return false
	}
	if ref.Digest != m.Digest && ref.Digest != m.IndexDigest {
		return false
	}
	stored, ok := canonicalRepository(m.Ref)
	return ok && stored == ref.repository
}
