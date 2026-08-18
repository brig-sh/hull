// Copyright (c) 2023-2026, Nubificus LTD
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
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An image that nominates a FIFO as its kernel used to hang the run.
//
// The mode check lives on the descriptor, which is the right place for it, but
// it cannot run until the open returns -- and open(2) on a FIFO waits for a
// writer that never arrives. No timeout, no output, nothing to see in `ps`.
// mkfifo needs no privilege, so the whole attack is one tar entry.
//
// Both shapes are covered: the FIFO named directly, and a symlink inside the
// rootfs pointing at one, which an Lstat-based guard would have missed.
func TestBootFileResolutionDoesNotBlockOnAFifo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nominal string
		build   func(t *testing.T, rootfs string)
	}{
		{
			name:    "fifo named directly",
			nominal: "/boot/vmlinuz",
			build: func(t *testing.T, rootfs string) {
				mkFifo(t, filepath.Join(rootfs, "boot", "vmlinuz"))
			},
		},
		{
			name:    "symlink to a fifo inside the rootfs",
			nominal: "/boot/vmlinuz",
			build: func(t *testing.T, rootfs string) {
				mkFifo(t, filepath.Join(rootfs, "boot", "pipe"))
				if err := os.Symlink("pipe", filepath.Join(rootfs, "boot", "vmlinuz")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rootfs := newRootfs(t)
			if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.build(t, rootfs)
			instanceDir := t.TempDir()

			type result struct {
				path string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				p, err := stageImageBootFile(rootfs, tc.nominal, instanceDir, stagedKernelName)
				done <- result{p, err}
			}()

			select {
			case got := <-done:
				if got.err == nil {
					t.Errorf("a FIFO was accepted as a kernel and staged to %q", got.path)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("hull blocked opening an image-planted FIFO; `hull run` would never return")
			}
		})
	}
}

func mkFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}
