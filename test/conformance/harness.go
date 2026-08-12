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

// This file holds the black-box harness for the static-tier conformance suite
// (ADR 0002, issue #58). It locates or builds the real hull binary and
// runs `compose config` against fixture files, capturing stdout, stderr and
// the exit code.
//
// The package deliberately carries NO darwin build tag, so the manifest guards
// (manifest_test.go, schema_sync_test.go) and the coverage guard build and run
// on every host. cmd/hull is //go:build darwin, so on a non-darwin host
// the binary cannot be built: the harness records a skip reason instead of
// failing, and TestConformanceStatic skips cleanly.

package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// projectName is passed as -p so `compose config` prints a deterministic
// "name:" and instance-name prefix regardless of the test's working directory.
const projectName = "conf"

// binaryImportPath is the package `go build` compiles for the black-box binary.
const binaryImportPath = "github.com/brig-sh/hull/cmd/hull"

var (
	// binPath is the resolved hull binary under test, or "" when it
	// could not be located or built (see skipReason).
	binPath string
	// skipReason explains why the static suite cannot run; empty when it can.
	skipReason string
)

// resolveBinary locates or builds the hull binary for the black-box
// suite: honor HULL_BIN if set, otherwise build cmd/hull into a
// fresh temp dir on darwin. TestMain (in conformance_test.go, where the test
// runner recognizes it) calls this once and removes buildDir on teardown.
//
// Skipping is reserved for exactly one condition: a non-darwin host, where the
// darwin-only sources make the binary impossible. Everything else — a broken
// HULL_BIN, a failed build, a missing toolchain — is an error, and
// TestMain turns it into a package failure. A suite that silently skips when
// the SUT does not compile would report green on a broken parser.
func resolveBinary() (bin, skip, buildDir string, err error) {
	if v := os.Getenv("HULL_BIN"); v != "" {
		info, statErr := os.Stat(v)
		if statErr != nil {
			return "", "", "", fmt.Errorf("HULL_BIN=%q: %w", v, statErr)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", "", "", fmt.Errorf("HULL_BIN=%q is not an executable file", v)
		}
		return v, "", "", nil
	}
	if runtime.GOOS != "darwin" {
		return "", fmt.Sprintf(
			"hull is a darwin-only binary (cmd/hull is //go:build darwin); "+
				"it cannot be built or exercised on %s/%s, so the static conformance suite is skipped",
			runtime.GOOS, runtime.GOARCH), "", nil
	}
	dir, mkErr := os.MkdirTemp("", "urunc-conformance")
	if mkErr != nil {
		return "", "", "", fmt.Errorf("could not create a build directory: %w", mkErr)
	}
	out := filepath.Join(dir, "hull")
	build := exec.Command("go", "build", "-o", out, binaryImportPath)
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		_ = os.RemoveAll(dir)
		return "", "", "", fmt.Errorf("go build %s failed (%v):\n%s", binaryImportPath, buildErr, combined)
	}
	return out, "", dir, nil
}

// recordSourceDeps reads every source file of the binary under test so the go
// test cache treats them as inputs. This package imports nothing from
// cmd/hull — the binary is built by shelling out — so without these
// reads the cache would keep serving a stale PASS after the parser under test
// changed.
func recordSourceDeps() error {
	srcs, err := filepath.Glob(filepath.Join("..", "..", "cmd", "hull", "*.go"))
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return fmt.Errorf("no cmd/hull sources found; conformance must run from test/conformance in the repo")
	}
	for _, s := range srcs {
		if _, err := os.ReadFile(s); err != nil {
			return err
		}
	}
	return nil
}

// runResult captures a single black-box invocation.
type runResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runBinary invokes the resolved binary with args and captures its streams and
// exit code. Reaching here without a binary is a harness bug (the parent test
// gates on skipReason before dispatching subtests), so it fails loudly rather
// than skipping — a per-subtest skip would be invisible in a quiet run.
func runBinary(t *testing.T, args ...string) runResult {
	t.Helper()
	return runBinaryEnv(t, nil, args...)
}

// runBinaryEnv is runBinary with extra environment entries, for the
// capabilities an environment variable activates. COMPOSE_PROFILES is always
// cleared first: it selects services in every compose invocation, and a
// developer who exported it in their shell must not change what this suite
// measures. Entries in env are appended, so they win.
func runBinaryEnv(t *testing.T, env []string, args ...string) runResult {
	t.Helper()
	if binPath == "" {
		t.Fatalf("no hull binary available (%s); the parent test should have gated this", skipReason)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "COMPOSE_PROFILES=")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run %s %s: %v", binPath, strings.Join(args, " "), err)
		}
	}
	return runResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: code}
}

// runConfig runs `compose --file <path> -p conf config` black-box. The parent
// -f/-p flags sit on the compose command (urfave resolves them before the
// config subcommand dispatches).
func runConfig(t *testing.T, path string) runResult {
	t.Helper()
	return runBinary(t, "compose", "--file", path, "--project-name", projectName, "config")
}

// fixturePath resolves a hand-written fixture to an absolute path so the
// invocation is independent of the binary's working directory.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("resolve fixture %q: %v", name, err)
	}
	return abs
}

// writeFixture writes generated compose content to a t.TempDir file and returns
// its path. Generated fixtures (the data-driven unsupported-key probes) never
// touch the checked-in fixtures/ tree.
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write generated fixture: %v", err)
	}
	return p
}

// requireExit0 fails the test when the invocation did not exit 0, echoing both
// streams for diagnosis.
func requireExit0(t *testing.T, res runResult) {
	t.Helper()
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}
}

// requireExitNonZero fails when the invocation unexpectedly succeeded.
func requireExitNonZero(t *testing.T, res runResult) {
	t.Helper()
	if res.exitCode == 0 {
		t.Fatalf("expected a non-zero exit, got 0\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
}

// mustContain fails when haystack lacks needle, labelling the assertion.
func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to find %q in:\n%s", what, needle, haystack)
	}
}

// mustNotContain fails when haystack contains needle.
func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: did not expect %q in:\n%s", what, needle, haystack)
	}
}

// requireNoWarnings asserts the invocation emitted no compose warning line at
// all (used by supported-tier cases whose fixtures are warning-free).
func requireNoWarnings(t *testing.T, res runResult) {
	t.Helper()
	mustNotContain(t, res.stderr, "warning: compose:", "supported capability must not warn")
}

// helpListsCommand reports whether the compose --help COMMANDS section lists a
// subcommand named verb. It scans only the indented lines of the COMMANDS
// block and matches the leading (comma-separated) command names, so a verb
// appearing inside a usage description is not a false positive.
func helpListsCommand(help, verb string) bool {
	inCommands := false
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "COMMANDS:") {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		// Blank lines separate command categories inside the COMMANDS block
		// and must not end the scan; only the next non-indented header
		// (OPTIONS:, GLOBAL OPTIONS:) does.
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			return false
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// Leading tokens are the command name and its aliases ("name, alias")
		// until the description; a two-space gap separates names from the
		// description, but checking the first token covers every real verb.
		if strings.TrimSuffix(fields[0], ",") == verb {
			return true
		}
	}
	return false
}
