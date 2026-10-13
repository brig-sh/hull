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
)

// statusChangeTime is the inode's ctime; see sys_darwin.go. Linux names the
// field Ctim where macOS says Ctimespec.
func statusChangeTime(fi os.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}

// osProductVersion is empty on Linux. The envelope field is documented as the
// macOS major.minor (internal/telemetry, Config.OSVersion), and a distribution
// version in the same field would read as a macOS release on the dashboard;
// the kernel is already in the uname field beside it.
func osProductVersion() string { return "" }
