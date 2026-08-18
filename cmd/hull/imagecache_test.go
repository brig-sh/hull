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

// seedImage writes an image into the store. withRootfs=false reproduces what an
// interrupted pull leaves behind: metadata present, rootfs gone.
func seedImage(t *testing.T, s *store.Store, withRootfs bool) {
	t.Helper()
	dir, err := s.SaveImage(cacheTestDigest, &store.ImageMetadata{
		Ref:      cacheTestRef,
		Digest:   cacheTestDigest,
		PulledAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if withRootfs {
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0755); err != nil {
			t.Fatalf("MkdirAll rootfs: %v", err)
		}
	}
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

	digest, ok := cachedDigest(s, cacheTestRef, "linux/amd64")
	if !ok || digest != amdDigest {
		t.Fatalf("amd64 lookup = %q,%v; want the amd64 digest", digest, ok)
	}
	digest, ok = cachedDigest(s, cacheTestRef, ociclient.DefaultPlatform)
	if !ok || digest == amdDigest {
		t.Fatalf("default lookup = %q,%v; must still resolve the arm64 record", digest, ok)
	}
}
