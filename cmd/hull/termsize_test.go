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
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// newPipe returns the write end of a pipe, standing in for a redirected
// stdout: a perfectly good file descriptor that is not a terminal.
func newPipe(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return w
}

func TestTerminalFdFromSkipsNonTerminals(t *testing.T) {
	_, slave := newPTY(t)
	pipe := newPipe(t)

	fd, ok := terminalFdFrom(int(pipe.Fd()), int(slave.Fd()))
	if !ok {
		t.Fatal("terminalFdFrom found no terminal, want the pty")
	}
	if fd != int(slave.Fd()) {
		t.Errorf("terminalFdFrom = %d, want the pty fd %d", fd, slave.Fd())
	}
}

func TestTerminalFdFromPrefersTheEarlierTerminal(t *testing.T) {
	_, first := newPTY(t)
	_, second := newPTY(t)

	fd, ok := terminalFdFrom(int(first.Fd()), int(second.Fd()))
	if !ok || fd != int(first.Fd()) {
		t.Errorf("terminalFdFrom = %d, %v; want the first pty fd %d, true", fd, ok, first.Fd())
	}
}

func TestTerminalFdFromReportsNoTerminal(t *testing.T) {
	pipe := newPipe(t)

	if fd, ok := terminalFdFrom(int(pipe.Fd())); ok {
		t.Errorf("terminalFdFrom = %d, true; want no terminal for a pipe", fd)
	}
}

// The regression from issue #52: stdin is the terminal and stdout is
// redirected, so the size must come from stdin rather than the 24x80 fallback.
func TestWindowSizeComesFromTerminalWhenStdoutIsRedirected(t *testing.T) {
	master, slave := newPTY(t)
	want := unix.Winsize{Row: 43, Col: 137}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &want); err != nil {
		t.Fatalf("set pty window size: %v", err)
	}
	stdout := newPipe(t)

	fd, ok := terminalFdFrom(int(stdout.Fd()), int(slave.Fd()))
	if !ok {
		t.Fatal("terminalFdFrom found no terminal, want the pty on stdin")
	}
	if rows, cols := terminalSize(fd); rows != want.Row || cols != want.Col {
		t.Errorf("terminalSize = %dx%d, want %dx%d", rows, cols, want.Row, want.Col)
	}
}

func TestTerminalSizeFallsBackWithoutATerminal(t *testing.T) {
	pipe := newPipe(t)

	if rows, cols := terminalSize(int(pipe.Fd())); rows != 24 || cols != 80 {
		t.Errorf("terminalSize on a pipe = %dx%d, want the 24x80 fallback", rows, cols)
	}
}
