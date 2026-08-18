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

// One-shot compose services.
//
// A one-shot service is a job: it runs a command to completion and its exit
// status is the thing dependents care about. The runtime has no other way to
// learn that status — the VMM's exit code is not a proxy (QEMU and Cloud
// Hypervisor exit 0 whatever the guest did) and urunit consumes the app's
// code — so a job boots with a benign long-lived init and runs its command
// through the guest agent, whose Exit frame carries the exact code. That
// makes an agent-bearing image a hard requirement, and its absence a loud
// failure rather than an assumed success.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urunc-dev/urunc/pkg/agentproto"

	"github.com/brig-sh/hull/internal/compose"
	"github.com/brig-sh/hull/pkg/store"
)

// The transport sentinel errAgentTransport is shared with the exec layer
// (exec_compose.go). The capture helpers stay separate on purpose: jobs may
// only retry pre-Open failures (see errAgentDial), while the exec layer's
// pre-warm no-ops and hooks are idempotent and retry any transport error.

// errAgentDial is the subset of transport failures that happen BEFORE the
// Open frame is written, so the guest command provably never started and a
// retry cannot run it twice. Everything after Open is terminal: a lost
// connection may mean the command is running right now, and re-executing a
// migration is worse than failing the up.
var errAgentDial = fmt.Errorf("%w: cannot reach the guest agent", errAgentTransport)

// agentExecCapture runs argv in a running instance through the guest agent,
// non-interactively, and returns the exit code with the combined output.
func agentExecCapture(s *store.Store, instanceID string, argv, env []string, timeout time.Duration) (int, string, error) {
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return 0, "", fmt.Errorf("instance not found: %s", instanceID)
	}
	if state.Status != "running" {
		return 0, "", fmt.Errorf("instance %s is not running (status: %s)", instanceID, state.Status)
	}
	sockPath := s.InstanceAgentSocket(instanceID)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return 0, "", fmt.Errorf("%w at %s: %v", errAgentDial, sockPath, err)
	}
	defer func() { _ = conn.Close() }()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	// A job's command must see what it would see as a normal service: the
	// image's env and WORKDIR, with the compose environment layered on top.
	imgEnv, imgCwd := bundleProcess(state.BundleDir)
	const stream = 1
	req := agentproto.OpenRequest{
		Argv: argv,
		Env:  append(append([]string{}, imgEnv...), env...),
		Cwd:  imgCwd,
		User: bundleUser(state.BundleDir),
	}
	if err := agentproto.WriteJSON(conn, agentproto.TypeOpen, stream, req); err != nil {
		// Still pre-Open from the guest's perspective: the frame never
		// landed, so nothing started and a retry is safe.
		return 0, "", fmt.Errorf("%w: send open request: %v", errAgentDial, err)
	}
	if err := agentproto.WriteFrame(conn, agentproto.TypeCloseStdin, stream, nil); err != nil {
		return 0, "", fmt.Errorf("%w: close stdin: %v", errAgentTransport, err)
	}

	var out bytes.Buffer
	for {
		f, rerr := agentproto.ReadFrame(conn)
		if rerr != nil {
			return 0, out.String(), fmt.Errorf("%w: connection to the guest agent lost: %v", errAgentTransport, rerr)
		}
		switch f.Type {
		case agentproto.TypeStdout, agentproto.TypeStderr:
			// Bounded for the same reason as execCapture: the buffer holds
			// whatever the guest sends, and a deadline is not a size limit.
			if out.Len() >= maxCaptureBytes {
				return 0, out.String(), fmt.Errorf(
					"%w: the guest sent more than %d bytes in reply to a one-shot command",
					errAgentTransport, maxCaptureBytes)
			}
			out.Write(f.Payload)
		case agentproto.TypeExit:
			var ex agentproto.Exit
			if err := json.Unmarshal(f.Payload, &ex); err != nil {
				// A garbled exit frame must never read as success.
				return 0, out.String(), fmt.Errorf("%w: malformed exit frame: %v", errAgentTransport, err)
			}
			return ex.Code, out.String(), nil
		case agentproto.TypeError:
			var ae agentproto.Error
			_ = json.Unmarshal(f.Payload, &ae)
			return 0, out.String(), fmt.Errorf("guest agent: %s", ae.Message)
		}
	}
}

// agentExecRetry is agentExecCapture with only PRE-Open failures retried,
// until dialDeadline. A session that reached Open is never retried: the
// command may have started, and re-running a job is worse than failing.
// perTry bounds one attempt; commandBudget bounds the session that starts.
func agentExecRetry(s *store.Store, instanceID string, argv, env []string, dialWindow, commandBudget time.Duration) (int, string, error) {
	deadline := time.Now().Add(dialWindow)
	for {
		code, out, err := agentExecCapture(s, instanceID, argv, env, commandBudget)
		if err == nil || !errors.Is(err, errAgentDial) || time.Now().After(deadline) {
			return code, out, err
		}
		time.Sleep(time.Second)
	}
}

// oneshotInitCommand is what a job's VM runs as its process: something that
// stays alive and does nothing, so the agent has a live guest to exec in.
// The real work is the service's command, run through the agent.
//
// Every element MUST be whitespace-free. Guest argv travels through the
// kernel command line (run.go joins it with spaces at all three rootfs
// paths) and the kernel re-splits it on unquoted whitespace, so an argv
// element containing a space arrives as two — which would kill the init and
// with it every job.
var oneshotInitCommand = []string{"/bin/sleep", "86400"}

// isOneShot reports whether a service runs as a job: either it says so, or
// some other service gates on its completion.
func isOneShot(p *types.Project, name string) bool {
	if svc, ok := p.Services[name]; ok && compose.OneShot(svc) {
		return true
	}
	for _, other := range p.Services {
		for depName, dep := range other.DependsOn {
			if depName == name && dep.Condition == "service_completed_successfully" {
				return true
			}
		}
	}
	return false
}

// jobResult is a completed one-shot run.
type jobResult struct {
	code   int
	output string
}

// runOneShot executes the job's command through the guest agent, records the
// exit status on the instance, and stops the VM. The agent is proven
// reachable first with a retried no-op session; the command itself then runs
// exactly once — a transport failure after Open is never retried, because
// the command may have started and re-running a job is worse than failing.
func runOneShot(s *store.Store, instanceID, svcName string, argv, env []string, dialWindow, commandBudget time.Duration) (jobResult, error) {
	if len(argv) == 0 {
		return jobResult{}, fmt.Errorf("service %q: a one-shot service needs a 'command' to run", svcName)
	}
	// Pre-warm, exactly like compose exec: on Vz a successful dial only
	// proves the host-side bridge is up, so during boot a session can
	// connect and EOF before the guest agent has bound its listener — and
	// agentExecCapture must treat everything past a successful dial as
	// terminal. A retried no-op session absorbs that window. Only a
	// transport-class failure blocks: an agent-side error reply (no
	// /bin/true in a minimal rootfs) proves the agent is alive, which is
	// all the pre-warm exists to check.
	if _, _, err := execCaptureRetry(s, instanceID, []string{"/bin/true"}, nil, "", 10*time.Second, dialWindow); err != nil && errors.Is(err, errAgentTransport) {
		return jobResult{}, fmt.Errorf("service %q: the guest agent never became reachable: %w "+
			"(does the image ship an agent-capable init?)", svcName, err)
	}
	code, out, err := agentExecRetry(s, instanceID, argv, env, dialWindow, commandBudget)
	if err != nil {
		return jobResult{output: out}, fmt.Errorf("service %q: cannot run the one-shot command: %w", svcName, err)
	}
	if err := recordExit(s, instanceID, code); err != nil {
		return jobResult{code: code, output: out}, err
	}
	return jobResult{code: code, output: out}, nil
}

// recordExit stores an observed exit status on the instance record.
func recordExit(s *store.Store, instanceID string, code int) error {
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("record exit status: %w", err)
	}
	state.ExitCode = &code
	state.ExitedAt = time.Now()
	if err := s.SaveInstance(state); err != nil {
		return fmt.Errorf("record exit status: %w", err)
	}
	return nil
}

// oneShotFailure renders a job's non-zero exit for the up error, trimming the
// captured output so the message stays readable.
func oneShotFailure(svcName string, res jobResult) error {
	out := strings.TrimSpace(res.output)
	if len(out) > 2000 {
		out = out[:2000] + "\n... (output truncated)"
	}
	if out == "" {
		return fmt.Errorf("one-shot service %q exited %d", svcName, res.code)
	}
	return fmt.Errorf("one-shot service %q exited %d:\n%s", svcName, res.code, out)
}

// bundleProcess reads the image's process environment and working directory
// from the instance's OCI bundle, so a job's command runs in the same
// context a normal service's command would (urunit applies both from
// urunit.conf; the agent needs them passed explicitly).
func bundleProcess(bundleDir string) (env []string, cwd string) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return nil, ""
	}
	var spec struct {
		Process struct {
			Env []string `json:"env"`
			Cwd string   `json:"cwd"`
		} `json:"process"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, ""
	}
	return spec.Process.Env, spec.Process.Cwd
}

// Budgets for a job: a short window to reach a freshly booted agent, and a
// generous ceiling on the command itself. They are separate so an image
// without /urunit-agent fails in seconds rather than after the command
// budget, and a legitimately long job is not killed as a transport failure.
const (
	oneShotDialWindow    = 90 * time.Second
	oneShotCommandBudget = 30 * time.Minute
)

// checkCompletedDeps enforces the invariant: a dependent may start
// only when every service it gates on completed with exit code 0. It fails
// closed — an unknown service, an unreadable record, or an absent status is
// a refusal, never an assumed success.
// A dependency declared 'required: false' never fails its dependent: docker
// only warns when an optional dependency is not started or not available, so
// every refusal below is downgraded to a warning line on warn.
func checkCompletedDeps(s *store.Store, proj *composeProject, svcName string, deps types.DependsOnConfig, warn io.Writer) error {
	fail := func(dep types.ServiceDependency, depName, format string, args ...any) error {
		if !dep.Required {
			composeWarn(warn, "service %q: optional dependency %q: %s", svcName, depName, fmt.Sprintf(format, args...))
			return nil
		}
		return fmt.Errorf(format, args...)
	}
	for _, depName := range sortedDependsOnKeys(deps) {
		dep := deps[depName]
		if dep.Condition != "service_completed_successfully" {
			continue
		}
		instance, ok := proj.Services[depName]
		if !ok {
			if err := fail(dep, depName, "service %q waits for %q to complete, but it never ran", svcName, depName); err != nil {
				return err
			}
			continue
		}
		state, err := s.GetInstance(instance)
		if err != nil {
			if ferr := fail(dep, depName, "service %q: cannot read %q's state: %v", svcName, depName, err); ferr != nil {
				return ferr
			}
			continue
		}
		if state.ExitCode == nil {
			if err := fail(dep, depName, "service %q waits for %q to complete, but %q reported no exit status", svcName, depName, depName); err != nil {
				return err
			}
			continue
		}
		if *state.ExitCode != 0 {
			if err := fail(dep, depName, "service %q waits for %q to complete, but it exited %d", svcName, depName, *state.ExitCode); err != nil {
				return err
			}
			continue
		}
	}
	return nil
}
