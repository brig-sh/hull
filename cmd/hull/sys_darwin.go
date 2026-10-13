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
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// statusChangeTime is the inode's ctime, which a rename updates and a copy of
// the file times does not. Zero when the platform does not report one, which
// reads as ancient and prunes.
func statusChangeTime(fi os.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}

// osProductVersion returns the macOS version (major.minor) for the event
// envelope; empty on failure, never an error.
func osProductVersion() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
