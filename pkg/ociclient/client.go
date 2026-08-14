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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/brig-sh/hull/pkg/store"
)

// Client provides OCI image operations
type Client struct {
	store *store.Store
	// Quiet suppresses pull progress output on stderr.
	Quiet bool
}

// PullResult contains the result of a successful image pull
type PullResult struct {
	Digest    string
	Size      int64
	Labels    map[string]string
	Platforms []string
}

// New creates a new OCI client
func New(s *store.Store) *Client {
	return &Client{store: s}
}

// registryTransport pins registry traffic to HTTP/1.1.
//
// Large layers pulled from ghcr over HTTP/2 fail often enough to be a real
// problem, always the same way:
//
//	stream error; stream ID N; PROTOCOL_ERROR; received from peer
//
// It killed a CI boot test twice and a developer's pull of the desktop image,
// each time part-way through a multi-hundred-megabyte layer, and the pull has
// no retry so the whole image is lost. Go's HTTP/2 stack surfaces the peer's
// stream reset as a fatal read error; HTTP/1.1 carries the same bytes without
// multiplexing and does not hit it. The cost is losing request multiplexing,
// which a layer download does not benefit from anyway.
func registryTransport() crane.Option {
	t := remote.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	return crane.WithTransport(t)
}

// DefaultPlatform is the pull platform when the caller does not ask for
// one: native arm64, the right answer for everything except the Rosetta
// path (an amd64 rootfs translated under an arm64 kernel).
const DefaultPlatform = "linux/arm64"

// parsePlatform turns an "os/arch" or "os/arch/variant" string into a
// registry platform selector.
func parsePlatform(s string) (*v1.Platform, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return &v1.Platform{OS: parts[0], Architecture: parts[1]}, nil
	case 3:
		return &v1.Platform{OS: parts[0], Architecture: parts[1], Variant: parts[2]}, nil
	}
	return nil, fmt.Errorf("invalid platform %q, expected os/arch (e.g. linux/amd64)", s)
}

// Pull fetches an OCI image from a registry and stores it locally,
// selecting the default (native arm64) platform.
func (c *Client) Pull(ctx context.Context, ref string) (*PullResult, error) {
	return c.PullPlatform(ctx, ref, DefaultPlatform)
}

// PullPlatform fetches an OCI image for an explicit platform. The store is
// digest-keyed, so different platform variants of the same tag coexist.
func (c *Client) PullPlatform(ctx context.Context, ref, platformStr string) (*PullResult, error) {
	p, err := parsePlatform(platformStr)
	if err != nil {
		return nil, err
	}
	platform := crane.WithPlatform(p)
	img, err := crane.Pull(ref, platform, registryTransport(),
		crane.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil && strings.Contains(err.Error(), "error getting credentials") {
		// Docker's credential helper needs an unlocked keychain, which
		// headless sessions (CI runners, ssh) don't have. Public images
		// must not depend on it — retry anonymously.
		log.Debugf("credential store unavailable, retrying pull anonymously: %v", err)
		img, err = crane.Pull(ref, platform, registryTransport(), crane.WithAuth(authn.Anonymous))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to pull image %s: %w", ref, err)
	}

	// Get image digest
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("failed to get image digest: %w", err)
	}

	// Get image config for labels
	config, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("failed to get image config: %w", err)
	}

	// Calculate total size
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("failed to get image layers: %w", err)
	}

	var totalSize int64
	for _, layer := range layers {
		size, err := layer.Size()
		if err == nil {
			totalSize += size
		}
	}

	// Extract labels and convert to map
	labels := make(map[string]string)
	if config != nil {
		labels = config.Config.Labels
	}

	// Store image metadata
	digestStr := digest.String()
	metadata := &store.ImageMetadata{
		Ref:      ref,
		Digest:   digestStr,
		PulledAt: time.Now(),
		Labels:   labels,
		Size:     totalSize,
		Platform: platformStr,
	}

	// Already have exactly this digest, complete on disk? Then the reference
	// resolved to what we are already holding and there is nothing to fetch.
	// This is what makes --pull=always affordable: it costs a manifest lookup
	// rather than a full re-download and re-unpack of an unchanged image.
	// The metadata is rewritten so PulledAt and Ref track this resolution.
	if c.store.ImageComplete(digestStr) {
		log.Debugf("image %s already present, skipping unpack", digestStr)
		if _, err := c.store.SaveImage(digestStr, metadata); err != nil {
			return nil, fmt.Errorf("failed to refresh image metadata: %w", err)
		}
		return &PullResult{
			Digest:    digestStr,
			Size:      totalSize,
			Labels:    labels,
			Platforms: []string{fmt.Sprintf("%s/%s", config.OS, config.Architecture)},
		}, nil
	}

	// Build the image atomically: unpack into a temp sibling and rename it
	// into place only when complete. Anything interrupted mid-pull must not
	// leave a half-populated directory that later runs mistake for a cached
	// image (metadata used to be written before the layers were unpacked,
	// so an interrupted pull poisoned the store for every subsequent run).
	imagesRoot := filepath.Join(c.store.RootDir(), "images")
	if err := os.MkdirAll(imagesRoot, 0700); err != nil {
		return nil, fmt.Errorf("failed to create images dir: %w", err)
	}
	for _, pattern := range []string{"*.tmp-*", "*.old-*"} {
		if stale, err := filepath.Glob(filepath.Join(imagesRoot, pattern)); err == nil {
			for _, d := range stale {
				_ = os.RemoveAll(d)
			}
		}
	}
	tmpDir := filepath.Join(imagesRoot, fmt.Sprintf("%s.tmp-%d", digestStr, os.Getpid()))
	defer func() { _ = os.RemoveAll(tmpDir) }()

	prog := newProgress(c.Quiet, len(layers), totalSize)
	if err := UnpackLayers(layers, filepath.Join(tmpDir, "rootfs"), prog); err != nil {
		return nil, fmt.Errorf("failed to unpack layers: %w", err)
	}
	prog.done()
	if err := storeImageConfig(tmpDir, config); err != nil {
		return nil, fmt.Errorf("failed to store image config: %w", err)
	}
	// Metadata goes into the staging dir so the rename below publishes it
	// together with the rootfs. Writing it afterwards left a window where
	// image.json existed without a rootfs, which is the state that poisons
	// the store permanently.
	if err := store.WriteImageMetadata(tmpDir, metadata); err != nil {
		return nil, fmt.Errorf("failed to write image metadata: %w", err)
	}

	// Commit by swapping directories, never by deleting in place. RemoveAll
	// on a populated rootfs takes seconds and deletes in readdir order, so an
	// interrupt during it could strip the rootfs while leaving image.json --
	// exactly the half-state that made every later run fail. Renaming the old
	// directory aside is atomic; the slow delete then happens once the new
	// image is already published, where an interrupt is harmless (the leftover
	// is swept by the *.old-* glob above).
	finalDir := filepath.Join(imagesRoot, digestStr)
	oldDir := filepath.Join(imagesRoot, fmt.Sprintf("%s.old-%d", digestStr, os.Getpid()))
	swapped := false
	if _, err := os.Stat(finalDir); err == nil {
		if err := os.Rename(finalDir, oldDir); err != nil {
			return nil, fmt.Errorf("failed to displace the previous image: %w", err)
		}
		swapped = true
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		if swapped {
			_ = os.Rename(oldDir, finalDir) // put the usable image back
		}
		return nil, fmt.Errorf("failed to commit image: %w", err)
	}
	if swapped {
		_ = os.RemoveAll(oldDir)
	}

	// Get available platforms
	platforms := []string{fmt.Sprintf("%s/%s", config.OS, config.Architecture)}

	return &PullResult{
		Digest:    digestStr,
		Size:      totalSize,
		Labels:    labels,
		Platforms: platforms,
	}, nil
}

// ImageExists reports whether an image is cached AND usable. Metadata on its
// own does not count: see store.ImageComplete.
func (c *Client) ImageExists(digest string) bool {
	return c.store.ImageComplete(digest)
}

// storeImageConfig saves the OCI image config for reference
func storeImageConfig(imageDir string, config *v1.ConfigFile) error {
	if config == nil {
		return nil
	}

	configPath := fmt.Sprintf("%s/oci-config.json", imageDir)
	configData, err := json.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, configData, 0600)
}
