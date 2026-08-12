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

// Runtime proof for ADR-0007: exit status, one-shot services (jobs) and
// restart policies. These are the claims parsing cannot reach — a job's exit
// code only exists once a guest ran its command, and a restart policy only
// means something once something has died.
//
// Two halves with different fixture needs, so they gate separately:
//
//   - The job half runs the service's command through the guest agent, so it
//     needs an agent-bearing image and gates on URUNC_EXEC_TEST_IMAGE exactly
//     like the ADR-0004 exec cases (exec_runtime_test.go). Without it the
//     subtest skips with the fixture reason and proves nothing — never a
//     silent pass.
//   - The supervision half only needs a VM that stays up and can be killed,
//     so it runs on the plain URUNC_TEST_IMAGE the rest of the runtime tier
//     uses. Gating it on the agent image would leave the restart policy
//     unproven on every host that has no agent-bearing fixture, for no
//     technical reason.
//
// Both halves hang off existing runtime entries rather than new ones: the
// job's completion gate IS service_completed_successfully
// (TestConformanceRuntime/service.depends_on) and the supervisor lives in the
// gateway daemon that 'up' starts (TestConformanceRuntime/cli.up). The
// manifest names both cross-references.

package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// instState is the subset of the persisted instance record these cases assert
// on. The runtime tier reads the store directly (as cli.down already does for
// the project state file) because the exit status and the stopped-by-user
// marker are recorded facts with no CLI surface in 'compose ps'.
type instState struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	ExitCode      *int   `json:"exitCode"`
	StoppedByUser bool   `json:"stoppedByUser"`
}

// readInstance reads one instance record from the test store. A missing or
// half-written file is an error, not a fatal: a restart deletes the record and
// recreates it, so callers poll through the gap.
func readInstance(e runtimeEnv, instance string) (instState, error) {
	data, err := os.ReadFile(filepath.Join(e.store, "instances", instance, "state.json"))
	if err != nil {
		return instState{}, err
	}
	var st instState
	if err := json.Unmarshal(data, &st); err != nil {
		return instState{}, err
	}
	return st, nil
}

// waitInstance polls the instance record until cond holds or timeout elapses,
// returning the last record successfully read.
func waitInstance(e runtimeEnv, instance string, timeout time.Duration, cond func(instState) bool) (instState, bool) {
	var last instState
	ok := pollUntil(timeout, time.Second, func() bool {
		st, err := readInstance(e, instance)
		if err != nil {
			return false
		}
		last = st
		return cond(st)
	})
	return last, ok
}

// -- the job half (service.depends_on) ---------------------------------------

// oneShotCompletionGate proves what x-oneshot exists for: a job's exit code
// decides whether its dependents start. Both directions are asserted, because
// either alone is satisfied by a broken implementation — an "always release"
// bug passes the success half, an "always fail" bug passes the failure half.
func oneShotCompletionGate(t *testing.T, e runtimeEnv) {
	e = execEnv(t, e) // skips unless URUNC_EXEC_TEST_IMAGE names an agent image

	// Success: the job writes into a bind mount before exiting 0, so the host
	// artifact proves the command really ran in the guest rather than the
	// runtime having assumed a status for a VM it merely booted.
	dir := t.TempDir()
	file := writeComposeFile(t, fmt.Sprintf(`services:
  migrate:
    image: %s
    x-oneshot: true
    command: ["/bin/sh", "-c", "echo migrated > /mnt/share/migrate.txt; exit 0"]
    volumes:
      - %s:/mnt/share
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      migrate:
        condition: service_completed_successfully
`, e.image, dir, e.image))
	up := bringUp(t, e, file, "pjob")
	requireExit0(t, up)
	mustContain(t, up.stdout, "running migrate to completion", "up must announce the job")
	mustContain(t, up.stdout, "migrate completed successfully", "a job exiting 0 must be reported as completed")

	// The dependent is released only after the job completed.
	cIdx := strings.Index(up.stdout, "migrate completed successfully")
	aIdx := strings.Index(up.stdout, "Creating app")
	if cIdx < 0 || aIdx < 0 || aIdx < cIdx {
		t.Errorf("service_completed_successfully must create the dependent only after the job completed; stdout:\n%s", up.stdout)
	}

	data, err := os.ReadFile(filepath.Join(dir, "migrate.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "migrated" {
		t.Errorf("the job's command never ran in the guest (err=%v, data=%q); a released gate without it "+
			"would mean the exit status was assumed, not observed", err, string(data))
	}

	// The record keeps the observed code, and the job's VM is stopped once its
	// command returned (ADR-0007 section 2). 'up' stops it after the agent
	// reports, so poll rather than assume the order.
	st, ok := waitInstance(e, "pjob-migrate", 60*time.Second, func(st instState) bool {
		return st.ExitCode != nil && st.Status == "stopped"
	})
	if !ok {
		t.Errorf("a completed job must leave a stopped instance carrying its exit code; record: %+v", st)
	} else if *st.ExitCode != 0 {
		t.Errorf("the completed job's recorded exit code is %d, want 0", *st.ExitCode)
	}
	// Divergence pinned: 'compose ps' shows the bare state word for that same
	// instance, not docker's 'Exited (0)'.
	ps := runCompose(t, e, time.Minute, file, "pjob", "ps")
	requireExit0(t, ps)
	mustContain(t, ps.stdout, "stopped", "compose ps must report the completed job as stopped")
	mustNotContain(t, ps.stdout, "Exited", "compose ps does not render docker's 'Exited (N)' form")

	// ...and the recorded code IS reachable, through the top-level verbs.
	// The manifest says so, so assert it rather than cross-referencing a
	// case that only reads state.json.
	top := execBin(t, time.Minute, "--store-dir", e.store, "ps")
	requireExit0(t, top)
	mustContain(t, top.stdout, "EXIT", "top-level ps must carry an EXIT column")
	insp := execBin(t, time.Minute, "--store-dir", e.store, "inspect", "pjob-migrate")
	requireExit0(t, insp)
	mustContain(t, insp.stdout, `"exitCode": 0`, "inspect must report the recorded exit code")

	// Failure: a non-zero job fails the up with the code AND its output, and
	// the dependent is never created.
	failFile := writeComposeFile(t, fmt.Sprintf(`services:
  migrate:
    image: %s
    x-oneshot: true
    command: ["/bin/sh", "-c", "echo JOB_OUTPUT_MARKER; exit 3"]
  app:
    image: %s
    command: ["/bin/sh", "-c", "sleep 300"]
    depends_on:
      migrate:
        condition: service_completed_successfully
`, e.image, e.image))
	fail := bringUp(t, e, failFile, "pjbf")
	requireExitNonZero(t, fail)
	mustContain(t, fail.stdout+fail.stderr, `one-shot service "migrate" exited 3`,
		"a job's non-zero exit must fail the up naming the exact code")
	mustContain(t, fail.stdout+fail.stderr, "JOB_OUTPUT_MARKER",
		"the failure must carry the job's captured output (its console is the benign init, not the command)")
	mustNotContain(t, fail.stdout, "Creating app",
		"the dependent must not be created when the job it gates on failed")
}

// -- the supervision half (cli.up) -------------------------------------------

// stopStaysStoppedWindow is how long a deliberately stopped service is watched
// for an unwanted resurrection. It must comfortably exceed the supervisor's
// poll interval plus the first backoff step, so a supervisor that ignored the
// StoppedByUser marker would be caught rather than merely be slow: the loop
// polls every 2s and its first retry waits 1s.
const stopStaysStoppedWindow = 20 * time.Second

// restartSupervision proves the two halves of ADR-0007 section 3 that only a
// booted project can show: a service under 'restart: always' comes back after
// its VM dies, and one the operator stopped on purpose does not.
//
// The second half is the load-bearing one. A supervisor that restarts
// everything it finds dead passes the first assertion and silently undoes
// every 'urunc-macos stop' within a poll.
func restartSupervision(t *testing.T, e runtimeEnv) {
	const project = "prst"
	instance := project + "-a"
	file := writeComposeFile(t, fmt.Sprintf(`services:
  a:
    image: %s
    restart: always
    command: ["/bin/sh", "-c", "sleep 300"]
`, e.image))
	requireExit0(t, bringUp(t, e, file, project))

	first, ok := waitInstance(e, instance, e.bootTimeout, func(st instState) bool {
		return st.Status == "running" && st.PID > 0
	})
	if !ok {
		t.Fatalf("service never recorded a running VMM before the kill; record: %+v", first)
	}

	// Kill the VMM out from under the project: this is exactly the
	// disappearance the supervisor polls for, and nothing else in the stack
	// notices it (the VMM's own exit code is not a status, ADR-0007 context).
	// os.FindProcess + os.Kill rather than syscall.Kill keeps this file
	// buildable everywhere the manifest guards are, like the rest of the
	// package.
	proc, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatalf("could not address the service's VMM (pid %d): %v", first.PID, err)
	}
	if err := proc.Signal(os.Kill); err != nil {
		t.Fatalf("could not kill the service's VMM (pid %d): %v", first.PID, err)
	}

	// Bounded wait, not a fixed sleep: one poll interval, one backoff step and
	// a full boot. A different pid is what makes this a restart rather than a
	// record that was never updated.
	back, ok := waitInstance(e, instance, e.bootTimeout+60*time.Second, func(st instState) bool {
		return st.Status == "running" && st.PID > 0 && st.PID != first.PID
	})
	if !ok {
		t.Fatalf("a killed service with 'restart: always' never came back within %s; record: %+v",
			e.bootTimeout+60*time.Second, back)
	}
	// The CLI must agree with the record: a restart that only rewrote state
	// would be no restart at all.
	if ps, running := waitRunning(t, e, file, project); !running {
		t.Errorf("the restarted service is not reported running by compose ps:\n%s", ps.stdout)
	}

	// Now the invariant: an explicit stop outranks the policy. The service is
	// currently up, supervised, and has a restart history (attempts=1), so the
	// supervisor is at its most eager here.
	stop := execBin(t, 2*time.Minute, "--store-dir", e.store, "stop", instance)
	requireExit0(t, stop)
	stopped, err := readInstance(e, instance)
	if err != nil {
		t.Fatalf("could not read the stopped instance record: %v", err)
	}
	if !stopped.StoppedByUser {
		t.Fatalf("'stop' must record the StoppedByUser marker; without it the supervisor cannot "+
			"tell a deliberate stop from a crash. Record: %+v", stopped)
	}

	deadline := time.Now().Add(stopStaysStoppedWindow)
	for time.Now().Before(deadline) {
		st, err := readInstance(e, instance)
		if err != nil {
			// The record only disappears when something is retiring it to
			// re-run the service, which is the failure this case exists for.
			t.Fatalf("the stopped service's record vanished (%v); the supervisor clears a record "+
				"before re-running the service, so this is a restart in progress", err)
		}
		if st.Status == "running" {
			// Two distinguishable failures, and the diagnosis matters: a
			// record that came back WITHOUT the marker was replaced by a
			// restart the supervisor decided on before 'stop' had recorded
			// the intent (the marker is written only after the VMM exits),
			// while one that kept the marker means the marker was read and
			// ignored.
			t.Fatalf("a service stopped with 'urunc-macos stop' is running again (pid %d, "+
				"stoppedByUser=%t); an explicit stop must outrank every restart policy",
				st.PID, st.StoppedByUser)
		}
		time.Sleep(time.Second)
	}
}
