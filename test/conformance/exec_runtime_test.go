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

// Runtime cases for the exec-dependent capabilities (ADR-0004): compose
// exec, exec-form healthchecks, post_start/pre_stop hooks, and compose top
// all need a guest that ships /urunit-agent. They gate on a SECOND image
// variable, HULL_EXEC_TEST_IMAGE, because the generic HULL_TEST_IMAGE
// fixture is not required to carry the agent; without it every case here
// skips with an explicit fixture reason. The image must be a Bunny-built
// urunc image like the rest of the runtime tier.

package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// execImage returns the agent-bearing guest image, or skips the case.
func execImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("HULL_EXEC_TEST_IMAGE")
	if img == "" {
		t.Skip("fixture image unavailable: HULL_EXEC_TEST_IMAGE is not set; exec-dependent " +
			"cases need a Bunny-built urunc image that ships /urunit-agent")
	}
	return img
}

// execEnv clones the runtime env with the agent-bearing image substituted.
func execEnv(t *testing.T, e runtimeEnv) runtimeEnv {
	t.Helper()
	e.image = execImage(t)
	return e
}

// upSingle brings up a one-service project running body via the seeded
// script and returns the host share dir and the compose file path.
func upSingle(t *testing.T, e runtimeEnv, project, body string, extraYAML string) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	writeGuestScript(t, dir, body+"echo "+readyMarker+"\nsleep 300\n")
	file = writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
%s`, e.image, dir, extraYAML))
	requireExit0(t, bringUp(t, e, file, project))
	if log, ok := waitMarker(t, e, file, project, "a", readyMarker); !ok {
		t.Fatalf("guest never became ready; console:\n%s", log)
	}
	return dir, file
}

func caseComposeExec(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e)
	_, file := upSingle(t, e, "pxe", "", "")

	// Non-interactive exec: output and exit code are the capability's claim.
	res := execBin(t, e.upTimeout(), append(selfArgs(e),
		"compose", "-f", file, "-p", "pxe", "exec", "-T", "a",
		"/bin/sh", "-c", "echo EXEC_MARKER_$((40+2))")...)
	requireExit0(t, res)
	mustContain(t, res.stdout, "EXEC_MARKER_42", "compose exec must run the command in the guest and relay stdout")

	// A failing command's exit code must propagate to the CLI's exit code.
	fail := execBin(t, e.upTimeout(), append(selfArgs(e),
		"compose", "-f", file, "-p", "pxe", "exec", "-T", "a",
		"/bin/sh", "-c", "exit 7")...)
	if fail.exitCode != 7 {
		t.Errorf("compose exec must propagate the guest exit code; got %d, want 7\nstdout:\n%s\nstderr:\n%s",
			fail.exitCode, fail.stdout, fail.stderr)
	}

	// An unknown service is a loud error, not a guest error.
	unknown := execBin(t, time.Minute, append(selfArgs(e),
		"compose", "-f", file, "-p", "pxe", "exec", "-T", "nosuch", "/bin/true")...)
	requireExitNonZero(t, unknown)
	mustContain(t, unknown.stdout+unknown.stderr, `unknown service "nosuch"`,
		"compose exec must reject unknown services by name")
}

func caseHealthcheckExec(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e)
	// db becomes healthy only once its script has created /tmp/healthy a few
	// seconds in; app must not be created before that. The probe is the
	// spec's exec form, so this is the capability's whole claim.
	dir := t.TempDir()
	writeGuestScript(t, dir, "sleep 3\ntouch /tmp/healthy\necho "+readyMarker+"\nsleep 300\n")
	file := writeComposeFile(t, fmt.Sprintf(`services:
  db:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    healthcheck:
      test: ["CMD", "/bin/test", "-f", "/tmp/healthy"]
      interval: 1s
      timeout: 5s
      retries: 30
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      db:
        condition: service_healthy
`, e.image, dir, e.image))
	up := bringUp(t, e, file, "phe")
	requireExit0(t, up)
	mustContain(t, up.stdout, "Waiting for db to be healthy (exec healthcheck)",
		"service_healthy must gate on the exec healthcheck when one is declared")
	wIdx := strings.Index(up.stdout, "Waiting for db to be healthy")
	cIdx := strings.Index(up.stdout, "Creating app")
	if wIdx < 0 || cIdx < 0 || cIdx < wIdx {
		t.Errorf("dependent must be created only after the exec healthcheck passes; stdout:\n%s", up.stdout)
	}

	// Negative control: a probe that can never pass must fail up with the
	// budget exhausted — a no-op waitHealthyExec cannot survive this.
	deadDir := t.TempDir()
	writeGuestScript(t, deadDir, "sleep 300\n")
	deadFile := writeComposeFile(t, fmt.Sprintf(`services:
  db:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    healthcheck:
      test: ["CMD", "/bin/false"]
      interval: 1s
      timeout: 5s
      retries: 3
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      db:
        condition: service_healthy
`, e.image, deadDir, e.image))
	dead := bringUp(t, e, deadFile, "phn")
	requireExitNonZero(t, dead)
	mustContain(t, dead.stdout+dead.stderr, "never became healthy",
		"a probe that always fails must fail the up")
	mustContain(t, dead.stdout+dead.stderr, "healthcheck exited",
		"the failure must carry the probe's exit status, proving a real exec ran")
	mustNotContain(t, dead.stdout, "Creating app",
		"the dependent must not be created when the probe never passes")
}

func casePostStart(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e)
	// The hook writes into the bind mount; the file appearing on the host
	// proves the hook really ran inside the guest, after start.
	dir, _ := upSingle(t, e, "pps", "", `    post_start:
      - command: ["/bin/sh", "-c", "echo from-hook > /mnt/share/hook.txt"]
`)
	hookOut := filepath.Join(dir, "hook.txt")
	if !pollUntil(30*time.Second, 500*time.Millisecond, func() bool {
		data, err := os.ReadFile(hookOut)
		return err == nil && strings.TrimSpace(string(data)) == "from-hook"
	}) {
		t.Fatalf("post_start hook output never appeared on the host at %s", hookOut)
	}

	// Negative control: a failing hook must fail up and tear down.
	dir2 := t.TempDir()
	writeGuestScript(t, dir2, "echo "+readyMarker+"\nsleep 300\n")
	file := writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    post_start:
      - command: ["/bin/false"]
`, e.image, dir2))
	fail := bringUp(t, e, file, "ppf")
	requireExitNonZero(t, fail)
	mustContain(t, fail.stdout+fail.stderr, "post_start[0] exited",
		"a failing post_start hook must fail the up with its exit status")
}

func casePreStop(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e)
	dir, file := upSingle(t, e, "pst", "", `    pre_stop:
      - command: ["/bin/sh", "-c", "echo bye > /mnt/share/prestop.txt"]
`)
	// Down runs the hook before stopping; the artifact must exist afterwards.
	down := execBin(t, e.upTimeout(), append(selfArgs(e),
		"compose", "-f", file, "-p", "pst", "down")...)
	requireExit0(t, down)
	mustContain(t, down.stdout, "pre_stop hook(s)", "down must announce the pre_stop hooks")
	data, err := os.ReadFile(filepath.Join(dir, "prestop.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "bye" {
		t.Errorf("pre_stop hook output missing on the host (err=%v, data=%q)", err, string(data))
	}
}

func caseComposeTop(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e)
	_, file := upSingle(t, e, "ptp", "", "")
	res := execBin(t, e.upTimeout(), append(selfArgs(e),
		"compose", "-f", file, "-p", "ptp", "top")...)
	requireExit0(t, res)
	mustContain(t, res.stdout, "a (ptp-a)", "top must print the service header")
	mustContain(t, res.stdout, "PID", "top must relay the guest's ps output")
}

// selfArgs builds the global flags for a direct binary invocation against
// the test store.
func selfArgs(e runtimeEnv) []string {
	return []string{"--store-dir", e.store}
}
