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
	"os/signal"
	"syscall"
	"unsafe"
)

// terminalFdFrom returns the first of fds that is a terminal.
func terminalFdFrom(fds ...int) (int, bool) {
	for _, fd := range fds {
		if isTerminal(fd) {
			return fd, true
		}
	}
	return -1, false
}

// windowSize measures the window size for a guest pty from the first of fds
// that is a terminal, and reports which descriptor that was so the caller can
// follow resizes on the same one. Callers pass stdout, stdin, stderr in that
// order: preferring stdout leaves the size unchanged for sessions whose output
// is the terminal, and the rest of the chain covers those that redirect it.
//
// Only the descriptors handed in are consulted. Reaching for /dev/tty when they
// are all redirected would size a session from the operator's window even
// though no terminal is being served, making captured output depend on whatever
// window happened to launch it — the same script wrapping at 210 columns on a
// wide monitor and at 80 in CI. docker and kubectl size from the std streams
// for the same reason.
//
// ok is false when no descriptor is a terminal. rows and cols are then the fixed
// fallback, and the caller has nothing to follow, so it must not install a
// resize listener.
func windowSize(fds ...int) (rows, cols uint16, fd int, ok bool) {
	if tty, found := terminalFdFrom(fds...); found {
		rows, cols = terminalSize(tty)
		return rows, cols, tty, true
	}
	return fallbackRows, fallbackCols, -1, false
}

// terminalSize returns the window size of fd, or 24x80 when unavailable.
func terminalSize(fd int) (rows, cols uint16) {
	var ws struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)), 0, 0, 0)
	if errno != 0 || ws.Row == 0 || ws.Col == 0 {
		return fallbackRows, fallbackCols
	}
	return ws.Row, ws.Col
}

// catchWindowSize starts catching SIGWINCH for a session whose terminal size
// is measured from fd. follow re-measures fd on each SIGWINCH caught, those
// caught before follow was called among them, and hands the size to resize.
// stop stops catching SIGWINCH, and ends follow's loop.
//
// Catching and following are apart because a session can have a size to
// follow before it has anywhere to send one: a resize while it is still being
// set up is caught, and reaches it once follow is called.
func catchWindowSize(fd int) (follow func(resize func(rows, cols uint16)), stop func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	follow = func(resize func(rows, cols uint16)) {
		go func() {
			for range winch {
				resize(terminalSize(fd))
			}
		}()
	}
	return follow, func() {
		// Once Stop returns no SIGWINCH is sent on winch, so closing it is
		// safe, and it ends the loop, which would otherwise wait for good.
		signal.Stop(winch)
		close(winch)
	}
}

// The geometry a session gets when no terminal is reachable. Fixed rather than
// guessed, so redirected and captured output is identical on every machine.
const (
	fallbackRows uint16 = 24
	fallbackCols uint16 = 80
)
