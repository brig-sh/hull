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
	"fmt"
	"sort"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Manifest is the image manifest of one platform, present in the content
// store.
type Manifest struct {
	Desc ocispec.Descriptor
	// Platform is nil when neither the index entry nor the image config
	// names one.
	Platform *ocispec.Platform
}

// ImageManifests returns the image manifests of img that are in the content
// store: each present image entry of the index the name points at, or the
// manifest itself when the name points straight at one. Entries that are not
// images, which isImage tells apart, are left out. A manifest counts as
// present when its blob is, whatever is missing under it.
func ImageManifests(ctx context.Context, cs content.Store, img images.Image) ([]Manifest, error) {
	if images.IsManifestType(img.Target.MediaType) {
		ok, err := present(ctx, cs, img.Target)
		if err != nil || !ok {
			return nil, err
		}
		return []Manifest{{Desc: img.Target, Platform: configPlatform(ctx, cs, img.Target)}}, nil
	}
	if !images.IsIndexType(img.Target.MediaType) {
		return nil, fmt.Errorf("unsupported media type %s", img.Target.MediaType)
	}
	var idx ocispec.Index
	err := readJSON(ctx, cs, img.Target, &idx)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	var out []Manifest
	for _, m := range idx.Manifests {
		if !images.IsManifestType(m.MediaType) || !isImage(m) {
			continue
		}
		ok, err := present(ctx, cs, m)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		platform := m.Platform
		if platform == nil {
			platform = configPlatform(ctx, cs, m)
		}
		out = append(out, Manifest{Desc: m, Platform: platform})
	}
	return out, nil
}

// present reports whether the blob desc names is in the content store.
func present(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (bool, error) {
	_, err := cs.Info(ctx, desc.Digest)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// configPlatform reads the platform from the config of the manifest desc
// describes, or returns nil when the config cannot be read or names no OS.
func configPlatform(ctx context.Context, cs content.Store, desc ocispec.Descriptor) *ocispec.Platform {
	var manifest ocispec.Manifest
	err := readJSON(ctx, cs, desc, &manifest)
	if err != nil {
		return nil
	}
	var config ocispec.Image
	err = readJSON(ctx, cs, manifest.Config, &config)
	if err != nil || config.OS == "" {
		return nil
	}
	return &config.Platform
}

// DeleteImage deletes the image name and waits for the garbage collector,
// so the content and snapshots nothing else refers to are gone when it
// returns.
func DeleteImage(ctx context.Context, c *client.Client, name string) error {
	err := c.ImageService().Delete(ctx, name, images.SynchronousDelete())
	if err != nil {
		return fmt.Errorf("remove image %s: %w", name, err)
	}
	return nil
}

// DropManifests removes the given platform manifests of img from the content
// store and keeps the name, as `docker rmi --platform` does. When no image
// manifest of img is left, the name goes too, and deletedName says so.
//
// The index blob keeps its containerd.io/gc.ref.content.m.<n> label for a
// dropped manifest. A label naming a digest that is not in the store keeps
// nothing alive, and ImageManifests and DescribeImage skip the missing entry.
// Deleting the manifest blob takes its own gc.ref labels with it, so the
// garbage collector then removes the config, the layers and the unpacked
// snapshot, which the config's gc.ref.snapshot label held, unless another
// image refers to them. The content is shared by digest: every name whose
// index lists a dropped manifest loses that platform too.
func DropManifests(ctx context.Context, c *client.Client, img images.Image, manifests []ocispec.Descriptor) (deletedName bool, err error) {
	cs := c.ContentStore()
	for _, m := range manifests {
		err := cs.Delete(ctx, m.Digest)
		if err != nil && !errdefs.IsNotFound(err) {
			return false, fmt.Errorf("remove manifest %s of %s: %w", m.Digest, img.Name, err)
		}
	}
	left, err := ImageManifests(ctx, cs, img)
	if err != nil {
		return false, err
	}
	if len(left) == 0 {
		return true, DeleteImage(ctx, c, img.Name)
	}
	return false, collectGarbage(ctx, c)
}

// collectGarbage runs containerd's garbage collector and waits for it to
// finish. containerd has no call for that, but deleting a lease with
// SynchronousDelete runs a collection before it returns, so this deletes a
// lease it has just created. Without it the collector runs on its own
// schedule, and the space a dropped platform held would still be in use
// when rmi returns.
func collectGarbage(ctx context.Context, c *client.Client) error {
	ls := c.LeasesService()
	l, err := ls.Create(ctx, leases.WithRandomID())
	if err != nil {
		return fmt.Errorf("collect garbage: %w", err)
	}
	err = ls.Delete(ctx, l, leases.SynchronousDelete)
	if err != nil {
		return fmt.Errorf("collect garbage: %w", err)
	}
	return nil
}

// Holder is a container whose image is the one asked about.
type Holder struct {
	ID string
	// Running is true when the container has a task that has not stopped:
	// created, running, paused or pausing.
	Running bool
}

// ImageHolders returns the containers in the client's namespace that were
// created from the image name, sorted by ID.
func ImageHolders(ctx context.Context, c *client.Client, name string) ([]Holder, error) {
	containers, err := c.Containers(ctx, fmt.Sprintf("image==%q", name))
	if err != nil {
		return nil, fmt.Errorf("list containers of %s: %w", name, err)
	}
	holders := make([]Holder, 0, len(containers))
	for _, ctr := range containers {
		running, err := taskRunning(ctx, ctr)
		if err != nil {
			return nil, err
		}
		holders = append(holders, Holder{ID: ctr.ID(), Running: running})
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].ID < holders[j].ID })
	return holders, nil
}

func taskRunning(ctx context.Context, ctr client.Container) (bool, error) {
	task, err := describeTask(ctx, ctr)
	if err != nil {
		return false, err
	}
	return task != nil && task.Status != client.Stopped, nil
}
