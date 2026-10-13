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

// e2fsprogs from Homebrew. macOS ships none of it, and Homebrew keeps the
// formula keg-only, so the binaries are not on PATH and are named by their
// keg path.
const (
	mke2fsBin  = "/opt/homebrew/opt/e2fsprogs/sbin/mke2fs"
	debugfsBin = "/opt/homebrew/opt/e2fsprogs/sbin/debugfs"
	e2fsckBin  = "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck"
)
