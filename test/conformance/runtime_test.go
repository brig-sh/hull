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

// TestConformanceRuntime is the runtime half of ADR 0002's "every claim is
// executable" invariant (issue #59): the manifest entries whose behavior a
// booted VM proves and parsing cannot — port publishing, depends_on ordering,
// TCP healthcheck gating, volume writes, and the up/ps/logs/down lifecycle.
//
// It boots real VMs, so it is gated OFF by default: unless
// HULL_CONFORMANCE_RUNTIME=1 is set the parent test skips with an explicit
// reason and the package still passes (the default in CI and on any host
// without VZ). When gated on it follows the env contract from
// test/share-test.py:
//
//	HULL_BIN         - the binary under test (resolved by TestMain).
//	HULL_STORE_DIR         - store root; a fresh temp store is used when unset
//	                          so a run never touches ~/.hull.
//	HULL_TEST_IMAGE        - the guest image. MUST be a real urunc image
//	                          built with Bunny (com.urunc.unikernel.*
//	                          annotations), e.g. a NOFireAI/urunc-images or
//	                          harbor.nbfc.io/nubificus build; conformance
//	                          against a generic OCI image proves nothing
//	                          about urunc guest behavior and overstates the
//	                          published percentage.
//	HULL_TEST_BOOT_TIMEOUT - per-boot budget in seconds (default 120); every
//	                          case polls against it and never fixed-sleeps.
//
// Exactly two conditions skip: HULL_TEST_IMAGE unset, and the image
// genuinely reporting no in-guest TCP listener tool (the probe VM booted and
// answered "none"). Everything else — a probe that failed to run, timed out,
// or could not report, including on a shell-less unikernel flavor — is a hard
// failure: cases that seed guest scripts require a shelled image, and a probe
// that cannot answer is indistinguishable from a broken runtime. Skips name
// the capability id, per ADR 0002's consequence that net-new fixture images
// are out of scope for this epic. The guest is driven with the shared-dir
// script-seeding trick share-test.py uses, never an assumed in-guest agent.
//
// TestConformanceRuntimeCoverage is the manifest<->suite guard: it needs no
// binary and no env gate, so it runs and must pass on every host.

package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// runtimeEnvVar gates the whole tier: only "1" runs it.
const runtimeEnvVar = "HULL_CONFORMANCE_RUNTIME"

// doneMarker is echoed by every seeded guest script once its body has run, so
// a case can poll `compose logs` for a deterministic completion sentinel
// instead of sleeping. readyMarker is the same idea for scripts that background
// a listener and then keep the VM alive.
const (
	doneMarker  = "HULL_CONF_DONE"
	readyMarker = "HULL_CONF_READY"
)

// runtimeEnv is the resolved env contract for one runtime run.
type runtimeEnv struct {
	image       string        // HULL_TEST_IMAGE
	store       string        // --store-dir root (temp unless HULL_STORE_DIR set)
	bootTimeout time.Duration // HULL_TEST_BOOT_TIMEOUT, default 120s
}

// runtimeCase is one capability's check against a booted project.
type runtimeCase func(t *testing.T, e runtimeEnv)

// runtimeCases is the id -> case registry. Its keys are the source of truth
// the coverage guard diffs against the manifest, so it is built without a
// binary or env and every runtime-tier entry MUST appear here exactly once.
func runtimeCases() map[string]runtimeCase {
	return map[string]runtimeCase{
		"cli.up":                caseCliUp,
		"cli.ps":                caseCliPs,
		"cli.logs":              caseCliLogs,
		"cli.down":              caseCliDown,
		"service.depends_on":    caseDependsOn,
		"service.ports":         casePorts,
		"service.volumes":       caseVolumes,
		"ext.x-healthcheck-tcp": caseHealthcheckTCP,
		// exec-dependent capabilities (ADR-0004): gated on
		// HULL_EXEC_TEST_IMAGE, an agent-bearing Bunny-built image.
		"cli.exec":            caseComposeExec,
		"cli.top":             caseComposeTop,
		"service.healthcheck": caseHealthcheckExec,
		"service.post_start":  casePostStart,
		"service.pre_stop":    casePreStop,
	}
}

func TestConformanceRuntime(t *testing.T) {
	// Record source deps here too: a developer running with
	// -run 'TestConformanceRuntime' would otherwise filter out the recording
	// test in conformance_test.go and resurrect the stale-cache hole.
	if err := recordSourceDeps(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(runtimeEnvVar) != "1" {
		t.Skipf("runtime tier gated off: set %s=1 on a macOS host with VZ and the "+
			"test/share-test.py env contract (HULL_TEST_IMAGE, optional HULL_STORE_DIR / "+
			"HULL_TEST_BOOT_TIMEOUT) to run", runtimeEnvVar)
	}
	// Gated on but the SUT cannot exist here (non-darwin): skip, don't fail.
	if skipReason != "" {
		t.Skip(skipReason)
	}
	e := loadRuntimeEnv(t)
	m := loadForTest(t)
	cases := runtimeCases()
	for _, c := range m.Capabilities {
		if c.Tier != TierRuntime {
			continue
		}
		fn, ok := cases[c.ID]
		if !ok {
			// The coverage guard reports this authoritatively; fail the subtest
			// too so a run surfaces it by id.
			t.Run(c.ID, func(t *testing.T) {
				t.Fatalf("no conformance case registered for runtime entry %s", c.ID)
			})
			continue
		}
		t.Run(c.ID, func(t *testing.T) { fn(t, e) })
	}
}

// TestConformanceRuntimeCoverage is the manifest<->suite coverage guard for the
// runtime tier: every runtime entry must have a registered case and every
// registered case must map to a runtime entry. It needs no binary and no env
// gate, so it runs (and must pass) on every host, mirroring
// TestConformanceStaticCoverage.
func TestConformanceRuntimeCoverage(t *testing.T) {
	if err := recordSourceDeps(); err != nil {
		t.Fatal(err)
	}
	m := loadForTest(t)
	cases := runtimeCases()

	runtimeIDs := map[string]bool{}
	for _, c := range m.Capabilities {
		if c.Tier == TierRuntime {
			runtimeIDs[c.ID] = true
		}
	}

	var missing []string
	for id := range runtimeIDs {
		if _, ok := cases[id]; !ok {
			missing = append(missing, id)
		}
	}
	var stray []string
	for id := range cases {
		if !runtimeIDs[id] {
			stray = append(stray, id)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("runtime manifest entries with no registered case (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("registered cases with no runtime manifest entry (%d):\n  %s",
			len(stray), strings.Join(stray, "\n  "))
	}
}

// -- env ---------------------------------------------------------------------

// loadRuntimeEnv resolves the share-test.py env contract. A missing
// HULL_TEST_IMAGE is a skip, not a failure: the tier is opt-in and useless
// without a bootable image. The store defaults to a fresh temp dir so a run
// never mutates the developer's real ~/.hull store.
func loadRuntimeEnv(t *testing.T) runtimeEnv {
	t.Helper()
	img := os.Getenv("HULL_TEST_IMAGE")
	if img == "" {
		t.Skip("fixture image unavailable: HULL_TEST_IMAGE is not set; the runtime tier " +
			"needs a Bunny-built urunc guest image (see test/share-test.py)")
	}
	store := os.Getenv("HULL_STORE_DIR")
	if store == "" {
		// The store root must stay short: the gateway binds unix sockets at
		// <store>/compose/<project>.gateway.sock[.qemu] and instances at
		// <store>/instances/<name>/agent.sock, and darwin's sun_path caps
		// socket paths at 104 bytes. The default os.MkdirTemp root
		// (/var/folders/...) is ~75 chars and overflows every project;
		// /tmp/urcNNN... keeps the whole budget under control.
		dir, err := os.MkdirTemp("/tmp", "urc")
		if err != nil {
			t.Fatalf("create temp store: %v", err)
		}
		store = dir
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	// Reserve 16 chars for the project-name segment rather than measuring
	// today's longest registered name, so a future longer project name cannot
	// slip past the guard and hit bind failures again.
	if sock := filepath.Join(store, "compose", strings.Repeat("p", 16)+".gateway.sock.qemu"); len(sock) >= 104 {
		t.Fatalf("store dir %q is too deep: gateway socket paths (up to %d chars) would exceed "+
			"darwin's 104-byte sun_path limit; use a shorter HULL_STORE_DIR", store, len(sock))
	}
	bootTimeout := 120 * time.Second
	if v := os.Getenv("HULL_TEST_BOOT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			bootTimeout = time.Duration(n) * time.Second
		}
	}
	return runtimeEnv{image: img, store: store, bootTimeout: bootTimeout}
}

// -- black-box exec ----------------------------------------------------------

// execRaw runs the binary under test with a hard timeout and captures its
// streams and exit code. It returns errors (timeout, spawn failure) instead of
// failing the test, so callers that must not fail — cleanup, capability probes
// — can use it directly; execBin is the fatal-on-error wrapper for assertions.
func execRaw(timeout time.Duration, args ...string) (runResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return runResult{stdout.String(), stderr.String(), -1},
			fmt.Errorf("timed out after %s: %s", timeout, strings.Join(args, " "))
	}
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			// Keep the captured streams: spawn-adjacent failures are the
			// hardest to debug and the buffers may already hold output.
			return runResult{stdout.String(), stderr.String(), -1},
				fmt.Errorf("run %s: %w", strings.Join(args, " "), err)
		}
	}
	return runResult{stdout.String(), stderr.String(), code}, nil
}

// execBin is execRaw with a fatal on any non-exit error (timeout, spawn
// failure). A non-zero exit is a normal result the caller asserts on.
func execBin(t *testing.T, timeout time.Duration, args ...string) runResult {
	t.Helper()
	if binPath == "" {
		t.Fatalf("no hull binary available (%s); the parent test should have gated this", skipReason)
	}
	res, err := execRaw(timeout, args...)
	if err != nil {
		t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, res.stdout, res.stderr)
	}
	return res
}

// runCompose invokes `--store-dir <store> compose --file <file>
// --project-name <project> <args...>`. Global flags precede the compose
// command; -f/-p sit on compose (urfave resolves them before the subcommand).
func runCompose(t *testing.T, e runtimeEnv, timeout time.Duration, file, project string, args ...string) runResult {
	t.Helper()
	full := append([]string{"--store-dir", e.store, "compose", "--file", file, "--project-name", project}, args...)
	return execBin(t, timeout, full...)
}

// upTimeout budgets a first `up`: the boot budget plus a generous margin for a
// cold image pull that `run` performs on demand.
func (e runtimeEnv) upTimeout() time.Duration { return e.bootTimeout + 5*time.Minute }

// bringUp runs `compose up` and registers an unconditional `compose down` in
// t.Cleanup so a failed or partial run never leaves a project, gateway, or VM
// behind. It returns up's result for the caller to assert.
func bringUp(t *testing.T, e runtimeEnv, file, project string) runResult {
	t.Helper()
	t.Cleanup(func() { composeDownQuiet(e, file, project) })
	return runCompose(t, e, e.upTimeout(), file, project, "up")
}

// composeDownQuiet tears a project down best-effort. It takes no *testing.T so
// it is safe to call from t.Cleanup, and ignores every error: a project that
// already failed to come up may have no state to remove.
func composeDownQuiet(e runtimeEnv, file, project string) {
	_, _ = execRaw(5*time.Minute, "--store-dir", e.store, "compose", "--file", file, "--project-name", project, "down")
}

// -- polling helpers ---------------------------------------------------------

// pollUntil calls cond every interval until it returns true or timeout
// elapses. It never fixed-sleeps a whole boot: it polls so a fast boot returns
// promptly and a slow one still gets the full budget.
func pollUntil(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// waitMarker polls `compose logs <service>` until marker appears on the
// console or the boot budget elapses, returning the last log snapshot seen.
func waitMarker(t *testing.T, e runtimeEnv, file, project, service, marker string) (string, bool) {
	t.Helper()
	var last string
	ok := pollUntil(e.bootTimeout, 2*time.Second, func() bool {
		r := runCompose(t, e, time.Minute, file, project, "logs", service)
		last = r.stdout + r.stderr
		return strings.Contains(last, marker)
	})
	return last, ok
}

// waitRunning polls `compose ps` until the project reports a running instance.
func waitRunning(t *testing.T, e runtimeEnv, file, project string) (runResult, bool) {
	t.Helper()
	var last runResult
	ok := pollUntil(e.bootTimeout, time.Second, func() bool {
		last = runCompose(t, e, time.Minute, file, project, "ps")
		return strings.Contains(last.stdout, "running")
	})
	return last, ok
}

// dialable reports whether 127.0.0.1:port accepts a TCP connection now.
func dialable(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// -- fixture builders --------------------------------------------------------

// writeComposeFile writes generated compose content to a fresh t.TempDir and
// returns its absolute path. Runtime fixtures are always generated; nothing is
// checked in.
func writeComposeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return p
}

// writeGuestScript seeds runtest.sh into dir (mounted into the guest as a
// share), running body then echoing doneMarker and sleeping so the VM stays up
// for `ps`/`logs`. This is the share-test.py trick: no in-guest agent, the
// script's stdout lands on the console (the instance log).
func writeGuestScript(t *testing.T, dir, body string) {
	t.Helper()
	content := "#!/bin/sh\n" + body + "echo " + doneMarker + "\nsleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "runtest.sh"), []byte(content), 0o755); err != nil {
		t.Fatalf("seed guest script: %v", err)
	}
}

// keepaliveComposeSingle is a one-service project whose guest just idles, for
// the lifecycle cases that only need a booted, running instance.
func keepaliveComposeSingle(t *testing.T, e runtimeEnv) string {
	return writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
`, e.image))
}

// twoServiceDepends is app depends_on db (service_started): db starts first,
// so startup order is [db, app] and teardown order is [app, db].
func twoServiceDepends(t *testing.T, e runtimeEnv) string {
	return writeComposeFile(t, fmt.Sprintf(`services:
  db:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      - db
`, e.image, e.image))
}

// -- in-guest TCP tooling probe ----------------------------------------------

// The generic image may lack any way to open a TCP listener from POSIX sh.
// Cases that need one (published-port reachability, healthcheck gating) probe
// once for an available tool and skip with a capability-named reason when none
// exists, rather than failing — net-new fixture images are out of scope
// (ADR 0002 consequences). The result is memoized across cases in a run.
var (
	tcpToolOnce sync.Once
	tcpToolVal  string
	tcpToolErr  error
)

// detectTCPTool returns the first usable in-guest TCP listener tool. Only a
// genuine fixture limitation — the image reports no listener tool — is a
// skip; a probe that failed to run at all is a hard failure, otherwise a
// broken 'run' or a failed pull would silently vacate service.ports and
// ext.x-healthcheck-tcp.
func detectTCPTool(t *testing.T, e runtimeEnv, capability string) string {
	t.Helper()
	tcpToolOnce.Do(func() { tcpToolVal, tcpToolErr = probeTCPTool(e) })
	if tcpToolErr != nil {
		t.Fatalf("%s: TCP tooling probe failed (this is a harness/runtime failure, not a fixture gap): %v", capability, tcpToolErr)
	}
	if tcpToolVal == "none" {
		t.Skipf("fixture image unavailable: %s needs an in-guest TCP listener "+
			"(python3/nc/ncat/socat); none present in HULL_TEST_IMAGE", capability)
	}
	return tcpToolVal
}

// probeTCPTool boots a throwaway instance whose seeded script reports the first
// listener tool it finds (or "none"), reads it back off the console, and tears
// the instance down. It mirrors share-test.py's boot+shared-dir+logs pattern.
func probeTCPTool(e runtimeEnv) (string, error) {
	dir, err := os.MkdirTemp("", "urc-tcpprobe")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := "#!/bin/sh\n" +
		"tool=none\n" +
		"for c in python3 nc ncat socat; do\n" +
		"  if command -v \"$c\" >/dev/null 2>&1; then tool=$c; break; fi\n" +
		"done\n" +
		"echo \"TCPTOOL=$tool\"\n" +
		"echo " + readyMarker + "\n" +
		"sleep 120\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.sh"), []byte(script), 0o755); err != nil {
		return "", err
	}
	// PID-suffixed so concurrent runs sharing a HULL_STORE_DIR cannot
	// clobber each other's probe instance.
	name := fmt.Sprintf("urc-probe-%d", os.Getpid())
	rm := func() {
		_, _ = execRaw(2*time.Minute, "--store-dir", e.store, "stop", name)
		_, _ = execRaw(2*time.Minute, "--store-dir", e.store, "rm", name)
	}
	rm() // clear any leftover from a crashed prior run
	defer rm()
	r, err := execRaw(e.upTimeout(), "--store-dir", e.store, "run", "--detach",
		"--net", "none", "--name", name, "--shared-dir", dir+":/mnt/p",
		e.image, "/bin/sh", "/mnt/p/probe.sh")
	if err != nil {
		return "", fmt.Errorf("probe run failed: %v (%s)", err, strings.TrimSpace(r.stderr))
	}
	// A non-zero exit is a real failure (failed pull, store conflict, run
	// regression), never a fixture gap — surface it instead of letting the
	// log poll below time out into a misleading "no tooling" answer.
	if r.exitCode != 0 {
		return "", fmt.Errorf("probe run exited %d:\n%s%s", r.exitCode, r.stdout, r.stderr)
	}
	deadline := time.Now().Add(e.bootTimeout)
	for {
		r, _ := execRaw(time.Minute, "--store-dir", e.store, "logs", name)
		out := r.stdout + r.stderr
		if i := strings.Index(out, "TCPTOOL="); i >= 0 {
			if fields := strings.Fields(out[i+len("TCPTOOL="):]); len(fields) > 0 {
				return fields[0], nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("probe VM did not report TCP tooling within %s", e.bootTimeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// listenerCmd returns a backgrounded sh line that opens a persistent TCP
// listener on port using tool, discarding all connection I/O.
func listenerCmd(tool string, port int) string {
	switch tool {
	case "python3":
		return fmt.Sprintf("python3 -c 'import socket\n"+
			"s=socket.socket()\n"+
			"s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)\n"+
			"s.bind((\"0.0.0.0\",%d))\n"+
			"s.listen(5)\n"+
			"while True:\n"+
			" c,_=s.accept()\n"+
			" c.close()' &\n", port)
	case "ncat":
		return fmt.Sprintf("ncat -k -l %d >/dev/null 2>&1 &\n", port)
	case "socat":
		return fmt.Sprintf("socat TCP-LISTEN:%d,reuseaddr,fork /dev/null >/dev/null 2>&1 &\n", port)
	case "nc":
		// GNU wants -p, BSD does not; try both in a loop so the listener
		// survives each accepted connection.
		return fmt.Sprintf("(while true; do nc -l -p %d </dev/null >/dev/null 2>&1 || "+
			"nc -l %d </dev/null >/dev/null 2>&1; done) &\n", port, port)
	default:
		return ""
	}
}

// -- lifecycle cases ---------------------------------------------------------

func caseCliUp(t *testing.T, e runtimeEnv) {
	file := keepaliveComposeSingle(t, e)
	up := bringUp(t, e, file, "pup")
	requireExit0(t, up)
	// up narrates per service and prints no project-wide summary line: it
	// creates the instance, then reports it up at its address.
	mustContain(t, up.stdout, "Creating a (pup-a)", "up must announce the service it creates")
	mustContain(t, up.stdout, "a is up at ", "up must report the service up at its subnet address")

	if _, ok := waitRunning(t, e, file, "pup"); !ok {
		t.Errorf("the project's instance never reached the running state after up")
	}

	// Re-running up on an already-up project errors instead of reconciling.
	re := runCompose(t, e, e.upTimeout(), file, "pup", "up")
	requireExitNonZero(t, re)
	mustContain(t, re.stdout+re.stderr, "already up",
		"re-running up on an up project must error (no reconcile)")

	// The gateway 'up' starts is also the project supervisor (ADR-0007
	// section 3), so what up leaves behind keeps acting on restart policies.
	// It is a nested subtest with its own project so its boots, kills and
	// waits cannot disturb the assertions above.
	t.Run("restart-supervision", func(t *testing.T) { restartSupervision(t, e) })
}

func caseCliPs(t *testing.T, e runtimeEnv) {
	file := keepaliveComposeSingle(t, e)
	requireExit0(t, bringUp(t, e, file, "pps"))

	ps, ok := waitRunning(t, e, file, "pps")
	if !ok {
		t.Fatalf("service never reported running in ps:\n%s", ps.stdout)
	}
	for _, col := range []string{"SERVICE", "INSTANCE", "STATUS", "IP"} {
		mustContain(t, ps.stdout, col, "ps must print the "+col+" column header")
	}
	// project-scoped, prefixed instance naming: <project>-<service>.
	mustContain(t, ps.stdout, "pps-a", "ps must list the project's own instance")
	mustContain(t, ps.stdout, "running", "ps must show the running status")
}

func caseCliLogs(t *testing.T, e runtimeEnv) {
	const line = "HULL_CONF_LOGLINE"
	dir := t.TempDir()
	writeGuestScript(t, dir, "echo "+line+"\n")
	file := writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
`, e.image, dir))
	requireExit0(t, bringUp(t, e, file, "plg"))

	// Single-service retrieval.
	log, ok := waitMarker(t, e, file, "plg", "a", line)
	if !ok {
		t.Fatalf("compose logs did not surface the service's log line %q; got:\n%s", line, log)
	}
	// No-argument form prefixes each line with the service name.
	all := runCompose(t, e, time.Minute, file, "plg", "logs")
	requireExit0(t, all)
	mustContain(t, all.stdout, "a | ", "logs with no service must prefix each line with the service name")
}

func caseCliDown(t *testing.T, e runtimeEnv) {
	file := twoServiceDepends(t, e)
	requireExit0(t, bringUp(t, e, file, "pdn"))
	if _, ok := waitRunning(t, e, file, "pdn"); !ok {
		t.Fatalf("project never reached running before down")
	}

	down := runCompose(t, e, e.bootTimeout+3*time.Minute, file, "pdn", "down")
	requireExit0(t, down)
	mustContain(t, down.stdout, "is down", "down must report the project down")

	// Reverse-order teardown: app (dependent) is stopped before db (dependency).
	ai := strings.Index(down.stdout, "Stopping app")
	di := strings.Index(down.stdout, "Stopping db")
	if ai < 0 || di < 0 || ai > di {
		t.Errorf("down must tear down in reverse startup order (app before db); stdout:\n%s", down.stdout)
	}

	// State file and gateway control socket are gone.
	statePath := filepath.Join(e.store, "compose", "pdn.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("down must remove the project state file %s (stat err: %v)", statePath, err)
	}
	gwSock := filepath.Join(e.store, "compose", "pdn.gateway.sock")
	if _, err := os.Stat(gwSock); !os.IsNotExist(err) {
		t.Errorf("down must remove the gateway control socket %s (stat err: %v)", gwSock, err)
	}
}

// -- service behavior cases --------------------------------------------------

func caseDependsOn(t *testing.T, e runtimeEnv) {
	file := twoServiceDepends(t, e)
	up := bringUp(t, e, file, "pdep")
	requireExit0(t, up)
	// service_started ordering: the dependency is created before the dependent.
	di := strings.Index(up.stdout, "Creating db")
	ai := strings.Index(up.stdout, "Creating app")
	if di < 0 || ai < 0 || di > ai {
		t.Errorf("service_started depends_on must create the dependency (db) before the dependent (app); stdout:\n%s", up.stdout)
	}

	// The completion condition needs a job, and a job needs an agent-bearing
	// image, so it is a nested subtest: gating the whole case on
	// HULL_EXEC_TEST_IMAGE would vacate the service_started ordering proof
	// above on every host without one.
	t.Run("service_completed_successfully", func(t *testing.T) { oneShotCompletionGate(t, e) })
}

// pickFreePort reserves an ephemeral 127.0.0.1 port and releases it for the
// binary under test. Dynamic ports keep an unrelated host listener from
// satisfying the reachability assertion (a fixed 18080 could be anything).
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func casePorts(t *testing.T, e runtimeEnv) {
	tool := detectTCPTool(t, e, "service.ports")
	pub := pickFreePort(t)   // published in the compose file
	unpub := pickFreePort(t) // listened on in-guest but NOT published
	if dialable(pub) || dialable(unpub) {
		t.Fatalf("ports %d/%d are already in use on the host; cannot test forwarding", pub, unpub)
	}
	dir := t.TempDir()
	writeGuestScript(t, dir, listenerCmd(tool, pub)+listenerCmd(tool, unpub)+"echo "+readyMarker+"\n")
	file := writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    ports:
      - "%d:%d"
`, e.image, dir, pub, pub))
	requireExit0(t, bringUp(t, e, file, "pport"))

	if log, ok := waitMarker(t, e, file, "pport", "a", readyMarker); !ok {
		t.Fatalf("guest never started its listeners; console:\n%s", log)
	}
	// The published port becomes reachable on the default 127.0.0.1 bind.
	if !pollUntil(e.bootTimeout, time.Second, func() bool { return dialable(pub) }) {
		t.Fatalf("published port %d is not reachable on 127.0.0.1 (service.ports)", pub)
	}
	// The unpublished port, though bound in the guest, is not forwarded. This
	// negative only means something once the positive proved the listeners
	// script ran and forwarding works — hence Fatal above, not Error.
	if dialable(unpub) {
		t.Errorf("unpublished port %d is reachable on 127.0.0.1; only published ports must be forwarded (service.ports)", unpub)
	}
}

func caseVolumes(t *testing.T, e runtimeEnv) {
	// Read-write bind: contents visible in the guest, guest writes reach host.
	runVolumeProbe(t, e, "pvol", "")

	// ":ro" divergence: read-only is NOT enforced. The mount still warns, and
	// a guest write still reaches the host path (the documented, security-
	// relevant divergence).
	_, up := runVolumeProbe(t, e, "pvro", "ro")
	mustContain(t, up.stderr, "read-only is not enforced",
		":ro must warn that read-only is not enforced (documented divergence)")

	// Named volumes: the ADR-0006 lifecycle, proven with real boots.
	caseNamedVolumes(t, e)
}

// runVolumeProbe boots a one-service project that reads a host-seeded file
// through the bind mount and writes a file back, asserting both directions. It
// returns the host mount dir and up's result (for the :ro warning check).
func runVolumeProbe(t *testing.T, e runtimeEnv, project, mode string) (string, runResult) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("hello-from-host"), 0o644); err != nil {
		t.Fatalf("seed host file: %v", err)
	}
	writeGuestScript(t, dir,
		`echo "SEED=[$(cat /mnt/share/seed.txt 2>/dev/null)]"`+"\n"+
			`echo guest-data > /mnt/share/from-guest.txt && echo GUEST_WRITE=ok || echo GUEST_WRITE=fail`+"\n")
	vol := dir + ":/mnt/share"
	if mode != "" {
		vol += ":" + mode
	}
	file := writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s
`, e.image, vol))
	up := bringUp(t, e, file, project)
	requireExit0(t, up)

	log, ok := waitMarker(t, e, file, project, "a", doneMarker)
	if !ok {
		t.Fatalf("guest script did not finish; console:\n%s", log)
	}
	mustContain(t, log, "SEED=[hello-from-host]", "bind-mount contents must be visible in the guest")
	mustContain(t, log, "GUEST_WRITE=ok", "the guest must be able to write into the bind mount")

	// The guest-written file must appear on the host (write reaches host).
	out := filepath.Join(dir, "from-guest.txt")
	if !pollUntil(15*time.Second, 500*time.Millisecond, func() bool {
		_, err := os.Stat(out)
		return err == nil
	}) {
		t.Errorf("guest-written file did not appear on the host at %s (write did not reach host)", out)
	}
	return dir, up
}

func caseHealthcheckTCP(t *testing.T, e runtimeEnv) {
	// Negative control FIRST, and before the tooling gate: it needs no
	// in-guest TCP tool (the whole point is that nothing listens), so it must
	// run even on images where the listener-dependent positive half skips.
	// Otherwise the manifest's only supported runtime capability would have
	// zero executed assertions on such images.
	deadPort := pickFreePort(t)
	deadDir := t.TempDir()
	writeGuestScript(t, deadDir, "sleep 300\n")
	deadFile := writeComposeFile(t, fmt.Sprintf(`services:
  db:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    x-healthcheck-tcp:
      port: %d
      interval: 1s
      retries: 5
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      db:
        condition: service_healthy
`, e.image, deadDir, deadPort, e.image))
	dead := bringUp(t, e, deadFile, "phcn")
	requireExitNonZero(t, dead)
	mustContain(t, dead.stdout+dead.stderr, "never became healthy",
		"a dependency that never listens must fail the up (proves the probe really probes)")
	mustNotContain(t, dead.stdout, "Creating app",
		"the dependent must not be created when the dependency never becomes healthy")

	// Positive half: needs a real in-guest listener, so it may skip on
	// listener-less images (the negative control above has already run).
	tool := detectTCPTool(t, e, "ext.x-healthcheck-tcp")
	port := pickFreePort(t)
	dir := t.TempDir()
	// db idles briefly, then opens the healthcheck listener: the dependent must
	// not be created until the listener answers.
	writeGuestScript(t, dir, "sleep 3\n"+listenerCmd(tool, port)+"echo "+readyMarker+"\n")
	file := writeComposeFile(t, fmt.Sprintf(`services:
  db:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
    x-healthcheck-tcp:
      port: %d
      interval: 1s
      retries: 60
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      db:
        condition: service_healthy
`, e.image, dir, port, e.image))
	up := bringUp(t, e, file, "phc")
	requireExit0(t, up)

	// The gate is observable: up announces the wait, and the dependent is
	// created only afterwards.
	mustContain(t, up.stdout, "Waiting for db to be healthy",
		"service_healthy must gate the dependent on the db TCP listener")
	wIdx := strings.Index(up.stdout, "Waiting for db to be healthy")
	cIdx := strings.Index(up.stdout, "Creating app")
	if wIdx < 0 || cIdx < 0 || cIdx < wIdx {
		t.Errorf("dependent 'app' must be created only after db becomes healthy; stdout:\n%s", up.stdout)
	}
}

// caseNamedVolumes proves the ADR-0006 lifecycle: guest-written data
// persists across down/up in the managed store directory, and 'down
// --volumes' removes exactly that directory.
func caseNamedVolumes(t *testing.T, e runtimeEnv) {
	dir := t.TempDir()
	writeGuestScript(t, dir,
		`if [ -f /data/persisted.txt ]; then echo "SECOND_RUN_SEES=$(cat /data/persisted.txt)"; fi`+"\n"+
			`echo first > /data/persisted.txt`+"\n"+
			`echo GUEST_WROTE=ok`+"\n")
	file := writeComposeFile(t, fmt.Sprintf(`volumes:
  vdata: {}
services:
  a:
    image: %s
    command: ["/bin/sh", "/mnt/share/runtest.sh"]
    volumes:
      - %s:/mnt/share
      - vdata:/data
`, e.image, dir))

	requireExit0(t, bringUp(t, e, file, "pnv"))
	log, ok := waitMarker(t, e, file, "pnv", "a", doneMarker)
	if !ok {
		t.Fatalf("guest script did not finish; console:\n%s", log)
	}
	mustContain(t, log, "GUEST_WROTE=ok", "the guest must write into the named volume")

	volDir := filepath.Join(e.store, "volumes", "pnv_vdata")
	if !pollUntil(15*time.Second, 500*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(volDir, "persisted.txt"))
		return err == nil && strings.TrimSpace(string(data)) == "first"
	}) {
		t.Fatalf("guest write never appeared in the managed volume dir %s", volDir)
	}

	// Plain down keeps the data; the second up must see it.
	down := execBin(t, e.upTimeout(), "--store-dir", e.store, "compose", "-f", file, "-p", "pnv", "down")
	requireExit0(t, down)
	if _, err := os.Stat(filepath.Join(volDir, "persisted.txt")); err != nil {
		t.Fatalf("plain down must keep the named volume: %v", err)
	}
	requireExit0(t, bringUp(t, e, file, "pnv"))
	log2, ok2 := waitMarker(t, e, file, "pnv", "a", doneMarker)
	if !ok2 {
		t.Fatalf("second run did not finish; console:\n%s", log2)
	}
	mustContain(t, log2, "SECOND_RUN_SEES=first", "data must survive down/up")

	// down --volumes removes exactly the managed directory.
	downV := execBin(t, e.upTimeout(), "--store-dir", e.store, "compose", "-f", file, "-p", "pnv", "down", "--volumes")
	requireExit0(t, downV)
	mustContain(t, downV.stdout, "Removed volume pnv_vdata", "down --volumes must announce the removal")
	if _, err := os.Stat(volDir); !os.IsNotExist(err) {
		t.Fatalf("down --volumes must remove %s (stat err: %v)", volDir, err)
	}
}
