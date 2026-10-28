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
	"encoding/json"
	"regexp"
	"testing"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// hull images through the command tree a user runs, against a live daemon:
// the global flags, connectContainerd, the listing, and the printed JSON
// and table. The image is written for the host's platform, the one
// listImages prefers, so the expected output holds on any host.
func TestImagesCommandAgainstDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	ns := containerdtest.NamespaceName("hull-test-cli")
	c, err := containerd.Connect(context.Background(), addr, ns)
	if err != nil {
		t.Fatal(err)
	}
	// Closed by a cleanup, not a defer: the cleanups that delete the test's
	// image and lease run after the test returns and still need the client.
	t.Cleanup(func() { _ = c.Close() })
	containerdtest.DeleteNamespace(t, c)
	ctx := containerdtest.Lease(context.Background(), t, c)
	cs := c.ContentStore()

	host := platforms.DefaultSpec()
	manifest := containerdtest.WriteImage(ctx, t, cs, host, map[string]string{"org.example": "yes"}, true)
	index := containerdtest.WriteBlob(ctx, t, cs, ocispec.MediaTypeImageIndex, ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifest},
	})
	const name = "example.com/hull/cli:1"
	containerdtest.CreateImage(ctx, t, c, name, index)

	run := func(namespace string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		app := newApp()
		app.Writer = &out
		argv := append([]string{"hull", "--containerd-address", addr, "--namespace", namespace}, args...)
		err := app.Run(context.Background(), argv)
		if err != nil {
			t.Fatalf("hull %v: %v", args, err)
		}
		return out.String()
	}

	var recs []store.ImageMetadata
	err = json.Unmarshal([]byte(run(ns, "images", "--json")), &recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("images --json listed %d records, want 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Ref != name || rec.Digest != manifest.Digest.String() || rec.IndexDigest != index.Digest.String() {
		t.Errorf("record = %+v; want %s at %s through index %s", rec, name, manifest.Digest, index.Digest)
	}
	if rec.Platform != platforms.Format(host) || rec.Size != 1024 || rec.Labels["org.example"] != "yes" {
		t.Errorf("record = %+v; want %s, 1024 bytes, label org.example=yes", rec, platforms.Format(host))
	}

	table := run(ns, "images")
	row := regexp.MustCompile(`(?m)^example\.com/hull/cli +1 +` + regexp.QuoteMeta(manifest.Digest.String()[:19]) + ` +1\.0 KB +`)
	if !row.MatchString(table) {
		t.Errorf("table has no row for %s:\n%s", name, table)
	}

	got := run(ns+"-empty", "images")
	if got != "No images found\n" {
		t.Errorf("another namespace printed %q, want No images found", got)
	}
}
