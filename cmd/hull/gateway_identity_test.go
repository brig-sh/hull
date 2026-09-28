//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// startArgvProcess re-execs this test binary from path with exactly argv as
// its kernel argv, blocked in TestMain's sleep mode, and returns its pid.
// gatewayProcessMatches reads argv from the kernel, so the fake has to carry
// the real tokens, not a command line packed into argv[0].
func startArgvProcess(t *testing.T, path string, argv ...string) int {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Args = argv
	cmd.Env = append(os.Environ(), SleepChildEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid
}

// startFakeGateway launches a process shaped like the network-gateway daemon,
// run from exe, and returns its pid. argv[0] is exe, exactly as
// startGatewayDaemon re-execs os.Executable, and extra tokens are appended so a
// caller can plant a decoy mention.
func startFakeGateway(t *testing.T, exe, sock string, extra ...string) int {
	t.Helper()
	argv := append([]string{exe, "network-gateway", "--socket", sock}, extra...)
	return startArgvProcess(t, exe, argv...)
}

func testExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// A genuine gateway for a socket path must match. This is the positive control:
// it fails if the matcher is mutated to always return false, or if the argv[0],
// subcommand, or --socket checks are made stricter than the real spawn shape.
//
// Regression: gatewayProcessMatches was at 0% coverage; nothing proved it ever
// returned true for a real daemon.
func TestGatewayProcessMatchesRealGateway(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proj.gateway.sock")
	pid := startFakeGateway(t, testExecutable(t), sock)

	if !gatewayProcessMatches(pid, sock) {
		t.Fatalf("gateway for %s did not match its own pid %d", sock, pid)
	}
}

// An unrelated process whose command line merely mentions "network-gateway" and
// the socket path must not match. This is the assertion that fails under the old
// substring matcher: `tail -f /var/log/network-gateway.log <sock>` contains both
// strings, so strings.Contains accepted it, and a stale record pointing at that
// pid would have had it SIGTERMed -- the vz-runner incident again.
//
// Regression / mutation: reverting the argv[0] base-name check (or the whole
// function to strings.Contains) flips the false assertion below to true. The
// paired real gateway keeps the test honest -- it fails if the body is reduced
// to `return false`.
func TestGatewayProcessMatchesIgnoresSubstringMention(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proj.gateway.sock")

	// argv[0] base is "tail", not this binary, yet the line mentions both the
	// gateway subcommand string and the exact socket path.
	decoyPID := startArgvProcess(t, testExecutable(t), "tail", "-f", "/var/log/network-gateway.log", sock)
	if gatewayProcessMatches(decoyPID, sock) {
		t.Fatalf("unrelated process %d matched merely by mentioning the strings", decoyPID)
	}

	realPID := startFakeGateway(t, testExecutable(t), sock)
	if !gatewayProcessMatches(realPID, sock) {
		t.Fatalf("real gateway %d for %s did not match", realPID, sock)
	}
}

// A gateway that owns one store's socket must not match another store's socket
// path when that path is a substring of its own. Store roots are user-chosen, so
// a store at /a/b and one at /x/a/b give socket paths where the first is a
// suffix, hence a substring, of the second.
//
// Regression / mutation: comparing sockPath with strings.Contains instead of an
// exact --socket value flips the false assertion to true, because pathA is a
// substring of pathB. The true assertion fails if the body becomes `return
// false`.
func TestGatewayProcessMatchesRejectsNestedSocketPath(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "store", "compose", "proj.gateway.sock")
	// pathB is pathA under another root: pathA is a substring of pathB.
	pathB := filepath.Join(dir, "outer") + pathA

	pidB := startFakeGateway(t, testExecutable(t), pathB)

	if gatewayProcessMatches(pidB, pathA) {
		t.Fatalf("gateway for %s matched project A's nested path %s", pathB, pathA)
	}
	if !gatewayProcessMatches(pidB, pathB) {
		t.Fatalf("gateway for %s did not match its own path", pathB)
	}
}

// A pid that no longer exists must return false and must not panic. The recorded
// SwitchPID can be stale after a crash and reuse, so the matcher is asked about
// pids whose `ps -p` lookup errors out.
//
// Regression / mutation: changing the `if err != nil { return false }` branch to
// return true (or dropping it so the code indexes empty fields) makes this fail
// or panic. The paired live gateway fails a `return false` body.
func TestGatewayProcessMatchesMissingPid(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proj.gateway.sock")

	// A pid we start and let the helper reap is gone by the time we assert, and a
	// very large pid is almost certainly unused; either way `ps -p` errors.
	const absentPID = 2147483646
	if gatewayProcessMatches(absentPID, sock) {
		t.Fatalf("matcher reported a nonexistent pid %d as the gateway", absentPID)
	}

	livePID := startFakeGateway(t, testExecutable(t), sock)
	if !gatewayProcessMatches(livePID, sock) {
		t.Fatalf("live gateway %d for %s did not match", livePID, sock)
	}
}

// A store dir with a space in it must not hide the real gateway. `ps -o
// command=` joins argv with spaces, so the old field split cut the --socket
// value at "My", the match failed, and stopGatewayDaemon removed the socket
// but left the daemon running.
//
// Regression / mutation: going back to splitting `ps` output on whitespace
// fails this test.
func TestGatewayProcessMatchesSpaceInStorePath(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "My Store", "compose", "proj.gateway.sock")
	pid := startFakeGateway(t, testExecutable(t), sock)

	if !gatewayProcessMatches(pid, sock) {
		t.Fatalf("gateway with a space in its socket path %q did not match", sock)
	}
}

// A hull binary under a directory with a space in it must still match: argv[0]
// is the full path, and splitting it on whitespace loses its base name.
//
// Regression / mutation: going back to splitting `ps` output on whitespace
// fails this test.
func TestGatewayProcessMatchesSpaceInBinaryPath(t *testing.T) {
	exe := testExecutable(t)
	binDir := filepath.Join(t.TempDir(), "My Bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, filepath.Base(exe))
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "proj.gateway.sock")
	pid := startFakeGateway(t, link, sock)

	if !gatewayProcessMatches(pid, sock) {
		t.Fatalf("gateway run from %q did not match", link)
	}
}
