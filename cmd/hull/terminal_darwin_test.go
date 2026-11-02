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

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// newPTY opens a pseudo-terminal pair. Both ends are closed when the test
// finishes.
func newPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	ioctl := func(req uintptr, arg uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), req, arg); errno != 0 {
			t.Fatalf("ioctl %#x on /dev/ptmx: %v", req, errno)
		}
	}
	ioctl(unix.TIOCPTYGRANT, 0)
	ioctl(unix.TIOCPTYUNLK, 0)
	var name [128]byte
	ioctl(unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))

	path := string(name[:bytes.IndexByte(name[:], 0)])
	// O_NOCTTY: this pty must not become the test process's controlling
	// terminal, which would break the /dev/tty of whatever ran `go test`.
	s, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pty slave %s: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return m, s
}
