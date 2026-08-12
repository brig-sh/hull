// Copyright (c) 2023-2026, Nubificus LTD
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/agentproto"

	"github.com/nofireai/urunc-macos/pkg/store"
)

// healthProbeArgv converts a compose-go healthcheck's Test form into the
// argv the agent runs. Returns nil for an absent healthcheck, NONE, or a
// disabled one. compose-go's own HealthCheckTest.DecodeMapstructure already
// turns the bare-string form into ["CMD-SHELL", v].
func healthProbeArgv(hc *types.HealthCheckConfig) []string {
	if hc == nil || hc.Disable || len(hc.Test) == 0 || hc.Test[0] == "NONE" {
		return nil
	}
	switch hc.Test[0] {
	case "CMD":
		return hc.Test[1:]
	case "CMD-SHELL":
		return []string{"/bin/sh", "-c", strings.Join(hc.Test[1:], " ")}
	}
	return nil
}

// validateHealthCheck checks a compose-go healthcheck the way this runtime
// needs it validated. A nil pointer (the key was absent) is not an error;
// compose-go's own checkConsistency already rejects a Test whose first
// element is not CMD/CMD-SHELL/NONE, so only the two checks it does not
// perform survive here: a completely empty test, and CMD/CMD-SHELL with no
// command.
func validateHealthCheck(hc *types.HealthCheckConfig) error {
	if hc == nil || hc.Disable {
		return nil
	}
	if len(hc.Test) == 0 {
		return errors.New("healthcheck: 'test' is required (or set 'disable: true')")
	}
	switch hc.Test[0] {
	case "NONE":
		return nil
	case "CMD", "CMD-SHELL":
		if len(hc.Test) < 2 {
			return fmt.Errorf("healthcheck: %s needs a command", hc.Test[0])
		}
		return nil
	default:
		return fmt.Errorf("healthcheck: test must start with CMD, CMD-SHELL or NONE (got %q)", hc.Test[0])
	}
}

// healthProbeBudget derives the polling interval, per-attempt timeout, and
// total wait from a compose-go healthcheck: docker-ish defaults (30s
// interval and timeout, 3 retries per the compose spec) when a field is
// unset. A nil hc (declared to warm the caller's isOneShot-style guards)
// uses the defaults throughout.
func healthProbeBudget(hc *types.HealthCheckConfig) (interval, timeout, total time.Duration) {
	interval, timeout, retries := 30*time.Second, 30*time.Second, uint64(3)
	var startPeriod time.Duration
	if hc != nil {
		if hc.Interval != nil && time.Duration(*hc.Interval) > 0 {
			interval = time.Duration(*hc.Interval)
		}
		if hc.Timeout != nil && time.Duration(*hc.Timeout) > 0 {
			timeout = time.Duration(*hc.Timeout)
		}
		if hc.Retries != nil && *hc.Retries > 0 {
			retries = *hc.Retries
		}
		if hc.StartPeriod != nil {
			startPeriod = time.Duration(*hc.StartPeriod)
		}
	}
	return interval, timeout, startPeriod + (interval+timeout)*time.Duration(retries)
}

// errAgentTransport marks failures of the transport itself (agent not
// listening yet, connection lost, per-attempt deadline expired) as opposed
// to the guest command failing. Probe loops treat these as failed attempts
// within their budget, because on a cold boot the agent starts a moment
// after the VMM does.
var errAgentTransport = errors.New("agent transport failure")

// execCapture runs argv in a running instance through the guest agent,
// non-interactively: no tty, no stdin, no signal forwarding. It returns the
// process exit code and the combined output. A transport failure (agent
// unreachable, connection lost, deadline expired) wraps errAgentTransport,
// distinct from the guest command exiting non-zero.
func execCapture(s *store.Store, instanceID string, argv, env []string, user string, timeout time.Duration) (int, string, error) {
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
		return 0, "", fmt.Errorf("%w: cannot reach guest agent at %s (instance started without agent transport, or image lacks /urunit-agent): %v", errAgentTransport, sockPath, err)
	}
	defer func() { _ = conn.Close() }()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if user == "" {
		user = bundleUser(state.BundleDir)
	}
	const stream = 1
	req := agentproto.OpenRequest{Argv: argv, Env: env, User: user}
	if err := agentproto.WriteJSON(conn, agentproto.TypeOpen, stream, req); err != nil {
		return 0, "", fmt.Errorf("%w: send open request: %v", errAgentTransport, err)
	}
	if err := agentproto.WriteFrame(conn, agentproto.TypeCloseStdin, stream, nil); err != nil {
		return 0, "", fmt.Errorf("%w: close stdin: %v", errAgentTransport, err)
	}

	var out bytes.Buffer
	for {
		f, rerr := agentproto.ReadFrame(conn)
		if rerr != nil {
			return 0, out.String(), fmt.Errorf("%w: connection to guest agent lost: %v", errAgentTransport, rerr)
		}
		switch f.Type {
		case agentproto.TypeStdout, agentproto.TypeStderr:
			out.Write(f.Payload)
		case agentproto.TypeExit:
			var ex agentproto.Exit
			if err := json.Unmarshal(f.Payload, &ex); err != nil {
				// A garbled exit frame must not read as success.
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

// execCaptureRetry is execCapture with transport-class failures retried
// until the budget expires: the guest agent starts a beat after the VMM, so
// the first seconds after boot legitimately EOF. A completed session with a
// non-zero exit is final immediately.
func execCaptureRetry(s *store.Store, instanceID string, argv, env []string, user string, perTry, budget time.Duration) (int, string, error) {
	deadline := time.Now().Add(budget)
	for {
		code, out, err := execCapture(s, instanceID, argv, env, user, perTry)
		if err == nil || !errors.Is(err, errAgentTransport) || time.Now().After(deadline) {
			return code, out, err
		}
		time.Sleep(time.Second)
	}
}

// waitHealthyExec polls the service's exec healthcheck through the agent
// until it exits 0 or the budget runs out. Per the compose spec, a probe
// that times out or cannot run is a FAILED ATTEMPT, not a fatal error: on a
// cold boot the agent starts a beat after the VMM, and a slow probe must
// consume a retry rather than abort the up. The TCP path retries the same
// way. When the budget expires, the last error (including "cannot reach
// guest agent" for images without one) surfaces loudly.
func waitHealthyExec(s *store.Store, instanceID string, hc *types.HealthCheckConfig) error {
	argv := healthProbeArgv(hc)
	if argv == nil {
		return nil
	}
	interval, timeout, budget := healthProbeBudget(hc)
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		code, out, err := execCapture(s, instanceID, argv, nil, "", timeout)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("healthcheck cannot run: %w", err)
		case code == 0:
			return nil
		default:
			lastErr = fmt.Errorf("healthcheck exited %d: %s", code, strings.TrimSpace(out))
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(interval)
	}
}

// runServiceHooks runs post_start or pre_stop entries in order. Any hook
// failure is an error (spec semantics): the caller decides whether that
// tears the project down (post_start during up) or degrades to a warning
// (pre_stop during down, where stopping must proceed regardless).
func runServiceHooks(s *store.Store, instanceID, svcName, kind string, hooks []types.ServiceHook) error {
	for i, h := range hooks {
		if len(h.Command) == 0 {
			return fmt.Errorf("service %q: %s[%d]: command is required", svcName, kind, i)
		}
		// Transport errors retry briefly: hooks often fire moments after
		// boot, before the guest agent is listening. A non-zero exit is
		// final immediately.
		code, out, err := execCaptureRetry(s, instanceID, h.Command, envSlice(h.Environment), h.User, time.Minute, 30*time.Second)
		if err != nil {
			return fmt.Errorf("service %q: %s[%d] cannot run: %w", svcName, kind, i, err)
		}
		if code != 0 {
			return fmt.Errorf("service %q: %s[%d] exited %d: %s", svcName, kind, i, code, strings.TrimSpace(out))
		}
	}
	return nil
}

// composeExecOpts is the hand-parsed flag set for `compose exec`. Flags are
// parsed docker-style: everything after the first positional (the SERVICE)
// belongs to the guest command verbatim, so `compose exec -T a /bin/sh -c x`
// never has -c stolen by the CLI parser (the subcommand registers with
// SkipFlagParsing for exactly this reason).
type composeExecOpts struct {
	noTTY   bool
	user    string
	workdir string
	env     []string
	rest    []string // SERVICE COMMAND [ARGS...]
}

func parseComposeExecArgs(argv []string) (composeExecOpts, error) {
	var o composeExecOpts
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(argv) {
			return "", fmt.Errorf("flag %s needs a value", flag)
		}
		return argv[i+1], nil
	}
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-h" || a == "--help":
			return o, errors.New("usage: compose exec [-T|--no-tty] [-u|--user U] [-e|--env K=V] [-w|--workdir DIR] SERVICE COMMAND [ARGS...]")
		case a == "--":
			i++
			goto done
		case a == "-T" || a == "--no-tty":
			o.noTTY = true
		case a == "-u" || a == "--user":
			v, err := need(i, a)
			if err != nil {
				return o, err
			}
			o.user = v
			i++
		case a == "-w" || a == "--workdir":
			v, err := need(i, a)
			if err != nil {
				return o, err
			}
			o.workdir = v
			i++
		case a == "-e" || a == "--env":
			v, err := need(i, a)
			if err != nil {
				return o, err
			}
			o.env = append(o.env, v)
			i++
		case strings.HasPrefix(a, "-") && a != "-":
			return o, fmt.Errorf("unknown flag %s (supported: -T/--no-tty, -u/--user, -e/--env, -w/--workdir)", a)
		default:
			goto done
		}
	}
done:
	o.rest = argv[i:]
	// Docker strips one "--" between SERVICE and the command; without this,
	// the guest would be asked to exec the separator itself.
	if len(o.rest) >= 2 && o.rest[1] == "--" {
		o.rest = append(o.rest[:1], o.rest[2:]...)
	}
	return o, nil
}

// composeExec implements `compose exec`: resolve SERVICE onto its instance,
// then re-exec the top-level exec command with stdio passed through, so the
// interactive machinery (raw mode, resize, signals) lives in one place.
func composeExec(_ context.Context, cmd *cli.Command) error {
	opts, err := parseComposeExecArgs(cmd.Args().Slice())
	if err != nil {
		return err
	}
	if len(opts.rest) < 2 {
		return errors.New("usage: compose exec [-T] [--user U] [--env K=V] [--workdir DIR] SERVICE COMMAND [ARGS...]")
	}
	svcName := opts.rest[0]
	project := projectName(cmd)
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	proj, err := loadProject(s, project)
	if err != nil {
		return fmt.Errorf("project %q is not up", project)
	}
	instance, ok := proj.Services[svcName]
	if !ok {
		return fmt.Errorf("unknown service %q", svcName)
	}

	// Pre-warm: the agent starts a beat after the VMM, and the interactive
	// session below is deliberately single-shot (retrying it could rerun a
	// non-idempotent command). One retried no-op session proves readiness.
	// Only a transport-class failure blocks: an agent-side error (e.g. the
	// guest has no /bin/true — minimal rootfs images are the norm here)
	// proves the agent is alive, which is all the pre-warm exists to check.
	if _, _, err := execCaptureRetry(s, instance, []string{"/bin/true"}, nil, "", 10*time.Second, 30*time.Second); err != nil && errors.Is(err, errAgentTransport) {
		return fmt.Errorf("service %q: guest agent not ready: %w", svcName, err)
	}

	execArgs := selfExecGlobalArgs(cmd)
	execArgs = append(execArgs, "exec")
	// Docker semantics: tty by default when stdin is one, -T disables.
	if !opts.noTTY {
		if _, terr := getTermios(int(os.Stdin.Fd())); terr == nil {
			execArgs = append(execArgs, "--tty")
		}
	}
	if opts.user != "" {
		execArgs = append(execArgs, "--user", opts.user)
	}
	if opts.workdir != "" {
		execArgs = append(execArgs, "--cwd", opts.workdir)
	}
	for _, e := range opts.env {
		execArgs = append(execArgs, "--env", e)
	}
	// The instance is the child's positional; "--" stops its parser from
	// eating dashes that belong to the guest command.
	execArgs = append(execArgs, instance, "--")
	execArgs = append(execArgs, opts.rest[1:]...)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, execArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err = c.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return err
}

// composeTop implements `compose top`: a process listing per service via the
// guest agent (busybox/procps ps). Best effort per service: a service whose
// image lacks ps or an agent reports its error and the rest still print.
func composeTop(_ context.Context, cmd *cli.Command) error {
	project := projectName(cmd)
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	proj, err := loadProject(s, project)
	if err != nil {
		return fmt.Errorf("project %q is not up", project)
	}
	services := proj.Order
	if cmd.Args().Len() > 0 {
		services = cmd.Args().Slice()
	}
	var firstErr error
	for _, svcName := range services {
		instance, ok := proj.Services[svcName]
		if !ok {
			return fmt.Errorf("unknown service %q", svcName)
		}
		fmt.Printf("%s (%s)\n", svcName, instance)
		code, out, err := execCaptureRetry(s, instance, []string{"/bin/ps"}, nil, "", time.Minute, 30*time.Second)
		switch {
		case err != nil:
			fmt.Printf("  error: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
		case code != 0:
			fmt.Printf("  ps exited %d: %s\n", code, strings.TrimSpace(out))
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: ps exited %d", svcName, code)
			}
		default:
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
	}
	return firstErr
}

// selfExecGlobalArgs mirrors selfExec's global-flag propagation for re-exec
// paths that need their own exec.Command wiring (streaming stdio).
func selfExecGlobalArgs(cmd *cli.Command) []string {
	var full []string
	if v := cmd.String("store-dir"); v != "" {
		full = append(full, "--store-dir", v)
	}
	if cmd.Bool("debug") {
		full = append(full, "--debug")
	}
	return full
}
