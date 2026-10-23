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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/imageplatform"
	"github.com/brig-sh/hull/pkg/store"
)

// The text rmiCommand and pickRemovals use, shared with macOS. Linux has no
// store: images live in containerd, under names that containers hold, and
// --force and --platform act on the names that share an image's content.
const (
	rmiUsage      = "remove one or more images from containerd"
	rmiForceUsage = "remove an image a stopped container was created from, or one that several " +
		"names share"
	rmiPlatformUsage  = "remove only this platform's content, e.g. linux/amd64; the name stays while it has another"
	rmiNotForPlatform = "%s has no %s image"
)

const rmiDescription = "An image is named by its name in any spelling that pull accepts " +
	"(busybox, busybox:latest or docker.io/library/busybox:latest), by its digest, or by the " +
	"first characters of that digest as `hull images` prints them.\n\n" +
	"containerd keeps one image per name. A digest that several names point at is removed " +
	"only with --force, and then every one of those names goes.\n\n" +
	"--platform removes that platform's content and keeps the name while another platform " +
	"remains. Every other name that lists the platform loses it too, so that needs --force " +
	"as well.\n\n" +
	"An image a running container was created from is never removed. One a stopped " +
	"container was created from is removed only with --force; the container keeps its " +
	"own snapshot."

func removeImages(ctx context.Context, cmd *cli.Command) error {
	refs := cmd.Args().Slice()
	if len(refs) == 0 {
		return errors.New("image reference required")
	}

	platform := cmd.String("platform")
	var only platforms.Matcher
	if platform != "" {
		p, err := imageplatform.Parse(platform)
		if err != nil {
			return err
		}
		only = platforms.OnlyStrict(p)
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	return removeImagesIn(ctx, cmd.Writer, c, refs, platform, only, cmd.Bool("force"))
}

// removeImagesIn removes the images refs name from hull's containerd
// namespace. only matches the --platform given as platform, and is nil
// without one.
//
// As on macOS, every reference is attempted and the failures are reported
// together at the end. The images are listed again for each reference, so
// one that an earlier reference removed is not removed twice.
func removeImagesIn(ctx context.Context, w io.Writer, c *client.Client, refs []string,
	platform string, only platforms.Matcher, force bool) error {
	var failures []error
	for _, ref := range refs {
		catalog, err := listCatalog(ctx, c)
		if err != nil {
			return err
		}
		matches, byPrefix := store.MatchImages(catalog.records, lookupRef(ref))
		dropping := make(map[*store.ImageMetadata][]ocispec.Descriptor)
		matches, err = pickRemovals(ref, platform, matches, byPrefix,
			func(records []*store.ImageMetadata) []*store.ImageMetadata {
				var kept []*store.ImageMetadata
				for _, rec := range records {
					manifests := catalog.platformManifests(ctx, c, rec, only)
					if len(manifests) > 0 {
						dropping[rec] = manifests
						kept = append(kept, rec)
					}
				}
				return kept
			})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if only == nil {
			err = removeWholeImages(ctx, w, c, matches, force)
		} else {
			err = dropPlatform(ctx, w, c, catalog, platform, matches, dropping, force)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// lookupRef is the form of ref the image records are matched against.
//
// containerd stores an image under the full name pull gives it, so a name is
// normalized the same way first, and `hull rmi busybox` finds
// docker.io/library/busybox:latest. A digest or digest prefix is left as it
// is: normalizing "sha256:c408baae42f5" would turn it into a tag of a
// repository named sha256 on Docker Hub. A reference that does not parse is
// left as it is too, and matches nothing.
func lookupRef(ref string) string {
	algorithm, _, ok := strings.Cut(ref, ":")
	if ok && digest.Algorithm(algorithm).Available() {
		return ref
	}
	name, err := containerd.NormalizeRef(ref)
	if err != nil {
		return ref
	}
	return name
}

// imageCatalog is the images in the namespace, with the record `hull images`
// prints for each.
type imageCatalog struct {
	records []*store.ImageMetadata
	images  map[*store.ImageMetadata]images.Image
}

// listCatalog lists the images and describes each the way `hull images`
// does. An image whose content cannot be read keeps the record
// DescribeImage returns with the error, its name and a digest, so it can
// still be removed.
func listCatalog(ctx context.Context, c *client.Client) (*imageCatalog, error) {
	imgs, err := c.ImageService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	catalog := &imageCatalog{
		records: make([]*store.ImageMetadata, 0, len(imgs)),
		images:  make(map[*store.ImageMetadata]images.Image, len(imgs)),
	}
	for _, img := range imgs {
		described, err := containerd.DescribeImage(ctx, c.ContentStore(), img, platforms.Default())
		if err != nil {
			logrus.Debugf("image %s: %v", img.Name, err)
		}
		rec := imageRecord(described)
		catalog.records = append(catalog.records, rec)
		catalog.images[rec] = img
	}
	return catalog, nil
}

// platformManifests returns the manifests of rec's image that only matches.
// An image whose manifests cannot be read has none to offer.
func (cat *imageCatalog) platformManifests(ctx context.Context, c *client.Client,
	rec *store.ImageMetadata, only platforms.Matcher) []ocispec.Descriptor {
	manifests, err := containerd.ImageManifests(ctx, c.ContentStore(), cat.images[rec])
	if err != nil {
		logrus.Debugf("image %s: %v", rec.Ref, err)
		return nil
	}
	var matching []ocispec.Descriptor
	for _, m := range manifests {
		if m.Platform != nil && only.Match(*m.Platform) {
			matching = append(matching, m.Desc)
		}
	}
	return matching
}

// removeWholeImages deletes the names matches holds.
//
// A name names one image, so a reference that matched several names was a
// digest, or a digest pin, that they all point at. As `docker rmi` does for
// an image ID referenced in several repositories, that takes --force: the
// user named the content, and removing it means removing names they never
// typed.
func removeWholeImages(ctx context.Context, w io.Writer, c *client.Client,
	matches []*store.ImageMetadata, force bool) error {
	if len(matches) > 1 && !force {
		return fmt.Errorf("image %s is referenced by names %s; remove one by name, or pass "+
			"--force to remove them all", matches[0].Digest, strings.Join(recordNames(matches), ", "))
	}
	var failures []error
	for _, rec := range matches {
		err := refuseHolders(ctx, c, rec.Ref, force)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		err = containerd.DeleteImage(ctx, c, rec.Ref)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "Removed %s %s\n", rec.Digest, rec.Ref)
	}
	return errors.Join(failures...)
}

// dropPlatform removes the manifests dropping lists from every image that
// lists them, and keeps each name while it has another platform.
//
// The content store holds a manifest once, by digest, so dropping it from
// one name drops it from every name whose index lists it, such as a second
// tag of the same image. Those names need --force, the same as a digest that
// several names point at.
func dropPlatform(ctx context.Context, w io.Writer, c *client.Client, catalog *imageCatalog, platform string,
	matches []*store.ImageMetadata, dropping map[*store.ImageMetadata][]ocispec.Descriptor, force bool) error {
	drop := make(map[digest.Digest]bool)
	for _, rec := range matches {
		for _, m := range dropping[rec] {
			drop[m.Digest] = true
		}
	}

	affected := make(map[*store.ImageMetadata][]ocispec.Descriptor)
	var names []string
	for _, rec := range catalog.records {
		manifests, err := containerd.ImageManifests(ctx, c.ContentStore(), catalog.images[rec])
		if err != nil {
			continue
		}
		for _, m := range manifests {
			if drop[m.Desc.Digest] {
				affected[rec] = append(affected[rec], m.Desc)
			}
		}
		if len(affected[rec]) > 0 {
			names = append(names, rec.Ref)
		}
	}
	slices.Sort(names)
	if len(names) > 1 && !force {
		return fmt.Errorf("the %s content of %s is shared by names %s; pass --force to remove "+
			"it from them all", platform, matches[0].Ref, strings.Join(names, ", "))
	}

	for _, name := range names {
		err := refuseHolders(ctx, c, name, force)
		if err != nil {
			return err
		}
	}
	var failures []error
	for _, rec := range catalog.records {
		manifests := affected[rec]
		if len(manifests) == 0 {
			continue
		}
		_, err := containerd.DropManifests(ctx, c, catalog.images[rec], manifests)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, m := range manifests {
			_, _ = fmt.Fprintf(w, "Removed %s %s\n", m.Digest, rec.Ref)
		}
	}
	return errors.Join(failures...)
}

// refuseHolders applies the macOS rules to the containers created from
// name: a running one refuses the removal even under --force, and a stopped
// one refuses it without --force. A container keeps its own snapshot, so
// unlike a vz generic-container instance on macOS it does not lose its root
// filesystem with the image.
func refuseHolders(ctx context.Context, c *client.Client, name string, force bool) error {
	found, err := containerd.ImageHolders(ctx, c, name)
	if err != nil {
		return err
	}
	held := make([]imageHolder, 0, len(found))
	for _, h := range found {
		held = append(held, imageHolder{id: h.ID, running: h.Running})
	}
	if running := runningHolders(held); len(running) > 0 {
		return fmt.Errorf("image %s is in use by running %s %s; stop %s first",
			name, plural(len(running), "container", "containers"),
			strings.Join(running, ", "), plural(len(running), "it", "them"))
	}
	if len(held) > 0 && !force {
		ids := holderIDs(held)
		return fmt.Errorf("image %s is referenced by %s %s; remove %s, or pass --force to "+
			"remove the image anyway", name, plural(len(ids), "container", "containers"),
			strings.Join(ids, ", "), plural(len(ids), "it", "them"))
	}
	return nil
}

func recordNames(records []*store.ImageMetadata) []string {
	names := make([]string, 0, len(records))
	for _, rec := range records {
		names = append(names, rec.Ref)
	}
	slices.Sort(names)
	return names
}
