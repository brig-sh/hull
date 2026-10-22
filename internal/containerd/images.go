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

//go:build linux

package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image is an image containerd holds under a name, described by the
// manifest of one platform.
type Image struct {
	// Name is the reference the image is stored under.
	Name string
	// Digest is the digest of the platform manifest.
	Digest string
	// IndexDigest is the digest of the multi-platform index the name points
	// at, or empty when the name points straight at a manifest.
	IndexDigest string
	// Platform is os/arch[/variant] from the image config, or empty when the
	// config names no OS.
	Platform string
	// Size is the sum of the manifest's compressed layer sizes.
	Size int64
	// Labels are the labels in the image config.
	Labels map[string]string
	// UpdatedAt is when the name was last pointed at its target.
	UpdatedAt time.Time
}

// DescribeImage reads img through to the manifest of one platform, chosen by
// pickManifest when the name points at an index, and describes the image by
// that manifest and its config.
//
// On error the returned Image still carries the name, UpdatedAt and a
// digest: the one the name points at, or the platform manifest's once one is
// chosen. A caller can still show an image whose content is damaged or
// missing.
func DescribeImage(ctx context.Context, cs content.Store, img images.Image, pref platforms.MatchComparer) (Image, error) {
	return describeImage(ctx, cs, img, pref, false)
}

// DescribePlatform is DescribeImage limited to the manifests of an index a
// pull for platform can have fetched, which pulledManifests picks out. A
// pull describes the image it has just fetched this way, because a
// successful pull can leave that platform's layers missing:
// containerd's unpacker does not download a layer whose snapshot already
// exists (core/unpack/unpacker.go). DescribeImage would then rank another
// platform, complete from an earlier pull, ahead of the one pulled.
func DescribePlatform(ctx context.Context, cs content.Store, img images.Image, platform platforms.MatchComparer) (Image, error) {
	return describeImage(ctx, cs, img, platform, true)
}

func describeImage(ctx context.Context, cs content.Store, img images.Image, pref platforms.MatchComparer, onlyPref bool) (Image, error) {
	out := Image{
		Name:      img.Name,
		Digest:    img.Target.Digest.String(),
		UpdatedAt: img.UpdatedAt,
	}
	manifestDesc := img.Target
	if images.IsIndexType(img.Target.MediaType) {
		var idx ocispec.Index
		err := readJSON(ctx, cs, img.Target, &idx)
		if err != nil {
			return out, fmt.Errorf("read index: %w", err)
		}
		manifests := idx.Manifests
		if onlyPref {
			manifests = pulledManifests(idx.Manifests, pref)
		}
		picked, err := pickManifest(ctx, cs, manifests, pref)
		if err != nil {
			return out, err
		}
		manifestDesc = picked
		out.IndexDigest = img.Target.Digest.String()
		out.Digest = picked.Digest.String()
	}
	if !images.IsManifestType(manifestDesc.MediaType) {
		return out, fmt.Errorf("unsupported media type %s", manifestDesc.MediaType)
	}

	var manifest ocispec.Manifest
	err := readJSON(ctx, cs, manifestDesc, &manifest)
	if err != nil {
		return out, fmt.Errorf("read manifest: %w", err)
	}
	for _, layer := range manifest.Layers {
		out.Size += layer.Size
	}

	var config ocispec.Image
	err = readJSON(ctx, cs, manifest.Config, &config)
	if err != nil {
		return out, fmt.Errorf("read config: %w", err)
	}
	out.Labels = config.Config.Labels
	if config.OS != "" {
		out.Platform = platforms.Format(config.Platform)
	}
	return out, nil
}

// pulledManifests returns the entries of an index that a pull for platform
// can have fetched: those whose platform matches, or, when none does, those
// that name no platform. The platform field of an entry is optional.
// containerd's pull keeps an entry without one (images.FilterPlatforms) but
// sorts it after every entry that has one (images.LimitManifests), and
// fetches only the first, so it fetches an entry without a platform only
// when no entry matches. Offering both kinds whenever both are present would
// let an earlier pull's complete entry without a platform outrank the
// matching one this pull fetched.
func pulledManifests(manifests []ocispec.Descriptor, platform platforms.Matcher) []ocispec.Descriptor {
	var matching, unnamed []ocispec.Descriptor
	for _, m := range manifests {
		switch {
		case m.Platform == nil:
			unnamed = append(unnamed, m)
		case platform.Match(*m.Platform):
			matching = append(matching, m)
		}
	}
	if len(matching) > 0 {
		return matching
	}
	return unnamed
}

// pickManifest chooses, among an index's manifests, the one pref ranks
// highest of those present in the content store.
//
// A manifest being present is not enough. ctr and the transfer service fetch
// the manifest and config of every platform in an index, but the layers of
// only the platform asked for, so a pull of linux/arm64 on an amd64 host
// leaves an amd64 manifest behind with nothing under it. A platform is
// complete when its config and every layer are present too, and complete
// platforms rank first.
//
// After that, platforms that match pref rank before those that don't, so an
// image pulled for another platform than the host's is described as that
// platform. A manifest that is present but incomplete is a candidate only
// when no platform is complete, which is what a containerd that discards
// layers once it has unpacked them leaves behind. Entries that are not
// images, which isImage tells apart, are never candidates.
//
// images.Check reports missing content as unavailable, not as an error, so
// an error means the entry could not be checked: its manifest is malformed,
// or the store failed to answer. Either way that entry is skipped, so one bad
// entry does not stop the rest of the index from being described. If no
// entry is usable, the first such error is returned rather than a claim that
// the content is missing: a daemon that fails every check is then reported
// as failing. A context that is done fails every later check too, so it ends
// the search at once.
func pickManifest(ctx context.Context, cs content.Store, manifests []ocispec.Descriptor, pref platforms.MatchComparer) (ocispec.Descriptor, error) {
	type candidate struct {
		desc     ocispec.Descriptor
		complete bool
		matches  bool
	}
	var present []candidate
	var checkErr error
	for _, m := range manifests {
		if !images.IsManifestType(m.MediaType) || !isImage(m) {
			continue
		}
		available, _, _, missing, err := images.Check(ctx, cs, m, platforms.All)
		if err != nil {
			if ctx.Err() != nil {
				return ocispec.Descriptor{}, ctx.Err()
			}
			if checkErr == nil {
				checkErr = err
			}
			continue
		}
		if !available {
			continue
		}
		present = append(present, candidate{
			desc:     m,
			complete: len(missing) == 0,
			matches:  m.Platform != nil && pref.Match(*m.Platform),
		})
	}
	if len(present) == 0 {
		if checkErr != nil {
			return ocispec.Descriptor{}, checkErr
		}
		return ocispec.Descriptor{}, fmt.Errorf("no image manifest of the index is in the content store")
	}
	sort.SliceStable(present, func(i, j int) bool {
		a, b := present[i], present[j]
		if a.complete != b.complete {
			return a.complete
		}
		if a.matches != b.matches {
			return a.matches
		}
		return a.matches && pref.Less(*a.desc.Platform, *b.desc.Platform)
	})
	return present[0].desc, nil
}

// isImage reports whether an index entry is an image, rather than metadata
// stored beside the images in the same index. BuildKit stores each
// platform's provenance and SBOM as an image manifest of its own, with the
// platform unknown/unknown and a vnd.docker.reference.type annotation, and
// an OCI 1.1 artifact such as a signature sets artifactType. Their layers
// are never unpacked, so a containerd that discards unpacked layers keeps
// theirs and drops the images', and pickManifest would rank them as the only
// complete platforms.
func isImage(m ocispec.Descriptor) bool {
	if m.ArtifactType != "" {
		return false
	}
	if m.Annotations["vnd.docker.reference.type"] != "" {
		return false
	}
	return m.Platform == nil || m.Platform.OS != "unknown"
}

func readJSON(ctx context.Context, cs content.Store, desc ocispec.Descriptor, v any) error {
	data, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
