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
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func newContentStore(t *testing.T) content.Store {
	t.Helper()
	cs, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

var (
	linuxAMD64 = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	linuxARM64 = ocispec.Platform{OS: "linux", Architecture: "arm64"}
)

// A single-platform image: the name points straight at the manifest, so
// there is no index digest.
func TestDescribeImageOfAManifest(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	manifest := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, map[string]string{"org.example": "yes"}, true)
	manifest.Platform = nil
	updated := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	rec, err := DescribeImage(ctx, cs, images.Image{
		Name:      "docker.io/library/alpine:latest",
		Target:    manifest,
		UpdatedAt: updated,
	}, platforms.Only(linuxAMD64))
	if err != nil {
		t.Fatal(err)
	}

	if rec.Name != "docker.io/library/alpine:latest" {
		t.Errorf("Name = %q", rec.Name)
	}
	if rec.Digest != manifest.Digest.String() || rec.IndexDigest != "" {
		t.Errorf("Digest, IndexDigest = %q, %q; want %q and none", rec.Digest, rec.IndexDigest, manifest.Digest)
	}
	if rec.Size != 1024 {
		t.Errorf("Size = %d, want the layer sum 1024", rec.Size)
	}
	if rec.Platform != "linux/amd64" {
		t.Errorf("Platform = %q", rec.Platform)
	}
	if rec.Labels["org.example"] != "yes" {
		t.Errorf("Labels = %v", rec.Labels)
	}
	if !rec.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v, want %v", rec.UpdatedAt, updated)
	}
}

// A multi-arch image: Digest is the platform manifest pref ranks highest, and
// the index the name points at goes in IndexDigest.
func TestDescribeImagePicksThePreferredPlatform(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})

	for _, tc := range []struct {
		pref ocispec.Platform
		want ocispec.Descriptor
	}{
		{linuxAMD64, amd},
		{linuxARM64, arm},
	} {
		rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(tc.pref))
		if err != nil {
			t.Fatal(err)
		}
		if rec.Digest != tc.want.Digest.String() {
			t.Errorf("pref %s: Digest = %q, want %q", platforms.Format(tc.pref), rec.Digest, tc.want.Digest)
		}
		if rec.IndexDigest != index.Digest.String() {
			t.Errorf("pref %s: IndexDigest = %q, want %q", platforms.Format(tc.pref), rec.IndexDigest, index.Digest)
		}
		if rec.Platform != platforms.Format(tc.pref) {
			t.Errorf("pref %s: Platform = %q", platforms.Format(tc.pref), rec.Platform)
		}
	}
}

// An image pulled for another platform holds only that platform's manifest.
// It is described as that platform, rather than as a broken image.
func TestDescribeImageFallsBackToThePlatformPresent(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, true)
	missing := linuxAMD64
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("absent"), Size: 10, Platform: &missing},
			arm,
		},
	})

	rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Digest != arm.Digest.String() || rec.Platform != "linux/arm64" {
		t.Errorf("Digest, Platform = %q, %q; want %q, linux/arm64", rec.Digest, rec.Platform, arm.Digest)
	}
}

// An image whose manifest is gone is an error, but the Image returned with it
// still carries the name and the digest the name points at, so a caller can
// still show it.
func TestDescribeImageOfADamagedImage(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	target := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("gone"), Size: 10}

	rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: target}, platforms.Only(linuxAMD64))
	if err == nil {
		t.Fatal("DescribeImage of an image with no manifest succeeded")
	}
	if rec.Name != "x" || rec.Digest != target.Digest.String() || rec.Size != 0 {
		t.Errorf("image = %+v; want the name and target digest, no size", rec)
	}
}

// What ctr leaves behind after `ctr images pull --platform linux/arm64` on an
// amd64 host: the manifest and config of every platform, but the layers of
// arm64 only. The arm64 image that was pulled must be described, not the
// amd64 one the host prefers but has no layers for.
func TestDescribeImageSkipsAPlatformWithoutLayers(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, false)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})

	rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Digest != arm.Digest.String() || rec.Platform != "linux/arm64" {
		t.Errorf("Digest, Platform = %q, %q; want %q, linux/arm64", rec.Digest, rec.Platform, arm.Digest)
	}
}

// A containerd that discards layers once it has unpacked them leaves no
// platform complete. The platform pref ranks highest is still described.
func TestDescribeImageWithNoCompletePlatform(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, false)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{arm, amd},
	})

	rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Digest != amd.Digest.String() || rec.Platform != "linux/amd64" {
		t.Errorf("Digest, Platform = %q, %q; want %q, linux/amd64", rec.Digest, rec.Platform, amd.Digest)
	}
}

// An index can hold entries that are not images: BuildKit's attestation
// manifests and OCI artifacts such as signatures. Their layers are never
// unpacked, so on a containerd that discards unpacked layers they can be the
// only complete entries. They must still never be described as the image.
func TestDescribeImageSkipsEntriesThatAreNotImages(t *testing.T) {
	unknown := ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	for _, tc := range []struct {
		name  string
		entry func(ocispec.Descriptor) ocispec.Descriptor
	}{
		{"attestation annotation", func(d ocispec.Descriptor) ocispec.Descriptor {
			d.Platform = &linuxAMD64
			d.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
			return d
		}},
		{"unknown platform", func(d ocispec.Descriptor) ocispec.Descriptor {
			return d
		}},
		{"artifact type", func(d ocispec.Descriptor) ocispec.Descriptor {
			d.Platform = &linuxAMD64
			d.ArtifactType = "application/vnd.example.signature"
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cs := newContentStore(t)
			amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, false)
			arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
			other := tc.entry(containerdtest.WriteImage(ctx, t, cs, unknown, nil, true))
			index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
				MediaType: ocispec.MediaTypeImageIndex,
				Manifests: []ocispec.Descriptor{other, arm, amd},
			})

			rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
			if err != nil {
				t.Fatal(err)
			}
			if rec.Digest != amd.Digest.String() || rec.Platform != "linux/amd64" {
				t.Errorf("Digest, Platform = %q, %q; want %q, linux/amd64", rec.Digest, rec.Platform, amd.Digest)
			}
		})
	}
}

// The other tests read from an in-process content store. hull reads through
// the client's, which goes over gRPC to the daemon and maps its errors back,
// and it finds images through the daemon's image service. Images written and
// recorded there must describe the same way. The second image has lost the
// layers of every platform, so it is described only if a missing blob comes
// back from the daemon as not found rather than as some other error.
func TestDescribeImageThroughDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), addr, "hull-test-describe")
	if err != nil {
		t.Fatal(err)
	}
	// Closed by a cleanup, not a defer: the cleanups that delete the test's
	// image and lease run after the test returns and still need the client.
	t.Cleanup(func() { _ = c.Close() })
	ctx := containerdtest.Lease(context.Background(), t, c)
	cs := c.ContentStore()

	riscv := ocispec.Platform{OS: "linux", Architecture: "riscv64"}
	s390x := ocispec.Platform{OS: "linux", Architecture: "s390x"}
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, map[string]string{"org.example": "yes"}, true)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
	riscvDesc := containerdtest.WriteImage(ctx, t, cs, riscv, nil, false)
	s390xDesc := containerdtest.WriteImage(ctx, t, cs, s390x, nil, false)
	for _, tc := range []struct {
		name      string
		manifests []ocispec.Descriptor
		pref      ocispec.Platform
		want      ocispec.Descriptor
	}{
		{"example.com/hull/complete:1", []ocispec.Descriptor{arm, amd}, linuxAMD64, amd},
		{"example.com/hull/discarded:1", []ocispec.Descriptor{s390xDesc, riscvDesc}, riscv, riscvDesc},
	} {
		index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: tc.manifests,
		})
		containerdtest.CreateImage(ctx, t, c, tc.name, index)

		img, err := c.ImageService().Get(ctx, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := DescribeImage(ctx, cs, img, platforms.Only(tc.pref))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rec.Digest != tc.want.Digest.String() || rec.IndexDigest != index.Digest.String() {
			t.Errorf("%s: Digest, IndexDigest = %q, %q; want %q, %q", tc.name, rec.Digest, rec.IndexDigest, tc.want.Digest, index.Digest)
		}
		if rec.Platform != platforms.Format(tc.pref) || rec.Size != 1024 {
			t.Errorf("%s: image = %+v; want %s, 1024 bytes", tc.name, rec, platforms.Format(tc.pref))
		}
	}

	imgs, err := c.ImageService().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Errorf("image service lists %d images, want the 2 written", len(imgs))
	}
}

// failingStore fails reads the way a daemon's content store does when its
// RPCs fail: reads of the blobs in fail return err, and every read fails once
// the context is done. reads counts the attempts.
type failingStore struct {
	content.Store
	fail  map[digest.Digest]bool
	err   error
	reads int
}

func (s *failingStore) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	s.reads++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.fail[desc.Digest] {
		return nil, s.err
	}
	return s.Store.ReaderAt(ctx, desc)
}

// An entry whose manifest is not even JSON cannot be checked. It is skipped
// like a missing one, and the rest of the index is described as usual.
func TestDescribeImageSkipsAMalformedEntry(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	junk := []byte("not a manifest")
	bad := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(junk), Size: int64(len(junk)), Platform: &linuxARM64}
	err := content.WriteBlob(ctx, cs, bad.Digest.String(), bytes.NewReader(junk), bad)
	if err != nil {
		t.Fatal(err)
	}
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{bad, amd},
	})

	rec, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Digest != amd.Digest.String() {
		t.Errorf("Digest = %q, want the amd64 manifest %q", rec.Digest, amd.Digest)
	}
}

// When the store fails every check, as a daemon that has stopped answering
// does, the error it returned is what comes back, not a claim that the
// content is missing.
func TestDescribeImageReportsAFailingStore(t *testing.T) {
	ctx := context.Background()
	local := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, local, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, local, linuxARM64, nil, true)
	index := containerdtest.WriteBlob(ctx, t, local, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})
	cs := &failingStore{
		Store: local,
		fail:  map[digest.Digest]bool{amd.Digest: true, arm.Digest: true},
		err:   errdefs.ErrUnavailable,
	}

	_, err := DescribeImage(ctx, cs, images.Image{Name: "x", Target: index}, platforms.Only(linuxAMD64))
	if !errors.Is(err, errdefs.ErrUnavailable) {
		t.Fatalf("error = %v, want the store's %v", err, errdefs.ErrUnavailable)
	}
}

// A context that is done fails every later check too, so the search ends at
// the first one instead of trying each entry in turn.
func TestPickManifestStopsWhenTheContextIsDone(t *testing.T) {
	ctx := context.Background()
	local := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, local, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, local, linuxARM64, nil, true)
	cs := &failingStore{Store: local}

	done, cancel := context.WithCancel(ctx)
	cancel()
	_, err := pickManifest(done, cs, []ocispec.Descriptor{amd, arm}, platforms.Only(linuxAMD64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if cs.reads != 1 {
		t.Errorf("store was read %d times, want 1", cs.reads)
	}
}
