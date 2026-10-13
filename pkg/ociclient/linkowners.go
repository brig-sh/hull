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

package ociclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LinkOwner is a symlink's ownership as the image declares it.
type LinkOwner struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// linkOwnersPath is where an unpack keeps the ownership of the symlinks in
// rootfsDir: a file beside the rootfs, never inside it, where the guest would
// see it. In the store that is images/<digest>/rootfs.link-owners.json, which
// the pull publishes and rmi removes together with the rootfs.
func linkOwnersPath(rootfsDir string) string {
	return filepath.Clean(rootfsDir) + ".link-owners.json"
}

// LinkOwners returns the symlink ownership an unpack set aside for rootfsDir,
// keyed by the slash-separated path inside it. No file means nothing was set
// aside -- a root unpack, a macOS one, which records links on the link itself,
// or a tree unpacked before this existed -- and is not an error.
//
// A key whose entry is no longer a symlink is possible and harmless: a later
// layer may have removed or replaced the link. Callers consult the map only
// for entries that are symlinks in the finished tree, and the last layer to
// write a symlink at a name is also the last to write its key.
func LinkOwners(rootfsDir string) (map[string]LinkOwner, error) {
	data, err := os.ReadFile(linkOwnersPath(rootfsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owners map[string]LinkOwner
	if err := json.Unmarshal(data, &owners); err != nil {
		return nil, fmt.Errorf("read %s: %w", linkOwnersPath(rootfsDir), err)
	}
	return owners, nil
}

// mergeLinkOwners adds one layer's symlinks to what earlier layers set aside.
// It is written to a temporary file and renamed, so an interrupted pull leaves
// the previous layers' record rather than half of one.
func mergeLinkOwners(rootfsDir string, layer map[string]LinkOwner) error {
	owners, err := LinkOwners(rootfsDir)
	if err != nil {
		return err
	}
	if owners == nil {
		owners = make(map[string]LinkOwner, len(layer))
	}
	for name, o := range layer {
		owners[name] = o
	}
	data, err := json.Marshal(owners)
	if err != nil {
		return err
	}
	dst := linkOwnersPath(rootfsDir)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
