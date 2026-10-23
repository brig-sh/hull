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
	"testing"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// An index entry may leave out the platform, and a name may point straight
// at a manifest. ImageManifests reads the platform from the config then, so
// --platform can still find it. A manifest whose blob is gone is left out.
func TestImageManifests(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
	unnamed := amd
	unnamed.Platform = nil
	missing := containerdtest.WriteImage(ctx, t, newContentStore(t), ocispec.Platform{OS: "linux", Architecture: "s390x"}, nil, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{unnamed, arm, missing},
	})

	got, err := ImageManifests(ctx, cs, images.Image{Name: "example.com/hull/index:1", Target: index})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ImageManifests = %+v, want the amd64 and arm64 entries", got)
	}
	if got[0].Desc.Digest != amd.Digest || got[0].Platform == nil || platforms.Format(*got[0].Platform) != "linux/amd64" {
		t.Errorf("entry without a platform = %+v, want %s as linux/amd64 from its config", got[0], amd.Digest)
	}
	if got[1].Desc.Digest != arm.Digest || got[1].Platform == nil || platforms.Format(*got[1].Platform) != "linux/arm64" {
		t.Errorf("arm64 entry = %+v, want %s as linux/arm64", got[1], arm.Digest)
	}

	got, err = ImageManifests(ctx, cs, images.Image{Name: "example.com/hull/manifest:1", Target: unnamed})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Platform == nil || platforms.Format(*got[0].Platform) != "linux/amd64" {
		t.Errorf("a name pointing at a manifest gave %+v, want that manifest as linux/amd64", got)
	}

	// DropManifests reads an empty answer as the name having no platform
	// left, so a name whose manifest blob is gone must give one.
	got, err = ImageManifests(ctx, cs, images.Image{Name: "example.com/hull/gone:1", Target: missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a name pointing at a missing manifest gave %+v, want none", got)
	}
}

// Dropping one platform of two keeps the name and the other platform.
// Dropping the last one takes the name too.
func TestDropManifestsThroughDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), addr, "hull-test-drop")
	if err != nil {
		t.Fatal(err)
	}
	// Closed by a cleanup, not a defer: the cleanups that delete the test's
	// image and lease run after the test returns and still need the client.
	t.Cleanup(func() { _ = c.Close() })
	ctx := containerdtest.Lease(context.Background(), t, c)
	cs := c.ContentStore()

	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})
	const name = "example.com/hull/drop:1"
	img := containerdtest.CreateImage(ctx, t, c, name, index)

	deleted, err := DropManifests(ctx, c, img, []ocispec.Descriptor{amd})
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("dropping one platform of two deleted the name")
	}
	_, err = cs.Info(ctx, amd.Digest)
	if !errdefs.IsNotFound(err) {
		t.Errorf("the dropped manifest is still in the content store: %v", err)
	}
	left, err := ImageManifests(ctx, cs, img)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Desc.Digest != arm.Digest {
		t.Errorf("manifests left = %+v, want only arm64's %s", left, arm.Digest)
	}

	deleted, err = DropManifests(ctx, c, img, []ocispec.Descriptor{arm})
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("dropping the last platform kept the name")
	}
	_, err = c.ImageService().Get(ctx, name)
	if !errdefs.IsNotFound(err) {
		t.Errorf("the name survived its last platform: %v", err)
	}
}

// A container created from an image holds it. One with no task is not
// running.
func TestImageHoldersThroughDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	c, err := Connect(context.Background(), addr, "hull-test-holders")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	const name = "example.com/hull/held:1"
	// A run that failed before its cleanup leaves the container behind.
	stale, err := c.LoadContainer(ctx, "holder")
	if err == nil {
		_ = stale.Delete(ctx)
	}
	ctr, err := c.NewContainer(ctx, "holder", client.WithImageName(name), client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })

	holders, err := ImageHolders(ctx, c, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].ID != "holder" || holders[0].Running {
		t.Errorf("ImageHolders(%s) = %+v, want holder, not running", name, holders)
	}

	holders, err = ImageHolders(ctx, c, "example.com/hull/other:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Errorf("another image has holders %+v", holders)
	}
}
