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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/authn"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"busybox":                         "docker.io/library/busybox:latest",
		"busybox:1.36":                    "docker.io/library/busybox:1.36",
		"user/app":                        "docker.io/user/app:latest",
		"ghcr.io/containerd/busybox:1.36": "ghcr.io/containerd/busybox:1.36",
		"localhost:5000/app":              "localhost:5000/app:latest",
		"busybox@sha256:" + strings.Repeat("a", 64): "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64),
	}
	for in, want := range cases {
		got, err := NormalizeRef(in)
		if err != nil {
			t.Errorf("NormalizeRef(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
	_, err := NormalizeRef("Bad Ref")
	if err == nil {
		t.Error("NormalizeRef accepted an invalid reference")
	}
}

// The registries the macOS pull may reach over plain HTTP, through
// go-containerregistry, are the ones the Linux pull may.
func TestAllowsPlainHTTP(t *testing.T) {
	cases := map[string]bool{
		"localhost:5000":       true,
		"127.0.0.1:5000":       true,
		"[::1]:5000":           true,
		"10.0.0.2:5000":        true,
		"192.168.1.10":         true,
		"registry.local":       true,
		"registry.local:5000":  true,
		"ghcr.io":              false,
		"registry-1.docker.io": false,
		"8.8.8.8:5000":         false,
	}
	for host, want := range cases {
		got := allowsPlainHTTP(host)
		if got != want {
			t.Errorf("allowsPlainHTTP(%q) = %v, want %v", host, got, want)
		}
	}
}

// fakeKeychain answers with the config it holds for a registry, or err.
type fakeKeychain struct {
	configs map[string]authn.AuthConfig
	err     error
	asked   []string
}

func (k *fakeKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	k.asked = append(k.asked, r.RegistryStr())
	if k.err != nil {
		return nil, k.err
	}
	cfg, ok := k.configs[r.RegistryStr()]
	if !ok {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(cfg), nil
}

func TestKeychainCreds(t *testing.T) {
	k := &fakeKeychain{configs: map[string]authn.AuthConfig{
		"index.docker.io": {Username: "hub-user", Password: "hub-pass"},
		"ghcr.io":         {Username: "ignored", IdentityToken: "id-token"},
	}}
	creds := (&registryCreds{keychain: k}).userSecret

	cases := []struct {
		host, user, secret string
	}{
		// containerd asks for Docker Hub by the host it pulls from; the
		// keychain files it under index.docker.io.
		{"registry-1.docker.io", "hub-user", "hub-pass"},
		// An identity token goes out as a secret with no username.
		{"ghcr.io", "", "id-token"},
		{"quay.io", "", ""},
	}
	for _, c := range cases {
		user, secret, err := creds(c.host)
		if err != nil {
			t.Errorf("creds(%q): %v", c.host, err)
			continue
		}
		if user != c.user || secret != c.secret {
			t.Errorf("creds(%q) = %q, %q; want %q, %q", c.host, user, secret, c.user, c.secret)
		}
	}
	want := []string{"index.docker.io", "ghcr.io", "quay.io"}
	if strings.Join(k.asked, " ") != strings.Join(want, " ") {
		t.Errorf("keychain asked for %v, want %v", k.asked, want)
	}

	// containerd asks again on every request; the keychain is asked once
	// per registry, under either name for Docker Hub.
	for _, host := range []string{"ghcr.io", "registry-1.docker.io", "docker.io"} {
		_, _, err := creds(host)
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(k.asked, " ") != strings.Join(want, " ") {
		t.Errorf("after repeated lookups the keychain was asked for %v, want %v", k.asked, want)
	}
}

// A credential helper that fails leaves the pull anonymous, as on macOS;
// any other keychain error fails it.
func TestKeychainCredsHelperFailure(t *testing.T) {
	helper := &fakeKeychain{err: errors.New("error getting credentials - err: exit status 1, out: `locked`")}
	user, secret, err := (&registryCreds{keychain: helper}).userSecret("ghcr.io")
	if err != nil || user != "" || secret != "" {
		t.Errorf("helper failure: got %q, %q, %v; want anonymous", user, secret, err)
	}

	broken := &fakeKeychain{err: errors.New("parsing config.json: unexpected EOF")}
	_, _, err = (&registryCreds{keychain: broken}).userSecret("ghcr.io")
	if err == nil {
		t.Error("a config that does not parse did not fail the credentials")
	}
}

// A registry token in the Docker config goes out as a bearer token on every
// request to its registry, as the macOS pull sends it, and only to that
// registry. Username and password credentials are sent only once the
// registry asks for them, so they never show on the first request.
func TestResolverSendsTheRegistryToken(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Host] = r.Header.Get("Authorization")
		mu.Unlock()
		http.NotFound(w, r)
	})
	withToken := httptest.NewServer(handler)
	defer withToken.Close()
	withPassword := httptest.NewServer(handler)
	defer withPassword.Close()
	tokenHost := strings.TrimPrefix(withToken.URL, "http://")
	passwordHost := strings.TrimPrefix(withPassword.URL, "http://")

	resolver := newResolver(&fakeKeychain{configs: map[string]authn.AuthConfig{
		tokenHost:    {RegistryToken: "registry-token"},
		passwordHost: {Username: "user", Password: "pass"},
	}})
	ctx := context.Background()
	for _, host := range []string{tokenHost, passwordHost} {
		// The registries answer 404 to everything, so the resolve fails;
		// only the request it sent matters.
		_, _, err := resolver.Resolve(ctx, host+"/app:1")
		if err == nil {
			t.Fatalf("resolving on %s succeeded against a registry that has nothing", host)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if seen[tokenHost] != "Bearer registry-token" {
		t.Errorf("registry with a token got Authorization %q, want the bearer token", seen[tokenHost])
	}
	got, ok := seen[passwordHost]
	if !ok {
		t.Fatalf("registry with a password got no request")
	}
	if got != "" {
		t.Errorf("registry with a password got Authorization %q before asking for it", got)
	}
}

// A pull of linux/arm64 whose layers containerd did not download, because
// their snapshots already existed, leaves the arm64 image without layers
// while an earlier amd64 pull's image is complete. DescribeImage then
// describes the amd64 image; DescribePlatform, which the pull uses,
// describes the arm64 image it was asked for.
func TestDescribePlatformKeepsToThePlatform(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})
	img := images.Image{Name: "example.com/app:1", Target: index}
	matcher := platforms.OnlyStrict(linuxARM64)

	got, err := DescribePlatform(ctx, cs, img, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != arm.Digest.String() || got.Platform != "linux/arm64" {
		t.Errorf("DescribePlatform = %s %s, want the arm64 manifest %s", got.Digest, got.Platform, arm.Digest)
	}

	got, err = DescribeImage(ctx, cs, img, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != amd.Digest.String() {
		t.Errorf("DescribeImage = %s, want the complete amd64 manifest %s", got.Digest, amd.Digest)
	}
}

// An index with no manifest for the platform is an error, not another
// platform's image.
func TestDescribePlatformWithoutThePlatform(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd},
	})
	img := images.Image{Name: "example.com/app:1", Target: index}
	_, err := DescribePlatform(ctx, cs, img, platforms.OnlyStrict(linuxARM64))
	if err == nil {
		t.Fatal("DescribePlatform described an image of another platform")
	}
}

// The platform of an index entry is optional, and containerd pulls an entry
// without one when no entry matches. DescribePlatform describes that image
// rather than failing the pull that fetched it.
func TestDescribePlatformOfAnIndexWithoutPlatforms(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	unnamed := amd
	unnamed.Platform = nil
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{unnamed},
	})
	img := images.Image{Name: "example.com/app:1", Target: index}
	got, err := DescribePlatform(ctx, cs, img, platforms.OnlyStrict(linuxARM64))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != amd.Digest.String() || got.Platform != "linux/amd64" {
		t.Errorf("DescribePlatform = %s %s, want the entry without a platform, %s linux/amd64", got.Digest, got.Platform, amd.Digest)
	}
}

// When an entry matches, containerd never pulls an entry without a platform,
// so a complete one from an earlier pull does not outrank the matching entry
// whose layers this pull left missing.
func TestDescribePlatformPrefersTheMatchingEntry(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	amd := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	amd.Platform = nil
	arm := containerdtest.WriteImage(ctx, t, cs, linuxARM64, nil, false)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	})
	img := images.Image{Name: "example.com/app:1", Target: index}
	got, err := DescribePlatform(ctx, cs, img, platforms.OnlyStrict(linuxARM64))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != arm.Digest.String() {
		t.Errorf("DescribePlatform = %s, want the matching arm64 entry %s", got.Digest, arm.Digest)
	}
}

// The progress reporter learns the layers from the manifest and counts each
// one as the fetch handler returns for it.
func TestPullProgress(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	manifest := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	var out bytes.Buffer
	p := newPullProgress(&out)
	h := p.wrap(images.ChildrenHandler(cs))
	err := images.Walk(ctx, h, manifest)
	if err != nil {
		t.Fatal(err)
	}
	p.done(true)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		"pull: 1/2 layers, 1000 B of 1.0 KB downloaded",
		"pull: 2/2 layers, 1.0 KB of 1.0 KB downloaded",
		"pull: 2/2 layers, 1.0 KB downloaded",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("progress output:\n%s\nwant:\n%s", out.String(), strings.Join(want, "\n"))
	}
}

// containerd's unpacker does not fetch a layer whose snapshot already exists,
// so the handler sees only some layers finish. A pull that succeeds still
// ends with every layer counted, and the bytes of only those it fetched; a
// pull that fails ends with the layers that did finish.
func TestPullProgressCountsSkippedLayersOnSuccess(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	manifest := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	for _, tc := range []struct {
		complete bool
		want     string
	}{
		{true, "pull: 2/2 layers, 1000 B downloaded"},
		{false, "pull: 1/2 layers, 1000 B downloaded"},
	} {
		var out bytes.Buffer
		p := newPullProgress(&out)
		h := p.wrap(images.ChildrenHandler(cs))
		children, err := h.Handle(ctx, manifest)
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			if images.IsLayerType(child.MediaType) {
				_, err = h.Handle(ctx, child)
				if err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		p.done(tc.complete)
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		got := lines[len(lines)-1]
		if got != tc.want {
			t.Errorf("done(%v) printed %q, want %q", tc.complete, got, tc.want)
		}
	}
}

// A layer the last poll caught part-way counts at its full size once it
// finishes, not at its size plus the offset the poll read.
func TestPullProgressCountsAFinishedLayerOnce(t *testing.T) {
	ctx := context.Background()
	cs := newContentStore(t)
	manifest := containerdtest.WriteImage(ctx, t, cs, linuxAMD64, nil, true)
	var out bytes.Buffer
	p := newPullProgress(&out)
	h := p.wrap(images.ChildrenHandler(cs))
	children, err := h.Handle(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var layer ocispec.Descriptor
	for _, child := range children {
		if images.IsLayerType(child.MediaType) {
			layer = child
			break
		}
	}
	p.mu.Lock()
	p.inFlight[layer.Digest] = 600
	p.mu.Unlock()
	_, err = h.Handle(ctx, layer)
	if err != nil {
		t.Fatal(err)
	}
	want := "pull: 1/2 layers, 1000 B of 1.0 KB downloaded\n"
	if out.String() != want {
		t.Errorf("progress output %q, want %q", out.String(), want)
	}
}

// A pull that fetched no layer prints nothing, as when the image is already
// unpacked.
func TestPullProgressSilentWhenNothingFetched(t *testing.T) {
	var out bytes.Buffer
	p := newPullProgress(&out)
	p.done(true)
	if out.Len() != 0 {
		t.Errorf("progress printed %q for a pull that fetched nothing", out.String())
	}
	var nilProgress *pullProgress
	nilProgress.done(true)
	nilProgress.watch(context.Background(), nil)()
}

// testPullImage is containerd's copy of busybox on ghcr.io, which keeps
// Docker Hub's rate limits out of CI.
const testPullImage = "ghcr.io/containerd/busybox:1.36"

// TestPullThroughDaemon pulls a public image into a namespace of its own with
// a Docker config whose credential helper fails, as Docker's does in a
// headless session. The pull must go ahead anonymously, store the image under
// its full name, and unpack it into the default snapshotter.
func TestPullThroughDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)

	bin := t.TempDir()
	helper := filepath.Join(bin, "docker-credential-hull-test-locked")
	asked := filepath.Join(bin, "asked")
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\necho 'keychain is locked' >&2\nexit 1\n", asked)
	err := os.WriteFile(helper, []byte(script), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dockerConfig := t.TempDir()
	err = os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"credsStore":"hull-test-locked"}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfig)

	ctx := context.Background()
	c, err := Connect(ctx, addr, containerdtest.NamespaceName("hull-test-pull"))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last in, first out, so the client is still open while
	// the image and namespace are removed.
	t.Cleanup(func() { _ = c.Close() })
	containerdtest.DeleteNamespace(t, c)

	// What hull pull asks for by default: the host's architecture, with no
	// variant.
	platform := ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}
	got, err := Pull(ctx, c, testPullImage, platform, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(asked)
	if err != nil {
		t.Errorf("the pull never asked the credential helper: %v", err)
	}
	if got.Name != testPullImage {
		t.Errorf("image stored as %q, want %q", got.Name, testPullImage)
	}
	if got.IndexDigest == "" || got.Digest == "" || got.Digest == got.IndexDigest {
		t.Errorf("digests = %q (index %q), want a platform manifest under an index", got.Digest, got.IndexDigest)
	}
	// The platform comes from the image config, which may name a variant
	// the request left out: busybox's arm64 config says linux/arm64/v8.
	// Both are compared normalized, as the pull matched them.
	gotPlatform, err := platforms.Parse(got.Platform)
	if err != nil {
		t.Fatalf("platform %q: %v", got.Platform, err)
	}
	if !platforms.OnlyStrict(platform).Match(gotPlatform) {
		t.Errorf("platform = %q, want %q", got.Platform, platforms.Format(platform))
	}
	if got.Size == 0 {
		t.Error("size is 0")
	}

	img, err := c.GetImage(ctx, testPullImage)
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := img.IsUnpacked(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !unpacked {
		t.Error("the image was not unpacked into the default snapshotter")
	}

	// A second pull of an image already present succeeds without fetching
	// its layers again.
	again, err := Pull(ctx, c, testPullImage, platform, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != got.Digest {
		t.Errorf("second pull digest %s, want %s", again.Digest, got.Digest)
	}
}
