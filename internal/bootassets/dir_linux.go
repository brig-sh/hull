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

package bootassets

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultDir is where the assets live when Dir has no store to put them
// under, which on Linux is always: hull has no store there.
//
// It is the directory brig fetches into when it drives nerdctl, so either
// runtime finds a bundle the other fetched, and it follows brig's rules
// (internal/runtime/bootdir_other.go in brig). A relative XDG_DATA_HOME is
// ignored, as the XDG Base Directory spec says, because honouring it would put
// the assets under whichever directory hull ran from, where brig never looks.
func defaultDir() (string, error) {
	share := os.Getenv("XDG_DATA_HOME")
	if filepath.IsAbs(share) {
		return filepath.Join(share, "brig", "assets"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to find the boot assets in: %w", err)
	}
	return filepath.Join(home, ".local", "share", "brig", "assets"), nil
}
