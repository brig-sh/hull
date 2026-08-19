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

// Restart policies and the project supervisor.
//
// The per-project gateway daemon is already the one host process with the
// right lifetime — it is started by 'compose up', outlives every service, and
// is stopped by 'compose down' — so it is where supervision lives rather than
// in a second daemon that would double the failure and teardown paths.
//
// The loop is poll-based: every supervisorPollInterval it asks, per service,
// "is the recorded VMM still there?" and re-runs the ones that are gone,
// spaced out by capped exponential backoff. Three divergences from docker
// follow from that and from what the runtime can actually observe today:
//
// - A restart is noticed within the poll interval, not instantly. There is
// no exit event to subscribe to: QMP's SHUTDOWN and the Vz delegate
// callback are monitor-specific and neither carries a status, so polling
// is the honest, uniform mechanism.
// - 'on-failure' degrades to 'always' and says so at load. A plain service
// reports no exit code at all (only a job's agent-run command does), so
// "gone" cannot be split into "failed" and "exited cleanly". The :N
// attempt cap is still honored, which is the part of the policy that is
// observable.
// - 'unless-stopped' is identical to 'always' until a 'compose stop' verb
// exists: the two differ only in whether an explicit stop suppresses the
// restart, and there is nothing yet that can express that intent.
//
// Two invariants are load-bearing and both derive from on-disk state, so they
// survive the daemon dying and being restarted at any moment:
//
// - Never double-start. A service is restarted only after its own record
// says stopped-or-dead, re-checked immediately before the run.
// - Never resurrect. The project's Supervise flag is cleared on disk before
// teardown stops anything, and it is re-read before and after every
// restart, so a service being torn down is not restarted underneath the
// teardown.
//
// A job (one-shot service) is never restarted, whatever its policy says: a
// completed job stays completed, or the completion gate it feeds
// (checkCompletedDeps) would be judging a fresh run of something a dependent
// already passed.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// restartMode is a compose 'restart:' value.
type restartMode string

const (
	restartNo            restartMode = "no"
	restartAlways        restartMode = "always"
	restartOnFailure     restartMode = "on-failure"
	restartUnlessStopped restartMode = "unless-stopped"
)

// restartAccepted names the accepted set verbatim in the error for an unknown
// value: a typo'd policy silently read as "never restart" is exactly the kind
// of quiet approximation forbids.
const restartAccepted = "no, always, on-failure, on-failure:N, unless-stopped"

// restartPolicy is a parsed 'restart:' value.
type restartPolicy struct {
	Mode restartMode
	// MaxAttempts caps 'on-failure:N' restarts. 0 means unlimited, both for
	// bare 'on-failure' and for the explicit ':0' docker also reads as
	// "no limit".
	MaxAttempts int
	// Declared separates an explicit 'restart: no' from an absent key, so
	// 'compose config' renders only what the file actually said.
	Declared bool
}

// parseRestartPolicy parses the compose 'restart:' scalar. An empty value (an
// absent key, or the key present with a null value) is docker's default, "no";
// anything not in the accepted set is an error naming the whole set.
func parseRestartPolicy(raw string) (restartPolicy, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return restartPolicy{Mode: restartNo}, nil
	}
	switch restartMode(s) {
	case restartNo, restartAlways, restartOnFailure, restartUnlessStopped:
		return restartPolicy{Mode: restartMode(s), Declared: true}, nil
	}
	if count, ok := strings.CutPrefix(s, string(restartOnFailure)+":"); ok {
		n, err := strconv.Atoi(count)
		if err != nil || n < 0 {
			return restartPolicy{}, fmt.Errorf("invalid restart policy %q: %q is not a retry count (accepted: %s)", raw, count, restartAccepted)
		}
		return restartPolicy{Mode: restartOnFailure, MaxAttempts: n, Declared: true}, nil
	}
	return restartPolicy{}, fmt.Errorf("invalid restart policy %q (accepted: %s)", raw, restartAccepted)
}

// String renders the policy in the form the parser accepts, so 'compose
// config' round-trips.
func (p restartPolicy) String() string {
	if p.Mode == restartOnFailure && p.MaxAttempts > 0 {
		return string(restartOnFailure) + ":" + strconv.Itoa(p.MaxAttempts)
	}
	if p.Mode == "" {
		return string(restartNo)
	}
	return string(p.Mode)
}

// effectiveMode is the policy the supervisor can actually enforce today. See
// the file comment for why on-failure and unless-stopped both collapse into
// always; Phase B narrows this without changing any name here.
func (p restartPolicy) effectiveMode() restartMode {
	switch p.Mode {
	case restartOnFailure, restartUnlessStopped, restartAlways:
		return restartAlways
	default:
		return restartNo
	}
}

// restarts reports whether a service's disappearance triggers a restart.
func (p restartPolicy) restarts() bool {
	return p.effectiveMode() == restartAlways
}

// warnRestartDivergence reports, at load time, that a declared policy is not
// enforced as written. Only on-failure needs it: it is the one policy whose
// meaning depends on a fact the runtime cannot observe for a plain service.
func warnRestartDivergence(warn io.Writer, svcName string, p restartPolicy) {
	if p.Mode != restartOnFailure {
		return
	}
	capNote := ""
	if p.MaxAttempts > 0 {
		capNote = fmt.Sprintf(" (the %d-attempt cap is still honored)", p.MaxAttempts)
	}
	composeWarn(warn, "service %q: restart %q degrades to %q: no exit code is observable for a plain service, "+
		"so a clean exit cannot be distinguished from a failure%s", svcName, p.String(), string(restartAlways), capNote)
}

// Poll and backoff tuning.
//
// The poll interval is the detection latency and the whole cost of
// supervision: one state.json read plus a signal-0 probe per service, so 2s
// keeps "my service came back" inside the couple of seconds a person reads as
// immediate while costing nothing measurable. It is deliberately of the same
// order as the default healthcheck interval (1s), not smaller: a VM that just
// died is not going to be back sooner than that.
//
// Backoff starts at one second because a VM boot is already ~a second — a
// tighter first retry cannot help — doubles, and caps at a minute so a service
// that is broken for hours retries 60 times an hour instead of thousands, and
// still recovers within a minute of whatever was wrong being fixed.
// restartStableUptime is how long a restarted service has to stay up before
// its restart history is forgiven: long enough that a crash-loop cannot reset
// its own budget by flapping, short enough that a service which really
// recovered is not penalized by an outage hours ago.
const (
	supervisorPollInterval = 2 * time.Second
	restartBackoffBase     = 1 * time.Second
	restartBackoffCap      = 60 * time.Second
	restartStableUptime    = 60 * time.Second
)

// restartBackoff returns the wait before the attempt-th restart of a service
// (attempt is 1-based). The schedule is 1s, 2s, 4s, 8s, 16s, 32s, then 60s
// forever: monotonic and capped, never overflowing however long a service
// stays broken.
func restartBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return restartBackoffBase
	}
	d := restartBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= restartBackoffCap {
			return restartBackoffCap
		}
	}
	return d
}

// supervisedService is one poll target: a service of the project, the instance
// that carries it, and how it is to be treated when it disappears.
type supervisedService struct {
	name     string
	instance string
	policy   restartPolicy
	// job marks a one-shot service. A job is never restarted.
	job bool
}

// supervisorHooks are every effect the loop has on the world, injected so the
// policy engine can be exercised without booting a VM.
type supervisorHooks struct {
	// now is the clock the backoff schedule is measured against.
	now func() time.Time
	// loadProject re-reads the persisted project state. Its error is what
	// "the project is gone" means.
	loadProject func() (*composeProject, error)
	// targets lists what to poll, re-derived from the project state and the
	// compose file on every tick so the daemon never acts on a stale view.
	targets func(*composeProject) ([]supervisedService, error)
	// alive reports whether the instance's VMM is still there.
	alive func(instance string) bool
	// restart re-runs a service. It performs its own idempotency and
	// teardown re-checks: this call can take as long as a VM boot.
	restart func(supervisedService) error
	// warnf reports something the operator should see.
	warnf func(format string, args ...any)
	// killInFlight ends a restart still booting. Used only when shutdown has
	// waited out its grace: the child is setpgid, so leaving it running would
	// orphan a VM past teardown.
	killInFlight func()
}

// errProjectGone reports that the project state could not be read: either
// 'down' removed it, or 'up' has not written it yet.
var errProjectGone = errors.New("no project state")

// restartTracker is a service's in-memory restart history.
//
// It is intentionally not persisted. What must survive the daemon dying is the
// pair of invariants in the file comment, and both are read from disk; a
// forgotten attempt count only means a restarted supervisor is more forgiving
// than one that never died, which is the safe direction to be wrong in.
type restartTracker struct {
	attempts int
	// next is the earliest time the next restart may run.
	next time.Time
	// gaveUp latches an exhausted on-failure:N budget, so the give-up is
	// reported once and not re-decided every tick.
	gaveUp bool
	// aliveSince is when the service was first seen alive in its current run,
	// zero while it is down. It is what makes a recovery "stable".
	aliveSince time.Time
}

// supervisor is the policy engine: it owns the backoff state and decides, per
// tick, which services to restart. All of its effects go through hooks.
type supervisor struct {
	interval time.Duration
	hooks    supervisorHooks
	trackers map[string]*restartTracker
	// sawProject latches the first successful project-state read. Before it,
	// a missing state file means 'up' has not saved it yet; after it, the
	// project has been torn down and there is nothing left to supervise.
	sawProject bool
	// goneStreak counts consecutive unreadable project states, so one bad
	// read cannot end supervision permanently.
	goneStreak int
}

func newSupervisor(interval time.Duration, hooks supervisorHooks) *supervisor {
	if interval <= 0 {
		interval = supervisorPollInterval
	}
	return &supervisor{interval: interval, hooks: hooks, trackers: map[string]*restartTracker{}}
}

// run polls until ctx is cancelled or the project it supervises is gone.
func (sv *supervisor) run(ctx context.Context) {
	ticker := time.NewTicker(sv.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := sv.tick(ctx)
			switch {
			case err == nil:
				sv.goneStreak = 0
			case errors.Is(err, errProjectGone) && sv.sawProject:
				// Tolerate a couple of bad reads before believing it: the
				// state file is written atomically now, but a transient I/O
				// error must not end supervision for the project's life, and
				// this path is Debug-only so the operator would never know.
				sv.goneStreak++
				if sv.goneStreak < projectGoneStreak {
					log.WithError(err).Debugf("supervisor: project state unreadable (%d/%d)", sv.goneStreak, projectGoneStreak)
					continue
				}
				log.Debug("supervisor: project state is gone; nothing left to supervise")
				return
			default:
				// A transient failure (an unreadable or momentarily invalid
				// compose file) must not end supervision for the rest of the
				// project's life.
				log.WithError(err).Debug("supervisor: skipping tick")
			}
		}
	}
}

// tick is one poll of the whole project. ctx is checked between services:
// with N crashed services, a shutdown that only checked once per tick would
// wait N boots instead of one.
func (sv *supervisor) tick(ctx context.Context) error {
	proj, err := sv.hooks.loadProject()
	if err != nil {
		return fmt.Errorf("%w: %v", errProjectGone, err)
	}
	sv.sawProject = true
	if !proj.Supervise {
		// Teardown has begun (or the project was created by a binary that
		// predates supervision): restarting now would fight it.
		return nil
	}
	targets, err := sv.hooks.targets(proj)
	if err != nil {
		return err
	}
	for _, t := range targets {
		// Between services too: with N crashed services, a shutdown that
		// only checked once would wait N boots instead of one.
		if ctx.Err() != nil {
			return nil
		}
		sv.consider(t)
	}
	return nil
}

// consider applies one service's policy to one observation of it.
func (sv *supervisor) consider(t supervisedService) {
	// A job is never restarted, whatever its policy says. Its exit status is
	// the thing dependents gated on (checkCompletedDeps); re-running it would
	// replace a result that has already been acted upon.
	if t.job {
		return
	}
	if !t.policy.restarts() {
		return
	}
	tr := sv.trackers[t.name]
	if tr == nil {
		tr = &restartTracker{}
		sv.trackers[t.name] = tr
	}
	now := sv.hooks.now()
	if sv.hooks.alive(t.instance) {
		// A service that has been up for restartStableUptime has recovered:
		// its history is forgiven, so a crash next week is a new incident and
		// not a continuation of this one (docker's model, and without it a
		// long-lived service would inherit the maximum backoff and an
		// exhausted on-failure budget forever).
		switch {
		case tr.aliveSince.IsZero():
			tr.aliveSince = now
		case now.Sub(tr.aliveSince) >= restartStableUptime:
			*tr = restartTracker{aliveSince: tr.aliveSince}
		}
		return
	}
	tr.aliveSince = time.Time{}
	if tr.gaveUp {
		return
	}
	if t.policy.MaxAttempts > 0 && tr.attempts >= t.policy.MaxAttempts {
		tr.gaveUp = true
		sv.hooks.warnf("service %q stayed down after %d restart attempt(s); its restart policy %q gives up",
			t.name, tr.attempts, t.policy.String())
		return
	}
	if !tr.next.IsZero() && now.Before(tr.next) {
		return // still backing off
	}
	tr.attempts++
	tr.next = now.Add(restartBackoff(tr.attempts))
	if err := sv.hooks.restart(t); err != nil {
		sv.hooks.warnf("service %q could not be restarted (attempt %d): %v", t.name, tr.attempts, err)
	}
}

// projectSupervisor is the real world behind the hooks: the store, the
// project's persisted state, and this binary re-invoked as 'run'.
type projectSupervisor struct {
	cmd     *cli.Command
	store   *store.Store
	project string
	volRoot string
	// confirmed remembers the pid whose argv was verified to be one of our
	// VMMs, per instance. A live pid cannot be reused, so the argv check is
	// needed once per (re)start rather than on every poll — which keeps the
	// poll a single signal-0 probe and still catches a pid inherited by an
	// unrelated process after a VMM crash.
	confirmed map[string]confirmedPID
	// inFlight is the restart child currently booting, if any.
	inFlight atomic.Pointer[exec.Cmd]
}

// newProjectSupervisor builds the supervisor the gateway daemon runs.
func newProjectSupervisor(cmd *cli.Command, project string, interval time.Duration) (*supervisor, error) {
	s, err := globalStore(cmd)
	if err != nil {
		return nil, err
	}
	ps := &projectSupervisor{
		cmd:       cmd,
		store:     s,
		project:   project,
		volRoot:   volumesRoot(cmd),
		confirmed: map[string]confirmedPID{},
	}
	return newSupervisor(interval, supervisorHooks{
		killInFlight: ps.killInFlight,
		now:          time.Now,
		loadProject:  func() (*composeProject, error) { return loadProject(s, project) },
		targets:      ps.targets,
		alive:        ps.alive,
		restart:      ps.restart,
		warnf: func(format string, args ...any) {
			log.Warnf("supervisor: "+format, args...)
		},
	}), nil
}

// targets re-derives the poll list from the persisted project state and the
// compose file it was started from. Both are read every tick: the daemon may
// have been started before 'up' created the last service, or restarted long
// after, and either way it must act on what is on disk now.
func (ps *projectSupervisor) targets(proj *composeProject) ([]supervisedService, error) {
	p, err := reloadProject(context.Background(), proj)
	if err != nil {
		return nil, fmt.Errorf("cannot reload %s: %w", proj.File, err)
	}
	out := make([]supervisedService, 0, len(proj.Order))
	for _, name := range proj.Order {
		instance, ok := proj.Services[name]
		if !ok {
			// 'up' has not created this service yet. Starting it here would
			// bypass its depends_on gates.
			continue
		}
		svc, ok := p.Services[name]
		if !ok {
			continue // the file no longer declares it
		}
		pol, err := parseRestartPolicy(svc.Restart)
		if err != nil {
			continue // the file's restart policy drifted into something invalid; skip, best effort
		}
		out = append(out, supervisedService{
			name:     name,
			instance: instance,
			policy:   pol,
			job:      isOneShot(p, name),
		})
	}
	return out, nil
}

// alive reports whether the instance's VMM is still running, from the record
// plus a signal-0 probe, with a one-off argv check per pid against pid reuse.
func (ps *projectSupervisor) alive(instance string) bool {
	st, err := ps.store.GetInstance(instance)
	if err != nil || st.Status != "running" || st.PID <= 0 {
		return false
	}
	if syscall.Kill(st.PID, 0) != nil {
		return false
	}
	// The argv check costs one /bin/ps, so it is not run on every poll — but
	// it MUST expire: a VMM can die and its pid be recycled to an unrelated
	// process, and a cache with no expiry would report that as alive forever,
	// silently disabling the restart policy.
	if c, ok := ps.confirmed[instance]; ok && c.pid == st.PID && time.Since(c.at) < confirmTTL {
		return true
	}
	if !processIsAVMM(st.PID, st) {
		delete(ps.confirmed, instance)
		return false
	}
	ps.confirmed[instance] = confirmedPID{pid: st.PID, at: time.Now()}
	return true
}

// confirmedPID is an argv-verified pid with the time it was verified: the
// verification expires so a recycled pid cannot read as alive indefinitely.
type confirmedPID struct {
	pid int
	at  time.Time
}

// confirmTTL is how long an argv verification is trusted. Well under the
// time it takes a pid to be recycled in practice, and a /bin/ps every 30s
// per service is negligible.
const confirmTTL = 30 * time.Second

// projectGoneStreak is how many consecutive unreadable project states are
// needed before the supervisor accepts that the project is gone. One is too
// few: the read is unlocked and the process exits silently on this path.
const projectGoneStreak = 3

// supervisorKeptLogs is how many prior boots' consoles are kept per service.
const supervisorKeptLogs = 3

// spawn runs the binary again for one restart, remembering the child so a
// shutdown that outlasts its grace can kill it instead of leaving an orphan
// VM behind (the child is setpgid, so it survives this process otherwise).
func (ps *projectSupervisor) spawn(args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	full := append(selfExecGlobalArgs(ps.cmd), args...)
	c := exec.Command(exe, full...)
	ps.inFlight.Store(c)
	defer ps.inFlight.Store(nil)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// killInFlight kills a restart that is still booting. Called only when
// shutdown has already waited out its grace: an orphan VM the teardown
// cannot see is worse than a half-created instance, which 'up' refuses to
// reuse and 'down' cleans up.
func (ps *projectSupervisor) killInFlight() {
	if c := ps.inFlight.Load(); c != nil && c.Process != nil {
		_ = c.Process.Kill()
	}
}

// killInFlight forwards to the effects layer when one is wired.
func (sv *supervisor) killInFlight() {
	if sv.hooks.killInFlight != nil {
		sv.hooks.killInFlight()
	}
}

// preserveLog moves an instance's console aside before its record is
// deleted, as <instance>.<n>.log next to the store's compose state, so the
// evidence for a crash loop survives the restart that follows it. Best
// effort: losing the copy must never stop a restart.
func (ps *projectSupervisor) preserveLog(instance string) {
	src := ps.store.InstanceLogFile(instance)
	if st, err := os.Stat(src); err != nil || st.Size() == 0 {
		return
	}
	dir := filepath.Join(ps.store.RootDir(), "compose", "crashlogs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	// Keep a bounded history: the most recent few boots explain a loop, and
	// an unbounded pile would grow for as long as the loop runs.
	for n := supervisorKeptLogs - 1; n >= 1; n-- {
		_ = os.Rename(
			filepath.Join(dir, fmt.Sprintf("%s.%d.log", instance, n)),
			filepath.Join(dir, fmt.Sprintf("%s.%d.log", instance, n+1)))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.1.log", instance)), data, 0600)
}

// restart re-runs one service, with the two on-disk invariants re-checked
// around the boot: not already running, and the project still wants to be
// supervised.
func (ps *projectSupervisor) restart(t supervisedService) error {
	// Idempotency: the poll that decided this service was gone is up to one
	// interval old, and 'up' or a person may have started it since.
	if ps.alive(t.instance) {
		return nil
	}
	// The teardown gate, re-read from disk rather than taken from the tick
	// that scheduled this restart: 'down' may have cleared it since.
	proj, err := loadProject(ps.store, ps.project)
	if err != nil {
		return fmt.Errorf("%w: %v", errProjectGone, err)
	}
	if !proj.Supervise {
		return nil
	}
	p, err := reloadProject(context.Background(), proj)
	if err != nil {
		return fmt.Errorf("cannot reload %s: %w", proj.File, err)
	}
	svc, ok := p.Services[t.name]
	if !ok {
		return fmt.Errorf("%s no longer declares service %q", proj.File, t.name)
	}
	// Belt to targets' job flag: a recorded exit status exists only for a job
	// that ran its command through the agent, and a completed job stays
	// completed even if the file stopped marking it as one.
	if isOneShot(p, t.name) {
		return nil
	}
	if st, err := ps.store.GetInstance(t.instance); err == nil {
		// A deliberate 'stop' outranks the restart policy, for every policy
		// and not just unless-stopped: docker's restart manager makes the
		// same call, and without it 'hull stop' on a supervised
		// service is undone within one poll.
		if st.StoppedByUser {
			return nil
		}
		if st.ExitCode != nil {
			return nil
		}
	}

	// Only this service is re-run: its depends_on gates are not re-evaluated,
	// which is docker's behavior too. The dependencies are already up (or the
	// project is in a state 'down' should resolve), and re-gating here would
	// let one service's crash block on another's healthcheck.
	//
	// 'run' refuses to reuse an instance name, so the dead record has to go
	// first. It is marked stopped before being deleted: if this process dies
	// in between, 'ps' and 'down' see an honestly stopped instance instead of
	// one claiming to run on a pid that is gone.
	if err := ps.clearInstance(t.instance); err != nil {
		return err
	}
	args, err := serviceRunArgs(t.name, t.instance, svc, ps.launch(proj, p, t.name))
	if err != nil {
		return err
	}
	log.Infof("supervisor: restarting service %q (%s) per restart %q", t.name, t.instance, t.policy.String())
	if out, err := ps.spawn(args...); err != nil {
		return fmt.Errorf("run: %v: %s", err, sanitizeGuestText(out))
	}
	// Teardown may have started while the VM was booting. A service must
	// never outlive its project, so undo the restart rather than leave a VM
	// behind that 'down' has already walked past.
	if !supervisionActive(ps.store, ps.project) {
		log.Warnf("supervisor: project %q was torn down while %q was restarting; stopping it again", ps.project, t.name)
		if err := stopInstanceIn(ps.store, t.instance, 3); err != nil {
			log.WithError(err).Warnf("supervisor: could not stop %q after teardown began", t.instance)
		}
		if err := ps.store.DeleteInstance(t.instance); err != nil {
			log.WithError(err).Warnf("supervisor: could not remove %q after teardown began", t.instance)
		}
	}
	return nil
}

// launch rebuilds the non-service half of the run invocation from persisted
// state, so a restarted service is launched with the argv it first got.
func (ps *projectSupervisor) launch(proj *composeProject, p *types.Project, svcName string) serviceLaunch {
	return serviceLaunch{
		project:     proj.Name,
		gatewaySock: proj.SwitchSock,
		maskBits:    proj.maskBits(),
		ip:          proj.IPs[svcName],
		hostEntries: projectHostEntries(proj.Order, proj.IPs, proj.Name, p.Services),
		volumesRoot: ps.volRoot,
		// A job is never restarted, so the benign-init form is never needed
		// here; restart returns early for one.
		oneShot: false,
	}
}

// clearInstance retires a dead instance record so its name can be reused.
func (ps *projectSupervisor) clearInstance(instance string) error {
	if st, err := ps.store.GetInstance(instance); err == nil && st.Status != "stopped" {
		// A record at "starting" with no pid is not a dead instance, it is an
		// instance whose pid we never learned -- and alive() answers false for
		// it, so the supervisor arrives here and would delete the directory and
		// boot a second VMM on the same rootfs, unattended. Reap the first one
		// using the argv recorded before it was spawned.
		if st.Status == store.StatusStarting && st.PID <= 0 {
			if reapOrphanVMM(ps.store, st) {
				log.Warnf("supervisor: instance %s had a VMM running that was never "+
					"recorded; killed it before restarting", instance)
			}
		}
		st.Status = "stopped"
		st.PID = 0
		if st.ExitedAt.IsZero() {
			st.ExitedAt = time.Now()
		}
		_ = ps.store.SaveInstance(st)
	}
	delete(ps.confirmed, instance)
	// Keep the console of the boot that just died: DeleteInstance removes the
	// whole instance directory, so a crash-looping service would erase the
	// output explaining each crash on every backoff, and 'compose logs' would
	// only ever show the boot that is currently failing.
	ps.preserveLog(instance)
	if err := ps.store.DeleteInstance(instance); err != nil {
		return fmt.Errorf("could not clear the previous instance %q: %w", instance, err)
	}
	return nil
}

// supervisionActive re-reads from disk whether the project still wants its
// services supervised. Every restart consults it twice, before and after the
// boot, which is what keeps supervision from fighting teardown.
func supervisionActive(s *store.Store, project string) bool {
	proj, err := loadProject(s, project)
	return err == nil && proj.Supervise
}

// pauseSupervision clears the flag and persists it before a teardown stops
// anything. It is never set back: 'up' is the only thing that enables
// supervision, so a project can not be resurrected by a stale daemon.
func pauseSupervision(s *store.Store, proj *composeProject, warn io.Writer) {
	if !proj.Supervise {
		return
	}
	proj.Supervise = false
	if err := saveProject(s, proj); err != nil {
		composeWarn(warn, "could not stop supervision of %q before teardown: %v", proj.Name, err)
		// Leave the in-memory flag cleared: the caller's own re-saves must
		// not turn it back on.
	}
}
