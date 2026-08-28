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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

const (
	cacheTestRef    = "ghcr.io/nofireai/urunc-ubuntu:aarch64"
	cacheTestDigest = "sha256:c408baae42f5c74c0661fbc20a289fd23d4322988e52c88cd54108e5c4c74893"
)

// seedImage writes the default test image into the store. withRootfs=false
// reproduces what an interrupted pull leaves behind: metadata present, rootfs
// gone.
func seedImage(t *testing.T, s *store.Store, withRootfs bool) {
	t.Helper()
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:    cacheTestRef,
		Digest: cacheTestDigest,
	}, withRootfs)
}

func newCacheTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

func TestCachedDigestHit(t *testing.T) {
	for _, lookup := range []struct{ name, ref string }{
		{"by ref", cacheTestRef},
		{"by digest", cacheTestDigest},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			s := newCacheTestStore(t)
			seedImage(t, s, true)

			digest, ok := cachedDigest(s, lookup.ref, ociclient.DefaultPlatform)
			if !ok {
				t.Fatal("a complete image should be a cache hit")
			}
			if digest != cacheTestDigest {
				t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
			}
		})
	}
}

// The regression. Interrupting a pull could leave image.json with no rootfs.
// resolveImageDigest used to return that digest anyway, so every later run
// died with "failed to copy rootfs: no such file or directory" and the only
// escape was rm -rf ~/.hull. A miss here means the caller re-pulls and
// the store heals itself.
func TestCachedDigestMissesImageWithoutRootfs(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, false)

	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); ok {
		t.Error("an image with no rootfs must not be treated as cached")
	}
}

// Losing the rootfs after a good pull must flip the answer, not stay cached.
func TestCachedDigestHealsAfterRootfsLoss(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); !ok {
		t.Fatal("precondition: freshly pulled image should hit")
	}

	rootfs := filepath.Join(s.RootDir(), "images", cacheTestDigest, "rootfs")
	if err := os.RemoveAll(rootfs); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); ok {
		t.Error("after losing its rootfs the image must stop being a cache hit")
	}
}

func TestCachedDigestUnknownRef(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	if _, ok := cachedDigest(s, "ghcr.io/nofireai/does-not-exist:aarch64", ociclient.DefaultPlatform); ok {
		t.Error("an unknown ref must not be a cache hit")
	}
}

// Staging leftovers from an interrupted pull must never be mistaken for a
// usable image.
func TestCachedDigestIgnoresStagingLeftovers(t *testing.T) {
	s := newCacheTestStore(t)
	for _, suffix := range []string{".tmp-4242", ".old-4242"} {
		dir := filepath.Join(s.RootDir(), "images", cacheTestDigest+suffix)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := store.WriteImageMetadata(dir, &store.ImageMetadata{
			Ref:    cacheTestRef,
			Digest: cacheTestDigest,
		}); err != nil {
			t.Fatalf("WriteImageMetadata: %v", err)
		}
	}

	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); ok {
		t.Error("staging leftovers must not satisfy a cache lookup")
	}
}

func TestValidPullPolicy(t *testing.T) {
	for _, p := range []string{pullMissing, pullAlways, pullNever} {
		if !validPullPolicy(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "yes", "IfNotPresent", "Always"} {
		if validPullPolicy(p) {
			t.Errorf("%q should be rejected", p)
		}
	}
}

// --pull=never must answer from the cache and never reach the network. The nil
// client is the assertion: touching it would panic.
func TestResolveImageDigestNeverUsesCache(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	digest, err := resolveImageDigest(context.Background(), nil, s, cacheTestRef, pullNever, ociclient.DefaultPlatform)
	if err != nil {
		t.Fatalf("resolveImageDigest: %v", err)
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
	}
}

func TestResolveImageDigestNeverFailsWhenAbsent(t *testing.T) {
	s := newCacheTestStore(t)

	_, err := resolveImageDigest(context.Background(), nil, s, cacheTestRef, pullNever, ociclient.DefaultPlatform)
	if err == nil {
		t.Fatal("expected an error when the image is not cached")
	}
	if !strings.Contains(err.Error(), "not cached") {
		t.Errorf("error should explain the cache miss, got: %v", err)
	}
}

// An image whose rootfs was lost is not a usable cache entry, so --pull=never
// must fail loudly rather than hand back a digest that cannot be booted.
func TestResolveImageDigestNeverRejectsIncomplete(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, false)

	if _, err := resolveImageDigest(context.Background(), nil, s, cacheTestRef, pullNever, ociclient.DefaultPlatform); err == nil {
		t.Error("expected an error for a cached image with no rootfs")
	}
}

func TestResolveImageDigestMissingUsesCache(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	digest, err := resolveImageDigest(context.Background(), nil, s, cacheTestRef, pullMissing, ociclient.DefaultPlatform)
	if err != nil {
		t.Fatalf("resolveImageDigest: %v", err)
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
	}
}

// Pulling a republished tag leaves two entries sharing the same Ref. Directory
// order decides nothing useful there, so the newest pull must win -- otherwise
// an explicit re-pull can still hand back the image it replaced.
func TestCachedDigestPrefersNewestPull(t *testing.T) {
	s := newCacheTestStore(t)
	const olderDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	now := time.Now()

	for _, img := range []struct {
		digest string
		at     time.Time
	}{
		{olderDigest, now.Add(-2 * time.Hour)},
		{cacheTestDigest, now},
	} {
		dir, err := s.SaveImage(img.digest, &store.ImageMetadata{
			Ref:      cacheTestRef,
			Digest:   img.digest,
			PulledAt: img.at,
		})
		if err != nil {
			t.Fatalf("SaveImage: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := store.WriteUnpackSchema(dir); err != nil {
			t.Fatalf("WriteUnpackSchema: %v", err)
		}
	}

	digest, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want the newest pull %q", digest, cacheTestDigest)
	}
}

// The newest entry being unusable must not hide an older, complete one.
func TestCachedDigestSkipsIncompleteNewerEntry(t *testing.T) {
	s := newCacheTestStore(t)
	const goodDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	now := time.Now()

	dir, err := s.SaveImage(goodDigest, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: goodDigest, PulledAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := store.WriteUnpackSchema(dir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
	// Newer, but its rootfs never landed.
	if _, err := s.SaveImage(cacheTestDigest, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, PulledAt: now,
	}); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}

	digest, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("expected the older complete image to be usable")
	}
	if digest != goodDigest {
		t.Errorf("digest = %q, want %q", digest, goodDigest)
	}
}

// The platform-aware lookup from the rosetta review: an arm64 pull must not
// satisfy a --platform linux/amd64 run of the same tag, and records from
// before the Platform field existed keep matching the default platform they
// were factually pulled for.
func TestCachedDigestHonorsPlatform(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true) // legacy-shaped record: no Platform field

	if _, ok := cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform); !ok {
		t.Fatal("a legacy record must keep matching the default platform")
	}
	if _, ok := cachedDigest(s, cacheTestRef, "linux/amd64"); ok {
		t.Fatal("a legacy (default-platform) record must not satisfy an amd64 lookup")
	}

	amdDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir, err := s.SaveImage(amdDigest, &store.ImageMetadata{
		Ref:      cacheTestRef,
		Digest:   amdDigest,
		PulledAt: time.Now(),
		Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
		t.Fatalf("MkdirAll rootfs: %v", err)
	}
	if err := store.WriteUnpackSchema(dir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}

	digest, ok := cachedDigest(s, cacheTestRef, "linux/amd64")
	if !ok || digest != amdDigest {
		t.Fatalf("amd64 lookup = %q,%v; want the amd64 digest", digest, ok)
	}
	digest, ok = cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform)
	if !ok || digest == amdDigest {
		t.Fatalf("default lookup = %q,%v; must still resolve the arm64 record", digest, ok)
	}
}

const (
	cacheTestRepo        = "ghcr.io/nofireai/urunc-ubuntu"
	cacheTestIndexDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// seedMetadata writes one image record into the store, with the rootfs and the
// layout stamp a good pull leaves behind unless withRootfs says otherwise.
func seedMetadata(t *testing.T, s *store.Store, metadata *store.ImageMetadata, withRootfs bool) {
	t.Helper()
	if metadata.PulledAt.IsZero() {
		metadata.PulledAt = time.Now()
	}
	dir, err := s.SaveImage(metadata.Digest, metadata)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if !withRootfs {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
		t.Fatalf("MkdirAll rootfs: %v", err)
	}
	if err := store.WriteUnpackSchema(dir); err != nil {
		t.Fatalf("WriteUnpackSchema: %v", err)
	}
}

// The bug this file grew for: `hull run repo@sha256:<manifest digest>` never
// matched anything. Ref holds the tag the image was pulled under and Digest
// holds the bare digest, so a reference carrying its repository matched
// neither, and the image was re-pulled (or refused under --pull=never) with
// its rootfs already on disk.
func TestCachedDigestHitByManifestDigestRef(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:    cacheTestRef,
		Digest: cacheTestDigest,
	}, true)

	digest, ok := cachedDigest(s, cacheTestRepo+"@"+cacheTestDigest, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("a digest reference must resolve against the stored manifest digest")
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
	}
}

// Pinning a multi-arch image pins the index digest, and the store is keyed by
// the per-platform manifest digest -- so the pinned digest names bytes that are
// never on disk. The recorded index digest is what closes that gap.
func TestCachedDigestHitByIndexDigestRef(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      cacheTestDigest,
		IndexDigest: cacheTestIndexDigest,
	}, true)

	digest, ok := cachedDigest(s, cacheTestRepo+"@"+cacheTestIndexDigest, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("an index digest reference must resolve to the stored platform variant")
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want the manifest digest %q", digest, cacheTestDigest)
	}
}

// Every platform variant of a multi-arch image shares one index digest, so the
// platform filter is the only thing separating them. Without it an index digest
// pin under --platform linux/amd64 could boot the arm64 rootfs.
func TestCachedDigestIndexRefHonorsPlatform(t *testing.T) {
	s := newCacheTestStore(t)
	const amdDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      cacheTestDigest,
		Platform:    ociclient.DefaultPlatform,
		IndexDigest: cacheTestIndexDigest,
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      amdDigest,
		Platform:    "linux/amd64",
		IndexDigest: cacheTestIndexDigest,
	}, true)

	indexRef := cacheTestRepo + "@" + cacheTestIndexDigest
	digest, ok := cachedDigest(s, indexRef, "linux/amd64")
	if !ok || digest != amdDigest {
		t.Fatalf("amd64 lookup = %q,%v; want %q", digest, ok, amdDigest)
	}
	digest, ok = cachedDigest(s, indexRef, ociclient.DefaultPlatform)
	if !ok || digest != cacheTestDigest {
		t.Fatalf("default lookup = %q,%v; want %q", digest, ok, cacheTestDigest)
	}
}

// A digest names bytes, not an image, and the same manifest can be pushed to
// several repositories. A pin must not be satisfied by an image the user did
// not name, whichever of the two digests it matches.
func TestCachedDigestDigestRefRequiresSameRepository(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      cacheTestDigest,
		IndexDigest: cacheTestIndexDigest,
	}, true)

	for _, ref := range []string{
		"ghcr.io/someone-else/urunc-ubuntu@" + cacheTestDigest,
		"ghcr.io/someone-else/urunc-ubuntu@" + cacheTestIndexDigest,
		"ghcr.io/nofireai/other-image@" + cacheTestDigest,
	} {
		if _, ok := cachedDigest(s, ref, ociclient.DefaultPlatform); ok {
			t.Errorf("%s must not be answered by an image from another repository", ref)
		}
	}
}

// The completeness rule applies to a digest reference like any other lookup:
// metadata with no rootfs is a miss, so the caller re-pulls and heals it.
func TestCachedDigestDigestRefMissesIncompleteImage(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      cacheTestDigest,
		IndexDigest: cacheTestIndexDigest,
	}, false)

	for _, ref := range []string{
		cacheTestRepo + "@" + cacheTestDigest,
		cacheTestRepo + "@" + cacheTestIndexDigest,
	} {
		if _, ok := cachedDigest(s, ref, ociclient.DefaultPlatform); ok {
			t.Errorf("%s must not hit while the rootfs is missing", ref)
		}
	}
}

// An image pulled by digest records that reference in Ref, and running it again
// must still hit.
func TestCachedDigestHitWhenStoredRefIsADigestRef(t *testing.T) {
	s := newCacheTestStore(t)
	digestRef := cacheTestRepo + "@" + cacheTestIndexDigest
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         digestRef,
		Digest:      cacheTestDigest,
		IndexDigest: cacheTestIndexDigest,
	}, true)

	digest, ok := cachedDigest(s, digestRef, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("a stored digest reference must resolve on the next run")
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
	}
}

// The offline half of the bug. --pull=never used to report a pinned image as
// absent with its rootfs on disk; the nil client is the assertion that nothing
// reaches the network.
func TestResolveImageDigestNeverAcceptsDigestRef(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:         cacheTestRef,
		Digest:      cacheTestDigest,
		IndexDigest: cacheTestIndexDigest,
	}, true)

	for _, ref := range []string{
		cacheTestRepo + "@" + cacheTestDigest,
		cacheTestRepo + "@" + cacheTestIndexDigest,
	} {
		digest, err := resolveImageDigest(context.Background(), nil, s, ref, pullNever, ociclient.DefaultPlatform)
		if err != nil {
			t.Fatalf("resolveImageDigest(%s): %v", ref, err)
		}
		if digest != cacheTestDigest {
			t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
		}
	}
}

// Records written before the index digest was tracked carry none, so an image
// pulled under an index digest reference has only that reference to be found
// by. Matching the stored reference as a string is what keeps it findable;
// without it the image is a permanent miss, re-pulled on every run and refused
// outright under --pull=never.
func TestCachedDigestHitForLegacyDigestRefRecord(t *testing.T) {
	s := newCacheTestStore(t)
	ref := cacheTestRepo + "@" + cacheTestIndexDigest
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:    ref,
		Digest: cacheTestDigest,
	}, true)

	digest, ok := cachedDigest(s, ref, ociclient.DefaultPlatform)
	if !ok {
		t.Fatal("a record with no index digest must still answer the reference it was pulled under")
	}
	if digest != cacheTestDigest {
		t.Errorf("digest = %q, want %q", digest, cacheTestDigest)
	}
}
