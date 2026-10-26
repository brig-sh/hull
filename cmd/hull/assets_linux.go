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

import "github.com/urfave/cli/v3"

const assetsDirUsage = "print the boot asset directory, and nothing else"

// assetsStoreDir is always empty: hull has no store on Linux, and
// bootassets.Dir gives the directory brig uses for an empty one. The
// containerd namespace does not move it, so every namespace boots the same
// kernel.
func assetsStoreDir(_ *cli.Command) string {
	return ""
}

// prepareAssetsStore has nothing to prepare: bootassets.Fetch creates the
// asset directory itself.
func prepareAssetsStore(_ string) error {
	return nil
}
