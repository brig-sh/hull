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
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// rmiFixture is a namespace on the test daemon to write images into and run
// hull against.
type rmiFixture struct {
	t    *testing.T
	addr string
	ns   string
	c    *client.Client
	ctx  context.Context
}

func newRmiFixture(t *testing.T) *rmiFixture {
	t.Helper()
	addr := containerdtest.DaemonAddress(t)
	ns := containerdtest.NamespaceName("hull-test-rmi")
	c, err := containerd.Connect(context.Background(), addr, ns)
	if err != nil {
		t.Fatal(err)
	}
	// Closed by a cleanup, not a defer: the cleanups that delete the test's
	// images and lease run after the test returns and still need the client.
	t.Cleanup(func() { _ = c.Close() })
	containerdtest.DeleteNamespace(t, c)
	return &rmiFixture{t: t, addr: addr, ns: ns, c: c, ctx: containerdtest.Lease(context.Background(), t, c)}
}

// image records each name as an image whose index lists one manifest per
// platform, and returns the manifests in the order given.
func (f *rmiFixture) image(ps []ocispec.Platform, names ...string) []ocispec.Descriptor {
	f.t.Helper()
	cs := f.c.ContentStore()
	var manifests []ocispec.Descriptor
	for _, p := range ps {
		manifests = append(manifests, containerdtest.WriteImage(f.ctx, f.t, cs, p, map[string]string{"test": f.t.Name()}, true))
	}
	index := containerdtest.WriteBlob(f.ctx, f.t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	})
	for _, name := range names {
		containerdtest.CreateImage(f.ctx, f.t, f.c, name, index)
	}
	return manifests
}

// run runs hull against the fixture's daemon in namespace ns and returns
// what it printed.
func (f *rmiFixture) run(ns string, args ...string) (string, error) {
	f.t.Helper()
	var out bytes.Buffer
	app := newApp()
	app.Writer = &out
	argv := append([]string{"hull", "--containerd-address", f.addr, "--namespace", ns}, args...)
	err := app.Run(context.Background(), argv)
	return out.String(), err
}

func (f *rmiFixture) exists(name string) bool {
	f.t.Helper()
	_, err := f.c.ImageService().Get(f.ctx, name)
	if err != nil && !errdefs.IsNotFound(err) {
		f.t.Fatal(err)
	}
	return err == nil
}

var host = platforms.DefaultSpec()

// A name is found in any spelling pull accepts, and an image by its digest
// or the prefix `hull images` prints.
func TestRmiByNameDigestAndPrefix(t *testing.T) {
	const name = "docker.io/library/hull-rmi-test:1"
	for _, tc := range []struct {
		label string
		ref   func(manifest string) string
	}{
		{"short name", func(string) string { return "hull-rmi-test:1" }},
		{"full name", func(string) string { return name }},
		{"digest", func(m string) string { return m }},
		{"prefix", func(m string) string { return m[:19] }},
		{"pin", func(m string) string { return "hull-rmi-test@" + m }},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newRmiFixture(t)
			manifest := f.image([]ocispec.Platform{host}, name)[0].Digest.String()

			out, err := f.run(f.ns, "rmi", tc.ref(manifest))
			if err != nil {
				t.Fatalf("rmi %s: %v", tc.ref(manifest), err)
			}
			if out != "Removed "+manifest+" "+name+"\n" {
				t.Errorf("rmi printed %q", out)
			}
			if f.exists(name) {
				t.Error("the image survived")
			}
		})
	}
}

func TestRmiUnknownRefStillRemovesTheOthers(t *testing.T) {
	f := newRmiFixture(t)
	const name = "example.com/hull/rmi-known:1"
	f.image([]ocispec.Platform{host}, name)

	_, err := f.run(f.ns, "rmi", "example.com/hull/nope:1", name)
	if err == nil || !strings.Contains(err.Error(), "no such image: example.com/hull/nope:1") {
		t.Fatalf("error = %v, want a no-such-image report", err)
	}
	if f.exists(name) {
		t.Error("the known reference was not removed")
	}
}

// containerd keeps one image per name, so a digest can name several images.
// As `docker rmi` does with an image ID in several repositories, that takes
// --force, which removes them all.
func TestRmiDigestSharedByNamesNeedsForce(t *testing.T) {
	f := newRmiFixture(t)
	const one, two = "example.com/hull/rmi-shared:1", "example.com/hull/rmi-shared:2"
	manifest := f.image([]ocispec.Platform{host}, one, two)[0].Digest.String()

	_, err := f.run(f.ns, "rmi", manifest)
	if err == nil || !strings.Contains(err.Error(), "referenced by names "+one+", "+two) {
		t.Fatalf("error = %v, want a refusal naming both names", err)
	}
	if !f.exists(one) || !f.exists(two) {
		t.Fatal("the refusal still removed a name")
	}

	// A name is not refused: removing it leaves the image under the other.
	out, err := f.run(f.ns, "rmi", one)
	if err != nil {
		t.Fatalf("rmi %s: %v", one, err)
	}
	if out != "Removed "+manifest+" "+one+"\n" || f.exists(one) || !f.exists(two) {
		t.Errorf("rmi %s printed %q and did not remove just that name", one, out)
	}

	f.image([]ocispec.Platform{host}, one)
	out, err = f.run(f.ns, "rmi", "--force", manifest[:19])
	if err != nil {
		t.Fatalf("rmi --force: %v", err)
	}
	if strings.Count(out, "Removed "+manifest) != 2 || f.exists(one) || f.exists(two) {
		t.Errorf("rmi --force printed %q and did not remove both names", out)
	}
}

// --platform removes that platform's content and keeps the name while
// another platform remains, as `docker rmi --platform` does.
func TestRmiPlatformDropsOnePlatform(t *testing.T) {
	f := newRmiFixture(t)
	const name = "example.com/hull/rmi-platform:1"
	amd64 := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	manifests := f.image([]ocispec.Platform{amd64, arm64}, name)

	_, err := f.run(f.ns, "rmi", "--platform", "linux/s390x", name)
	if err == nil || err.Error() != "example.com/hull/rmi-platform:1 has no linux/s390x image" {
		t.Fatalf("error = %v, want a report that the name has no linux/s390x image", err)
	}

	out, err := f.run(f.ns, "rmi", "--platform", "linux/amd64", name)
	if err != nil {
		t.Fatalf("rmi --platform: %v", err)
	}
	if out != "Removed "+manifests[0].Digest.String()+" "+name+"\n" {
		t.Errorf("rmi --platform printed %q", out)
	}
	if !f.exists(name) {
		t.Fatal("the name went with one of its two platforms")
	}
	_, err = f.c.ContentStore().Info(f.ctx, manifests[0].Digest)
	if !errdefs.IsNotFound(err) {
		t.Errorf("the amd64 manifest is still in the content store: %v", err)
	}

	_, err = f.run(f.ns, "rmi", "--platform", "linux/arm64", name)
	if err != nil {
		t.Fatalf("rmi --platform of the last platform: %v", err)
	}
	if f.exists(name) {
		t.Error("the name survived its last platform")
	}
}

// A single-platform image is stored with its manifest as the name's target,
// with no index. Dropping that platform leaves the name nothing, so the name
// goes too.
func TestRmiPlatformRemovesASingleManifestName(t *testing.T) {
	f := newRmiFixture(t)
	const name = "example.com/hull/rmi-single:1"
	amd64 := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	manifest := containerdtest.WriteImage(f.ctx, t, f.c.ContentStore(), amd64, nil, true)
	containerdtest.CreateImage(f.ctx, t, f.c, name, manifest)

	out, err := f.run(f.ns, "rmi", "--platform", "linux/amd64", name)
	if err != nil {
		t.Fatalf("rmi --platform: %v", err)
	}
	if out != "Removed "+manifest.Digest.String()+" "+name+"\n" {
		t.Errorf("rmi --platform printed %q", out)
	}
	if f.exists(name) {
		t.Error("the name survived its only platform")
	}
}

// The content store holds a manifest once, so dropping a platform from one
// name drops it from every name that lists it. That takes --force.
func TestRmiPlatformSharedByNamesNeedsForce(t *testing.T) {
	f := newRmiFixture(t)
	const one, two = "example.com/hull/rmi-platform-shared:1", "example.com/hull/rmi-platform-shared:2"
	amd64 := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	manifests := f.image([]ocispec.Platform{amd64, arm64}, one, two)

	_, err := f.run(f.ns, "rmi", "--platform", "linux/amd64", one)
	if err == nil || !strings.Contains(err.Error(), "shared by names "+one+", "+two) {
		t.Fatalf("error = %v, want a refusal naming both names", err)
	}
	_, err = f.c.ContentStore().Info(f.ctx, manifests[0].Digest)
	if err != nil {
		t.Fatalf("the refusal still dropped the manifest: %v", err)
	}

	out, err := f.run(f.ns, "rmi", "--force", "--platform", "linux/amd64", one)
	if err != nil {
		t.Fatalf("rmi --force --platform: %v", err)
	}
	if strings.Count(out, "Removed "+manifests[0].Digest.String()) != 2 {
		t.Errorf("rmi --force --platform printed %q, want a line for each name", out)
	}
	if !f.exists(one) || !f.exists(two) {
		t.Error("a name went with one of its two platforms")
	}
}

// A container created from an image holds it as an instance does on macOS:
// a stopped one refuses the removal without --force.
func TestRmiRefusesAnImageAStoppedContainerHolds(t *testing.T) {
	f := newRmiFixture(t)
	const name = "example.com/hull/rmi-held:1"
	f.image([]ocispec.Platform{host}, name)
	ctx := context.Background()
	stale, err := f.c.LoadContainer(ctx, "rmi-holder")
	if err == nil {
		_ = stale.Delete(ctx)
	}
	ctr, err := f.c.NewContainer(ctx, "rmi-holder", client.WithImageName(name), client.WithSpec(&oci.Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(ctx) })

	_, err = f.run(f.ns, "rmi", name)
	if err == nil || !strings.Contains(err.Error(), "referenced by container rmi-holder; remove it, or pass --force") {
		t.Fatalf("error = %v, want a refusal naming the container", err)
	}
	if !f.exists(name) {
		t.Fatal("the refusal still removed the image")
	}

	_, err = f.run(f.ns, "rmi", "--force", name)
	if err != nil {
		t.Fatalf("rmi --force: %v", err)
	}
	if f.exists(name) {
		t.Error("--force did not remove the image")
	}
}

func TestRmiSeesOnlyItsNamespace(t *testing.T) {
	f := newRmiFixture(t)
	const name = "example.com/hull/rmi-namespace:1"
	f.image([]ocispec.Platform{host}, name)

	_, err := f.run(f.ns+"-other", "rmi", name)
	if err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("error = %v, want no such image in another namespace", err)
	}
	if !f.exists(name) {
		t.Error("another namespace removed the image")
	}
}
