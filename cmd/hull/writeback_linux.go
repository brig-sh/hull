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

	"golang.org/x/sys/unix"
)

// canStartWriteback says startWriteback does something here.
const canStartWriteback = true

// startWriteback asks the kernel to start writing n bytes of f from off to
// disk, and returns at once: no wait, no sync of the metadata or of the
// journal; n 0 means to the end of the file. A long stream written through the page cache then never piles
// up enough dirty pages for the kernel to throttle the writer.
func startWriteback(f *os.File, off, n int64) {
	_ = unix.SyncFileRange(int(f.Fd()), off, n, unix.SYNC_FILE_RANGE_WRITE)
}
