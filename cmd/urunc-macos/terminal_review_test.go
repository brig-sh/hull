// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// --- review finding: /dev/tty traded away the one deterministic case ---------
//
// Sizing from stdin when stdout is redirected is the #52 fix and is right. The
// step beyond it -- opening /dev/tty when *every* stream is redirected -- is
// not: there is no terminal being served in that case, and measuring the
// operator's window makes captured output depend on the window that launched
// it. The same script would wrap at 210 columns on a wide monitor and at 80 in
// CI. A fixed fallback keeps redirected output reproducible, which is what
// docker and kubectl do by sizing from the std streams only.
//
// windowSize therefore consults only the descriptors it is handed.
func TestWindowSizeIgnoresTheControllingTerminal(t *testing.T) {
	// Three redirected streams. The test process may well have a controlling
	// terminal -- that is exactly what must not be consulted.
	a, b, c := newPipe(t), newPipe(t), newPipe(t)

	rows, cols, fd, ok := windowSize(int(a.Fd()), int(b.Fd()), int(c.Fd()))
	if ok {
		t.Errorf("windowSize reported a terminal (fd %d) with every stream redirected; "+
			"it must not fall back to /dev/tty, or captured output stops being reproducible", fd)
	}
	if rows != fallbackRows || cols != fallbackCols {
		t.Errorf("got %dx%d, want the fixed fallback %dx%d", rows, cols, fallbackRows, fallbackCols)
	}
	if fd != -1 {
		t.Errorf("got fd %d, want -1 when no descriptor is a terminal", fd)
	}
}

// The #52 fix itself: a terminal anywhere in the list is measured, and the
// descriptor is reported so the caller can follow resizes on it.
func TestWindowSizeMeasuresTheFirstTerminalAndReportsIt(t *testing.T) {
	master, slave := newPTY(t)
	redirected := newPipe(t)
	want := unix.Winsize{Row: 37, Col: 141}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &want); err != nil {
		t.Fatalf("set pty window size: %v", err)
	}
	wantRows, wantCols := want.Row, want.Col

	// stdout redirected, stdin on the terminal -- the shape #52 described.
	rows, cols, fd, ok := windowSize(int(redirected.Fd()), int(slave.Fd()), int(redirected.Fd()))
	if !ok {
		t.Fatal("windowSize found no terminal, but stdin is one")
	}
	if fd != int(slave.Fd()) {
		t.Errorf("measured fd %d, want the terminal %d", fd, int(slave.Fd()))
	}
	if rows != wantRows || cols != wantCols {
		t.Errorf("got %dx%d, want %dx%d", rows, cols, wantRows, wantCols)
	}
}

// The step we dropped must stay dropped: nothing in the sizing policy may reach
// for the controlling terminal behind the caller's back.
func TestSizingPolicyDoesNotOpenDevTTY(t *testing.T) {
	fset := token.NewFileSet()
	// Parsed rather than grepped: the doc comment explains why /dev/tty is not
	// consulted, and mentioning it there is the point.
	file, err := parser.ParseFile(fset, "terminal_darwin.go", nil, 0)
	if err != nil {
		t.Fatalf("parse terminal_darwin.go: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "/dev/tty") {
			t.Errorf("%s: sizing reaches for /dev/tty; it must use only the descriptors "+
				"it is handed, so redirected output stays reproducible",
				fset.Position(lit.Pos()))
		}
		return true
	})
}

// --- review finding: geometry was measured but then frozen -------------------
//
// The initial size can come from stdout or stderr, but the resize listener used
// to install only when *stdin* was a terminal. So `exec -t web top </dev/null`
// started at the right geometry and then never followed a window resize.
//
// Whether to follow resizes is a question about the descriptor we measured, not
// about stdin: SIGWINCH is delivered to the foreground process group of the
// controlling terminal regardless of which descriptors are open. Raw mode stays
// stdin's business.
func TestResizeFollowsTheMeasuredDescriptorNotStdin(t *testing.T) {
	src, err := os.ReadFile("exec.go")
	if err != nil {
		t.Fatalf("read exec.go: %v", err)
	}
	body := string(src)

	notify := strings.Index(body, "signal.Notify(winch, syscall.SIGWINCH)")
	if notify < 0 {
		t.Fatal("cannot find the SIGWINCH listener in exec.go")
	}
	// The gate immediately preceding the listener decides when resizes are
	// followed. It must test the measured descriptor.
	gate := strings.LastIndex(body[:notify], "if useTTY &&")
	if gate < 0 {
		t.Fatal("cannot find the guard preceding the SIGWINCH listener")
	}
	guard := body[gate:notify]
	if strings.Contains(guard, "stdinIsTTY") {
		t.Error("the SIGWINCH listener is gated on stdinIsTTY, so a session that measured " +
			"from stdout or stderr starts at the right size and then never follows a resize; " +
			"gate it on the measured descriptor instead")
	}
	if !strings.Contains(guard, "sizeFdIsTerminal") {
		t.Error(`the SIGWINCH listener should be gated on sizeFdIsTerminal`)
	}
}
