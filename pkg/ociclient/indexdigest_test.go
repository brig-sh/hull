// Copyright (c) 2023-2026, Nubificus LTD
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

package ociclient

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// platformImage builds a one-layer image that declares os and arch, so an index
// entry for it is selectable by platform.
func platformImage(t *testing.T, content, os, arch string) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, testLayer(t, content))
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	config = config.DeepCopy()
	config.OS = os
	config.Architecture = arch
	img, err = mutate.ConfigFile(img, config)
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	return img
}

// testIndexRegistry serves a two-platform image index and returns its tagged
// reference, the index digest and the arm64 manifest digest.
func testIndexRegistry(t *testing.T) (ref, indexDigest, armDigest string) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	tagged, err := name.ParseReference(u.Host+"/urunc/multiarch:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}

	arm := platformImage(t, "hello from arm64", "linux", "arm64")
	amd := platformImage(t, "hello from amd64", "linux", "amd64")
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        arm,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
		mutate.IndexAddendum{
			Add:        amd,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		},
	)
	if err := remote.WriteIndex(tagged, index); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}

	idxDigest, err := index.Digest()
	if err != nil {
		t.Fatalf("index digest: %v", err)
	}
	manifestDigest, err := arm.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	return tagged.String(), idxDigest.String(), manifestDigest.String()
}

// The store is keyed by the per-platform manifest digest, but a user pinning a
// multi-arch image pins the index digest. Recording it with the image is what
// lets that pin resolve locally instead of going back to the registry.
func TestPullRecordsIndexDigest(t *testing.T) {
	ref, indexDigest, armDigest := testIndexRegistry(t)
	c, s := newClient(t)

	res, err := c.PullPlatform(context.Background(), ref, DefaultPlatform)
	if err != nil {
		t.Fatalf("PullPlatform: %v", err)
	}
	if res.Digest != armDigest {
		t.Fatalf("pulled digest = %q, want the arm64 manifest %q", res.Digest, armDigest)
	}
	metadata, err := s.GetImage(res.Digest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if metadata.IndexDigest != indexDigest {
		t.Errorf("IndexDigest = %q, want %q", metadata.IndexDigest, indexDigest)
	}
}

// Both platform variants come from one index, so both records carry the same
// index digest. That is what makes a pinned index digest resolvable per
// platform.
func TestPullRecordsIndexDigestForEachPlatform(t *testing.T) {
	ref, indexDigest, _ := testIndexRegistry(t)
	c, s := newClient(t)

	digests := map[string]string{}
	for _, platform := range []string{DefaultPlatform, "linux/amd64"} {
		res, err := c.PullPlatform(context.Background(), ref, platform)
		if err != nil {
			t.Fatalf("PullPlatform(%s): %v", platform, err)
		}
		digests[platform] = res.Digest
		metadata, err := s.GetImage(res.Digest)
		if err != nil {
			t.Fatalf("GetImage: %v", err)
		}
		if metadata.IndexDigest != indexDigest {
			t.Errorf("%s: IndexDigest = %q, want %q", platform, metadata.IndexDigest, indexDigest)
		}
		if metadata.Platform != platform {
			t.Errorf("%s: Platform = %q", platform, metadata.Platform)
		}
	}
	if digests[DefaultPlatform] == digests["linux/amd64"] {
		t.Error("the two platforms must be stored under different manifest digests")
	}
}

// A pull that does not go through an index learns no index digest, and every
// pull rewrites image.json in full. Pulling by manifest digest after a pull by
// tag must not erase the index digest the tag pull recorded, or the pin the
// user set stops resolving.
func TestPullByManifestDigestKeepsIndexDigest(t *testing.T) {
	ref, indexDigest, armDigest := testIndexRegistry(t)
	c, s := newClient(t)

	if _, err := c.PullPlatform(context.Background(), ref, DefaultPlatform); err != nil {
		t.Fatalf("PullPlatform: %v", err)
	}

	repo, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	manifestRef := repo.Context().Name() + "@" + armDigest
	if _, err := c.PullPlatform(context.Background(), manifestRef, DefaultPlatform); err != nil {
		t.Fatalf("PullPlatform by manifest digest: %v", err)
	}

	metadata, err := s.GetImage(armDigest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if metadata.IndexDigest != indexDigest {
		t.Errorf("IndexDigest = %q, want the index digest %q to survive", metadata.IndexDigest, indexDigest)
	}
	if metadata.Ref != manifestRef {
		t.Errorf("Ref = %q, want the reference of the latest pull %q", metadata.Ref, manifestRef)
	}
}

// A single-arch image has no index in front of it, so there is nothing to
// record and nothing to invent.
func TestPullSingleArchRecordsNoIndexDigest(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	res, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	metadata, err := s.GetImage(res.Digest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if metadata.IndexDigest != "" {
		t.Errorf("IndexDigest = %q, want empty", metadata.IndexDigest)
	}
}

// An index digest is only true for the repository that hosts that index. The
// same manifest can be pushed to two repositories, and the store keys on the
// manifest digest, so both pulls land on one record -- carrying the index
// digest over would let a pin be answered by a repository that never hosted
// that index, which is what the lookup's repository check exists to prevent.
func TestPullDoesNotCarryIndexDigestAcrossRepositories(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	arm := platformImage(t, "hello from arm64", "linux", "arm64")
	armDigest, err := arm.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}

	// The first repository serves the image behind an index.
	first, err := name.ParseReference(u.Host+"/urunc/first:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add:        arm,
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
	})
	if err := remote.WriteIndex(first, index); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}

	// The second serves the same manifest with no index in front of it.
	second, err := name.ParseReference(u.Host+"/urunc/second:latest", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.Write(second, arm); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	c, s := newClient(t)
	if _, err := c.PullPlatform(context.Background(), first.String(), DefaultPlatform); err != nil {
		t.Fatalf("PullPlatform: %v", err)
	}
	secondRef := second.Context().Name() + "@" + armDigest.String()
	if _, err := c.PullPlatform(context.Background(), secondRef, DefaultPlatform); err != nil {
		t.Fatalf("PullPlatform from the second repository: %v", err)
	}

	metadata, err := s.GetImage(armDigest.String())
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if metadata.IndexDigest != "" {
		t.Errorf("IndexDigest = %q, want none carried over from another repository", metadata.IndexDigest)
	}
}
