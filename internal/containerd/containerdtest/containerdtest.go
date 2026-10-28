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
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
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
//
// The lease ends with a synchronous delete. Otherwise what it held goes
// only at the daemon's next collection, and a namespace that still holds
// it cannot be deleted when the test ends.
func Lease(ctx context.Context, t testing.TB, c *client.Client) context.Context {
	t.Helper()
	ls := c.LeasesService()
	l, err := ls.Create(ctx, leases.WithRandomID(), leases.WithExpiration(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ls.Delete(ctx, l, leases.SynchronousDelete)
	})
	return leases.WithLease(ctx, l.ID)
}

// NamespaceName returns a namespace for one run of a test, made of prefix
// and the time, so no other test, and no earlier run of this one, shares
// what is in it.
func NamespaceName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// DeleteNamespace deletes the namespace of c when the test ends, with the
// images left in it. Register it right after the cleanup that closes c, so
// it runs after the test's other cleanups have removed its containers and
// leases, and while c is still open.
func DeleteNamespace(t testing.TB, c *client.Client) {
	t.Helper()
	ns := c.DefaultNamespace()
	t.Cleanup(func() {
		ctx := context.Background()
		imgs, _ := c.ImageService().List(ctx)
		for _, img := range imgs {
			_ = c.ImageService().Delete(ctx, img.Name, images.SynchronousDelete())
		}
		_ = c.NamespaceService().Delete(ctx, ns)
	})
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

// TaskImage is the image StartTask runs: containerd's copy of busybox on
// ghcr.io. Docker Hub's anonymous rate limits would make CI flaky.
const TaskImage = "ghcr.io/containerd/busybox:1.36"

// StartTask pulls TaskImage for the host's platform, creates the container
// id from it with args as its process, and starts the container's task with
// no I/O attached. The task, the container and its snapshot are deleted
// when the test ends, unless the test removed them first.
//
// The task runs under runc, which the daemon's runc shim finds on its PATH,
// and the daemon must be able to unpack layers: rootful, or rootless inside
// rootlesskit.
func StartTask(ctx context.Context, t testing.TB, c *client.Client, id string, args ...string) (client.Container, client.Task) {
	t.Helper()
	img, err := c.Pull(ctx, TaskImage, client.WithPullUnpack)
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := c.NewContainer(ctx, id,
		client.WithNewSnapshot(id+"-snapshot", img),
		client.WithNewSpec(
			// Not oci.WithImageConfig: it reads the image's /etc/group from
			// the snapshot in this process, and a test that is not root
			// cannot open the snapshot of a daemon that is, as in CI.
			// busybox needs only a PATH to find its commands.
			oci.WithDefaultPathEnv,
			oci.WithProcessArgs(args...),
			// No cgroup path, so runc creates no cgroup. A rootless daemon
			// without a delegated cgroup could not, and these tasks need
			// no limits.
			oci.WithCgroup(""),
		))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		task, err := ctr.Task(ctx, nil)
		if err == nil {
			_, _ = task.Delete(ctx, client.WithProcessKill)
		}
		_ = ctr.Delete(ctx, client.WithSnapshotCleanup)
	})
	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatal(err)
	}
	err = task.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ctr, task
}
