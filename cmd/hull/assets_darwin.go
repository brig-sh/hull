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

import "github.com/urfave/cli/v3"

const assetsDirUsage = "print the boot asset directory for this store, and nothing else"

// assetsStoreDir is the store whose boot assets the assets commands act on.
func assetsStoreDir(cmd *cli.Command) string {
	return cmd.String("store-dir")
}

// prepareAssetsStore makes the store ready for `hull assets pull` to write
// into.
func prepareAssetsStore(storeDir string) error {
	// The assets live under the store, so the store has to be
	// real before anything is written into it. Skipping this
	// leaves the files on the bare mountpoint, and the next
	// command that opens the store refuses to mount over them
	// -- a fresh install would pull its assets and then be
	// unable to run anything.
	//
	// `show` and `dir` do not do this: they only read a path,
	// and mounting a 200GB image to answer a question would be
	// the same overreach that made every command mount one.
	return ensureStore(storeDir, defaultStoreDir())
}
