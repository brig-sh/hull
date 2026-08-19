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
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// A pull must publish the layout version it wrote, not just the bytes. Without
// the stamp on the publish path, every image a current hull writes still looks
// like an old one and the store never converges.
func TestPullStampsTheUnpackSchema(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	res, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	imageDir := filepath.Join(s.RootDir(), "images", res.Digest)
	if got := store.ReadUnpackSchema(imageDir); got != store.UnpackSchema {
		t.Errorf("published image carries schema %d, want %d", got, store.UnpackSchema)
	}
}

// The self-heal, end to end and in the user's own shape: a store already holds
// a rootfs written by an older hull -- present, populated, and missing the
// ownership records the guest needs. The next pull must rewrite it rather than
// short-circuit on the unchanged digest.
//
// The sentinel is what makes this a real test. TestPullSkipsUnchangedDigest
// uses the same file to prove the short circuit DOES fire for a current image;
// here it must not survive, so the two tests cannot both pass unless the
// schema is what decides.
func TestPullRewritesAnOlderLayout(t *testing.T) {
	ref := testRegistry(t, "hello from the test layer")
	c, s := newClient(t)

	first, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	imageDir := filepath.Join(s.RootDir(), "images", first.Digest)

	// Age the image back to the layout the reported store was in.
	if err := os.Remove(filepath.Join(imageDir, "unpack-schema")); err != nil {
		t.Fatalf("remove stamp: %v", err)
	}
	sentinel := filepath.Join(imageDir, "rootfs", ".sentinel")
	if err := os.WriteFile(sentinel, []byte("from the old unpack"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if s.ImageComplete(first.Digest) {
		t.Fatal("precondition: an unstamped image must not be a cache hit")
	}

	second, err := c.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if second.Digest != first.Digest {
		t.Fatalf("digest changed: %s -> %s", first.Digest, second.Digest)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("the old rootfs survived: pull short-circuited on a stale layout " +
			"instead of re-unpacking it")
	}
	if got := store.ReadUnpackSchema(imageDir); got != store.UnpackSchema {
		t.Errorf("after the repair the image carries schema %d, want %d", got, store.UnpackSchema)
	}
	if !s.ImageComplete(second.Digest) {
		t.Error("the repaired image should be a cache hit")
	}
}
