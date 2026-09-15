//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// startFakeGateway launches a process shaped like the network-gateway daemon and
// returns its pid.
//
// `ps -o command=` prints argv joined by spaces, and gatewayProcessMatches splits
// it back into fields, so the whole daemon command line is encoded into argv[0].
// That lets a single /bin/sleep carry a realistic argv while still blocking:
// sleep only ever sees its own duration argument. argv[0]'s base name is
// os.Executable's base, exactly as startGatewayDaemon re-execs os.Executable, and
// extra tokens are appended so a caller can plant a decoy mention.
func startFakeGateway(t *testing.T, sock string, extra ...string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	line := filepath.Base(exe) + " network-gateway --socket " + sock
	for _, e := range extra {
		line += " " + e
	}
	pid, _ := startProcessAs(t, line, "/bin/sleep", "3600")
	return pid
}

// A genuine gateway for a socket path must match. This is the positive control:
// it fails if the matcher is mutated to always return false, or if the argv[0],
// subcommand, or --socket checks are made stricter than the real spawn shape.
//
// Regression: gatewayProcessMatches was at 0% coverage; nothing proved it ever
// returned true for a real daemon.
func TestGatewayProcessMatchesRealGateway(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proj.gateway.sock")
	pid := startFakeGateway(t, sock)

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
	decoyLine := "tail -f /var/log/network-gateway.log " + sock
	decoyPID, _ := startProcessAs(t, decoyLine, "/bin/sleep", "3600")
	if gatewayProcessMatches(decoyPID, sock) {
		t.Fatalf("unrelated process %d matched merely by mentioning the strings", decoyPID)
	}

	realPID := startFakeGateway(t, sock)
	if !gatewayProcessMatches(realPID, sock) {
		t.Fatalf("real gateway %d for %s did not match", realPID, sock)
	}
}

// A gateway that owns project B's socket must not match project A's socket path
// when A's path is a substring of B's. Socket paths are built from user-supplied
// project names as <root>/compose/<project>.gateway.sock, so a project named
// "proj.gateway.sock.bak" yields a path that literally contains project "proj"'s
// path as a prefix -- the containment below reproduces that by hand.
//
// Regression / mutation: comparing sockPath with strings.Contains instead of an
// exact --socket value flips the false assertion to true, because pathA is a
// substring of pathB. The true assertion fails if the body becomes `return
// false`.
func TestGatewayProcessMatchesRejectsNestedSocketPath(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "proj.gateway.sock")
	// pathB has pathA as a strict prefix: pathA is a substring of pathB.
	pathB := pathA + ".bak"

	pidB := startFakeGateway(t, pathB)

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

	livePID := startFakeGateway(t, sock)
	if !gatewayProcessMatches(livePID, sock) {
		t.Fatalf("live gateway %d for %s did not match", livePID, sock)
	}
}
