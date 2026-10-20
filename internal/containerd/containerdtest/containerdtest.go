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

// Package containerdtest holds helpers for tests that run against
// containerd, for use from any package.
package containerdtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DaemonAddress returns the socket of the containerd daemon a test runs
// against, which HULL_TEST_CONTAINERD_ADDRESS names. Without it the test
// skips, unless HULL_TEST_REQUIRE_CONTAINERD is set: then it fails. CI sets
// that, so a daemon that went missing fails the job rather than passing it
// with the test skipped.
func DaemonAddress(t testing.TB) string {
	t.Helper()
	addr := os.Getenv("HULL_TEST_CONTAINERD_ADDRESS")
	if addr == "" {
		if os.Getenv("HULL_TEST_REQUIRE_CONTAINERD") != "" {
			t.Fatal("HULL_TEST_REQUIRE_CONTAINERD is set, but HULL_TEST_CONTAINERD_ADDRESS names no daemon")
		}
		t.Skip("HULL_TEST_CONTAINERD_ADDRESS not set")
	}
	return addr
}

// WriteBlob stores v as JSON in cs and returns its descriptor.
func WriteBlob(ctx context.Context, t testing.TB, cs content.Ingester, mediaType string, v any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	err = content.WriteBlob(ctx, cs, desc.Digest.String(), bytes.NewReader(data), desc)
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// WriteImage stores a config and a manifest for platform p, with two layers
// of 1000 and 24 bytes, and returns the manifest's descriptor as an index
// would list it. The layers are stored only when withLayers is set; without
// them the store holds what a pull for another platform leaves of this one.
func WriteImage(ctx context.Context, t testing.TB, cs content.Ingester, p ocispec.Platform, labels map[string]string, withLayers bool) ocispec.Descriptor {
	t.Helper()
	config := WriteBlob(ctx, t, cs, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: p,
		Config:   ocispec.ImageConfig{Labels: labels},
	})
	var layers []ocispec.Descriptor
	for i, size := range []int{1000, 24} {
		// Distinct per platform and per layer, so platforms share no blob.
		data := make([]byte, size)
		copy(data, fmt.Sprintf("%d %s", i, platforms.Format(p)))
		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(size)}
		if withLayers {
			err := content.WriteBlob(ctx, cs, desc.Digest.String(), bytes.NewReader(data), desc)
			if err != nil {
				t.Fatal(err)
			}
		}
		layers = append(layers, desc)
	}
	desc := WriteBlob(ctx, t, cs, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    layers,
	})
	desc.Platform = &p
	return desc
}

// Lease returns ctx under a containerd lease that ends with the test, and
// content written with that context is kept until then. Without a lease the
// daemon may collect it at once: an image record references only its
// target, and the blobs under an index or manifest are kept by labels that
// a pull sets and WriteBlob does not.
func Lease(ctx context.Context, t testing.TB, c *client.Client) context.Context {
	t.Helper()
	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = done(ctx)
	})
	return ctx
}

// CreateImage records name as an image that points at target, replacing a
// record an earlier run left behind, and deletes it when the test ends.
func CreateImage(ctx context.Context, t testing.TB, c *client.Client, name string, target ocispec.Descriptor) images.Image {
	t.Helper()
	is := c.ImageService()
	err := is.Delete(ctx, name)
	if err != nil && !errdefs.IsNotFound(err) {
		t.Fatal(err)
	}
	img, err := is.Create(ctx, images.Image{Name: name, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = is.Delete(ctx, name, images.SynchronousDelete())
	})
	return img
}
