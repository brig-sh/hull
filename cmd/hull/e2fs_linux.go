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
	"os"
	"os/exec"
	"path/filepath"
)

// e2fsprogs on Linux, found where distributions install it.
//
// /usr/sbin and /sbin are searched before PATH because they are usually not
// ON an ordinary user's PATH: Debian and Raspberry Pi OS put mke2fs in /sbin
// and give a non-root login a PATH without it, so a LookPath-only search fails
// on exactly the hosts that have it installed.
var (
	mke2fsBin  = findE2fsTool("mke2fs")
	debugfsBin = findE2fsTool("debugfs")
	e2fsckBin  = findE2fsTool("e2fsck")
)

// findE2fsTool returns the first executable named name in /usr/sbin, /sbin or
// PATH. When there is none it still returns a path, /usr/sbin/<name>, so the
// error buildBlockRootfs reports names a concrete file rather than a bare
// program name.
func findE2fsTool(name string) string {
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return filepath.Join("/usr/sbin", name)
}
