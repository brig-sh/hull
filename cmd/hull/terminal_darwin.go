//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

func getTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func restoreTerminal(t *syscall.Termios) {
	if t == nil {
		return
	}
	_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(syscall.Stdin),
		uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(t)), 0, 0, 0)
}

// setTermios applies t to stdin.
func setTermios(t *syscall.Termios) {
	if t == nil {
		return
	}
	_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(syscall.Stdin),
		uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(t)), 0, 0, 0)
}

// makeRawTerminal puts fd into raw mode (cfmakeraw semantics) and returns
// the original state for restoreTerminal.
func makeRawTerminal(fd int) (*syscall.Termios, error) {
	orig, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	raw := *orig
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&raw)), 0, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return orig, nil
}

// isTerminal reports whether fd refers to a terminal.
func isTerminal(fd int) bool {
	_, err := getTermios(fd)
	return err == nil
}

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

// The geometry a session gets when no terminal is reachable. Fixed rather than
// guessed, so redirected and captured output is identical on every machine.
const (
	fallbackRows uint16 = 24
	fallbackCols uint16 = 80
)

// claimForeground makes this process's group the terminal's foreground
// group. Callers must have SIGTTOU ignored: issuing TIOCSPGRP from a
// background group raises it otherwise.
func claimForeground() {
	pgrp := syscall.Getpgrp()
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdin),
		uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&pgrp)))
}
