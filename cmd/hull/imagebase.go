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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// An image's base is the ext4 image `hull run` builds from an OCI image's
// rootfs, with the ownership and modes the image records, and nothing of
// any one instance: no /urunit.conf, no init wrapper, no /etc/hosts. It is
// built once per image (and size) on a host and kept under <store>/bases by
// its disk digest; every instance of the image on that host starts from a
// copy of it, with its own files written in. Instances of one image then
// share one base, and a destination that holds it takes a move of any of
// them by the blocks that differ (docs/migration.md, "Bases").
//
// mke2fs writes a fresh UUID, hash seed and timestamps into every image, so
// two hosts build two bases of one image: a base goes to another host once
// per (image, host it was built on, destination).

// imageBaseRecipe changes whenever hull builds an image's base differently,
// so a base built the old way is not taken for one built the new way.
const imageBaseRecipe = "hull-image-base-1"

// imageBaseKey names the base of image imageDigest built at sizeMB, or is
// "" for an image hull cannot name by content (a local bundle).
func imageBaseKey(imageDigest string, sizeMB int) string {
	if !strings.HasPrefix(imageDigest, "sha256:") {
		return ""
	}
	return sha256Hex([]byte(fmt.Sprintf("%s\x00%s\x00%d", imageBaseRecipe, imageDigest, sizeMB)))
}

// imageBaseEntry is <store>/bases/images/<key>.json: which base an image's
// instances start from on this host.
type imageBaseEntry struct {
	Digest  string    `json:"digest"`
	Image   string    `json:"image"`
	SizeMB  int       `json:"sizeMB"`
	Recipe  string    `json:"recipe"`
	Created time.Time `json:"created"`
}

// imageBasesDir is where the entries live.
func imageBasesDir(s *store.Store) string { return filepath.Join(s.BasesDir(), "images") }

// readImageBaseEntry reads the entry of key, nil when there is none.
func readImageBaseEntry(s *store.Store, key string) (*imageBaseEntry, error) {
	data, err := os.ReadFile(filepath.Join(imageBasesDir(s), key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e imageBaseEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("the image base entry %s: %w", key, err)
	}
	if err := checkDiskDigest(e.Digest); err != nil {
		return nil, fmt.Errorf("the image base entry %s: %w", key, err)
	}
	return &e, nil
}

// imageBaseEntries lists every entry, by key.
func imageBaseEntries(s *store.Store) (map[string]*imageBaseEntry, error) {
	names, err := os.ReadDir(imageBasesDir(s))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*imageBaseEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]*imageBaseEntry{}
	for _, n := range names {
		key, ok := strings.CutSuffix(n.Name(), ".json")
		if !ok || checkDiskDigest(key) != nil {
			continue
		}
		e, err := readImageBaseEntry(s, key)
		if err != nil || e == nil {
			continue
		}
		out[key] = e
	}
	return out, nil
}

// lockImageBase holds the build of key's base, so two runs of one image
// build it once.
func lockImageBase(s *store.Store, key string) (func(), error) {
	if err := os.MkdirAll(imageBasesDir(s), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(imageBasesDir(s), key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// imageBase returns the digest of the base of the image whose rootfs is
// rootfsDir, and whether this call built it: the one the store keeps for
// imageDigest at sizeMB, or a new one, built, kept, and recorded as that
// image's (a local image's is built every time and recorded nowhere).
func imageBase(s *store.Store, imageDigest, rootfsDir string, sizeMB int) (string, bool, error) {
	key := imageBaseKey(imageDigest, sizeMB)
	if key != "" {
		unlock, err := lockImageBase(s, key)
		if err != nil {
			return "", false, err
		}
		defer unlock()
		e, err := readImageBaseEntry(s, key)
		if err != nil {
			log.Warnf("%v; building the image's base again", err)
		} else if e != nil && baseHeld(s, e.Digest) {
			return e.Digest, false, nil
		}
	}
	if err := os.MkdirAll(s.BasesDir(), 0o700); err != nil {
		return "", false, err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(s.BasesDir(), ".new-"+hex.EncodeToString(rnd[:]))
	err := makeBlockImage(tmp, rootfsDir, sizeMB)
	if err == nil {
		err = fixupBlockImage(tmp, rootfsDir, nil)
	}
	var sum string
	if err == nil {
		sum, _, err = diskDigestFile(tmp)
	}
	if err == nil {
		err = sealBase(s, tmp, sum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	if key != "" {
		js, err := json.MarshalIndent(imageBaseEntry{Digest: sum, Image: imageDigest, SizeMB: sizeMB,
			Recipe: imageBaseRecipe, Created: time.Now().UTC()}, "", "  ")
		if err != nil {
			return "", false, err
		}
		if err := writeFileAtomic(filepath.Join(imageBasesDir(s), key+".json"), append(js, '\n')); err != nil {
			return "", false, err
		}
	}
	return sum, true, nil
}

// writeFileAtomic writes data to path through a synced temporary file
// renamed over it, then syncs the directory.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFileSynced(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// diskFromImageBase makes the instance's disk at diskPath: a copy of its
// image's base (a clone where the filesystem has them), with the
// instance's own files written in. It returns the base's digest.
func diskFromImageBase(s *store.Store, imageDigest, diskPath, rootfsDir string, injects []blockInject,
	sizeMB int) (string, error) {
	started := time.Now()
	sum, built, err := imageBase(s, imageDigest, rootfsDir, sizeMB)
	if err != nil {
		return "", fmt.Errorf("the image's base: %w", err)
	}
	_ = os.Remove(diskPath)
	how, err := copyDisk(s.BasePath(sum), diskPath)
	if err != nil {
		return "", fmt.Errorf("copy the image's base: %w", err)
	}
	if err := os.Chmod(diskPath, 0o600); err != nil {
		return "", err
	}
	if err := fixupBlockImage(diskPath, "", injects); err != nil {
		return "", err
	}
	log.Debugf("Disk %s from base %s (built %v, %s) in %s", diskPath, digestHead(sum), built, how,
		time.Since(started))
	return sum, nil
}
