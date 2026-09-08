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

	"golang.org/x/sys/unix"
)

// unixSocketPathMax is the longest unix socket path the kernel binds: the
// sun_path array less its terminating NUL, 103 bytes on macOS and 107 on
// Linux. Longer paths fail deep inside the VMM ("path must be shorter than
// SUN_LEN") after the instance has been staged, so hull checks up front.
const unixSocketPathMax = len(unix.RawSockaddrUnix{}.Path) - 1

// checkUnixSocketPath rejects a socket path the kernel cannot bind, naming
// the path, its length and the way out. The store directory is the only
// part of the path a user controls, so that is what the message points at.
func checkUnixSocketPath(what, path string) error {
	if len(path) <= unixSocketPathMax {
		return nil
	}
	return fmt.Errorf("%s socket path is %d bytes, the limit for a unix socket on this OS is %d: %s\n"+
		"move the store to a shorter path (--store-dir) or use a shorter instance name",
		what, len(path), unixSocketPathMax, path)
}
