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
	"fmt"
	"os"
)

// ensureStore makes storeDir ready to hold a store on Linux: a plain directory.
//
// The macOS build mounts a case-sensitive APFS sparse image here (see
// storemount_darwin.go), because the default macOS volume folds case and a
// Linux rootfs holds names that differ only by it. A Linux filesystem is case
// sensitive already, and there is no hdiutil, so the directory itself is the
// store. It is still probed rather than assumed, because a store pointed at a
// vfat or exFAT mount, or a case-folding ext4 directory, would unpack an image
// whose colliding names silently overwrite each other and boot a guest that
// is not the image.
//
// 0700 because the mode bits are the store's only access control: the
// instance state, the urunit config with the guest's environment, and the
// block image all live under it.
func ensureStore(storeDir, _ string) error {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return fmt.Errorf("create store directory: %w", err)
	}
	sensitive, err := dirIsCaseSensitive(storeDir)
	if err != nil {
		return fmt.Errorf("check whether %s is case-sensitive: %w", storeDir, err)
	}
	if !sensitive {
		return fmt.Errorf("%s is on a case-insensitive filesystem; a Linux rootfs holds names "+
			"that differ only by case, so point --store-dir at a case-sensitive one", storeDir)
	}
	return nil
}

// storeCompactHint is empty: a plain directory gives freed space back at once.
func storeCompactHint(string) string { return "" }
