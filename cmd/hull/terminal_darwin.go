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

// claimForeground makes this process's group the terminal's foreground
// group. Callers must have SIGTTOU ignored: issuing TIOCSPGRP from a
// background group raises it otherwise.
func claimForeground() {
	pgrp := syscall.Getpgrp()
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdin),
		uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&pgrp)))
}
