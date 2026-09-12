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

//go:build darwin

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// otherTestDigest is a second store key, for the cases that need two images.
const otherTestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func seedInstance(t *testing.T, s *store.Store, id, digest, status string, pid int) {
	t.Helper()
	if _, err := s.CreateInstance(id); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID:          id,
		ImageDigest: digest,
		Status:      status,
		PID:         pid,
		StartTime:   time.Now(),
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

func TestRmiByRefDigestAndPrefix(t *testing.T) {
	for _, ref := range []string{cacheTestRef, cacheTestDigest, cacheTestDigest[:19]} {
		t.Run(ref, func(t *testing.T) {
			s := newCacheTestStore(t)
			seedImage(t, s, true)

			var out bytes.Buffer
			if err := removeImagesIn(&out, s, []string{ref}, "", false); err != nil {
				t.Fatalf("removeImagesIn: %v", err)
			}
			if !strings.Contains(out.String(), "Removed "+cacheTestDigest) {
				t.Errorf("output does not name what it deleted: %q", out.String())
			}
			if _, err := os.Stat(s.ImageDir(cacheTestDigest)); !os.IsNotExist(err) {
				t.Errorf("the image directory survived: %v", err)
			}
		})
	}
}

// A republished tag leaves two digests answering one reference, which is the
// case this command exists for. Both go.
func TestRmiRemovesEveryDigestAnsweringTheTag(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, PulledAt: time.Now().Add(-time.Hour),
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{Ref: cacheTestRef, Digest: otherTestDigest}, true)

	var out bytes.Buffer
	if err := removeImagesIn(&out, s, []string{cacheTestRef}, "", false); err != nil {
		t.Fatalf("removeImagesIn: %v", err)
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("%d image(s) still answer the tag", len(images))
	}
}

// A digest prefix that matches two images names one of them and cannot say
// which, so removing either would be a guess.
func TestRmiRefusesAnAmbiguousPrefix(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{Ref: cacheTestRef, Digest: otherTestDigest}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref:    "ghcr.io/nofireai/other:aarch64",
		Digest: "sha256:1111112222222222222222222222222222222222222222222222222222222222",
	}, true)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{"sha256:111111"}, "", false)
	if err == nil {
		t.Fatal("an ambiguous prefix was accepted")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error does not say why: %v", err)
	}
	images, _ := s.ListImages()
	if len(images) != 2 {
		t.Errorf("the refusal still removed something: %d image(s) left", len(images))
	}
}

func TestRmiUnknownRefStillRemovesTheOthers(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{"ghcr.io/nofireai/nope:v1", cacheTestRef}, "", false)
	if err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("error = %v, want a no-such-image report", err)
	}
	if _, statErr := os.Stat(s.ImageDir(cacheTestDigest)); !os.IsNotExist(statErr) {
		t.Error("the known reference was not removed")
	}
}

// An instance that is not running still refers to its image: its bundle can be
// a bare symlink into the cache. Removing it silently would break a VM the
// operator can still see in `hull ps`.
func TestRmiRefusesAnImageAStoppedInstanceRefers(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	seedInstance(t, s, "held", cacheTestDigest, "stopped", 0)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{cacheTestRef}, "", false)
	if err == nil {
		t.Fatal("the image was removed out from under an instance")
	}
	if !strings.Contains(err.Error(), "held") {
		t.Errorf("error does not name the instance: %v", err)
	}
	if _, statErr := os.Stat(s.ImageDir(cacheTestDigest)); statErr != nil {
		t.Errorf("the refusal still removed the image: %v", statErr)
	}

	out.Reset()
	if err := removeImagesIn(&out, s, []string{cacheTestRef}, "", true); err != nil {
		t.Fatalf("--force did not remove it: %v", err)
	}
}

// A record left saying "running" by a host that crashed must not pin its image
// forever. Liveness is decided by looking at the pid, as stop and rm do, so
// such a record refuses like a stopped one -- which --force can answer --
// rather than like a live VM, which nothing can.
func TestRmiTreatsAStaleRunningRecordAsStopped(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	seedInstance(t, s, "stale", cacheTestDigest, "running", stalePID)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{cacheTestRef}, "", false)
	if err == nil {
		t.Fatal("an instance record still referring to the image did not refuse")
	}
	// The refusal has to be the stopped-holder one, which --force can answer,
	// and not the live-VM one, which nothing can.
	if strings.Contains(err.Error(), "is in use by running") {
		t.Errorf("a stale record was read as a live VM: %v", err)
	}
	if !strings.Contains(err.Error(), "is referenced by") {
		t.Errorf("unexpected refusal: %v", err)
	}

	out.Reset()
	if err := removeImagesIn(&out, s, []string{cacheTestRef}, "", true); err != nil {
		t.Fatalf("--force did not clear a stale record: %v", err)
	}
	if _, err := os.Stat(s.ImageDir(cacheTestDigest)); !os.IsNotExist(err) {
		t.Error("a stale running record blocked the removal")
	}
}

// stalePID is above the macOS pid ceiling, so it can never name a live
// process and the liveness check can only answer one way.
const stalePID = 999999

// --force covers a stopped instance's claim on an image. It must not cover a
// running one: that VM is reading the image cache right now, on the vz generic
// path directly over virtiofs.
func TestRmiNeverRemovesAnImageALiveVMIsBooting(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	img, err := s.GetImage(cacheTestDigest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}

	var out bytes.Buffer
	held := []imageHolder{{id: "live", running: true}}
	for _, force := range []bool{false, true} {
		err := removeOneImage(&out, s, img, held, force)
		if err == nil {
			t.Fatalf("force=%v removed an image a live VM is booting", force)
		}
		if !strings.Contains(err.Error(), "is in use by running instance live") {
			t.Errorf("not the live-VM refusal, or it does not name the instance: %v", err)
		}
	}
	if _, err := os.Stat(s.ImageDir(cacheTestDigest)); err != nil {
		t.Errorf("the refusal still removed the image: %v", err)
	}
}

// "No such image" for a tag the store does have, under a platform the caller
// did not ask for, sends the reader looking for a typo that is not there.
func TestRmiSaysWhenOnlyThePlatformMissed(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, Platform: "linux/arm64",
	}, true)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{cacheTestRef}, "linux/amd64", false)
	if err == nil {
		t.Fatal("a platform that matches nothing was accepted")
	}
	if !strings.Contains(err.Error(), "not for platform linux/amd64") {
		t.Errorf("error does not name the platform: %v", err)
	}
	if _, statErr := os.Stat(s.ImageDir(cacheTestDigest)); statErr != nil {
		t.Errorf("the refusal still removed the image: %v", statErr)
	}
}

func TestRmiPlatformNarrowsTheMatch(t *testing.T) {
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, Platform: "linux/arm64",
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: otherTestDigest, Platform: "linux/amd64",
	}, true)

	var out bytes.Buffer
	if err := removeImagesIn(&out, s, []string{cacheTestRef}, "linux/amd64", false); err != nil {
		t.Fatalf("removeImagesIn: %v", err)
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 1 || images[0].Digest != cacheTestDigest {
		t.Fatalf("--platform removed the wrong entries: %v", images)
	}
}

// run writes a "starting" record with PID 0 before it spawns the VMM, and the
// pid write afterwards is warn-only. Reading that record as stopped let
// --force delete the rootfs of a running guest.
func TestRmiRefusesAStartingRecordWithNoPID(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	seedInstance(t, s, "spawning", cacheTestDigest, store.StatusStarting, 0)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{cacheTestRef}, "", true)
	if err == nil {
		t.Fatal("--force removed an image under a record that may have spawned a VMM")
	}
	if !strings.Contains(err.Error(), "is in use by running") {
		t.Errorf("not the live-VM refusal: %v", err)
	}
	if _, statErr := os.Stat(s.ImageDir(cacheTestDigest)); statErr != nil {
		t.Errorf("the refusal still removed the image: %v", statErr)
	}
}

// A "starting" record that carries a pid has the start-time check available,
// and skipping it left such a record refusing every removal for good: `ps`
// leaves a "starting" record alone, so nothing cleared it, and `rm --force`
// would signal whatever process inherited the recycled number.
func TestRmiClearsAStartingRecordWithAStalePID(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	seedInstance(t, s, "recycled", cacheTestDigest, store.StatusStarting, stalePID)

	var out bytes.Buffer
	err := removeImagesIn(&out, s, []string{cacheTestRef}, "", false)
	if err == nil {
		t.Fatal("a record still referring to the image did not refuse")
	}
	if strings.Contains(err.Error(), "is in use by running") {
		t.Errorf("a starting record with a dead pid was read as a live VM: %v", err)
	}

	out.Reset()
	if err := removeImagesIn(&out, s, []string{cacheTestRef}, "", true); err != nil {
		t.Fatalf("--force could not clear a starting record with a dead pid: %v", err)
	}
	if _, statErr := os.Stat(s.ImageDir(cacheTestDigest)); !os.IsNotExist(statErr) {
		t.Error("the image survived --force")
	}
}

// A bare index digest names every platform record that resolved through it. It
// used to fall through to the prefix pass and be refused as ambiguous, listing
// digests that do not start with what was typed.
func TestRmiAcceptsABareIndexDigest(t *testing.T) {
	const index = "sha256:28bd5fe8aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := newCacheTestStore(t)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: cacheTestDigest, Platform: "linux/arm64", IndexDigest: index,
	}, true)
	seedMetadata(t, s, &store.ImageMetadata{
		Ref: cacheTestRef, Digest: otherTestDigest, Platform: "linux/amd64", IndexDigest: index,
	}, true)

	var out bytes.Buffer
	if err := removeImagesIn(&out, s, []string{index}, "", false); err != nil {
		t.Fatalf("a bare index digest was refused: %v", err)
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("%d record(s) survived removal by index digest", len(images))
	}
}

// The singular and plural forms of both refusals have to agree with themselves.
func TestRmiRefusalsAgreeWithTheirCount(t *testing.T) {
	s := newCacheTestStore(t)
	seedImage(t, s, true)
	img, err := s.GetImage(cacheTestDigest)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	var out bytes.Buffer

	one := []imageHolder{{id: "a", running: true}}
	two := []imageHolder{{id: "a", running: true}, {id: "b", running: true}}
	for _, tc := range []struct {
		held []imageHolder
		want string
	}{
		{one, "running instance a; stop it first"},
		{two, "running instances a, b; stop them first"},
	} {
		err := removeOneImage(&out, s, img, tc.held, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("live refusal for %d holder(s) = %v, want %q", len(tc.held), err, tc.want)
		}
	}

	stoppedOne := []imageHolder{{id: "a"}}
	stoppedTwo := []imageHolder{{id: "a"}, {id: "b"}}
	for _, tc := range []struct {
		held []imageHolder
		want string
	}{
		{stoppedOne, "by instance a; remove it with"},
		{stoppedTwo, "by instances a, b; remove them with"},
	} {
		err := removeOneImage(&out, s, img, tc.held, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("stopped refusal for %d holder(s) = %v, want %q", len(tc.held), err, tc.want)
		}
	}
}
