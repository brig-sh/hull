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

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// TestRestartPolicyParsing pins the whole surface of 'restart:': every
// accepted form, the loud failure for anything else, and the two places the
// parsed policy shows up — the load-time divergence warnings and the
// 'compose config' rendering.
func TestRestartPolicyParsing(t *testing.T) {
	t.Run("every accepted form parses", func(t *testing.T) {
		cases := []struct {
			raw       string
			want      restartPolicy
			wantStr   string
			restarts  bool
			effective restartMode
		}{
			// An absent key is docker's default: never restart.
			{"", restartPolicy{Mode: restartNo}, "no", false, restartNo},
			{"no", restartPolicy{Mode: restartNo, Declared: true}, "no", false, restartNo},
			{"always", restartPolicy{Mode: restartAlways, Declared: true}, "always", true, restartAlways},
			{"on-failure", restartPolicy{Mode: restartOnFailure, Declared: true}, "on-failure", true, restartAlways},
			{"on-failure:3", restartPolicy{Mode: restartOnFailure, MaxAttempts: 3, Declared: true}, "on-failure:3", true, restartAlways},
			// unless-stopped is accepted and, with no 'compose stop' verb to
			// suppress a restart, behaves exactly like always (ADR-0007).
			{"unless-stopped", restartPolicy{Mode: restartUnlessStopped, Declared: true}, "unless-stopped", true, restartAlways},
			// Surrounding whitespace is not a different policy.
			{"  always  ", restartPolicy{Mode: restartAlways, Declared: true}, "always", true, restartAlways},
		}
		for _, tc := range cases {
			t.Run(tc.raw, func(t *testing.T) {
				got, err := parseRestartPolicy(tc.raw)
				if err != nil {
					t.Fatalf("parseRestartPolicy(%q) = %v", tc.raw, err)
				}
				if got != tc.want {
					t.Errorf("parseRestartPolicy(%q) = %+v, want %+v", tc.raw, got, tc.want)
				}
				if got.String() != tc.wantStr {
					t.Errorf("String() = %q, want %q", got.String(), tc.wantStr)
				}
				// The canonical rendering must parse back to the same policy,
				// or 'compose config' output would not be a valid input.
				back, err := parseRestartPolicy(got.String())
				if err != nil || back.Mode != got.Mode || back.MaxAttempts != got.MaxAttempts {
					t.Errorf("re-parsing %q gave %+v, %v", got.String(), back, err)
				}
				if got.restarts() != tc.restarts {
					t.Errorf("restarts() = %v, want %v", got.restarts(), tc.restarts)
				}
				if got.effectiveMode() != tc.effective {
					t.Errorf("effectiveMode() = %q, want %q", got.effectiveMode(), tc.effective)
				}
			})
		}
	})

	t.Run("on-failure:N carries the attempt cap", func(t *testing.T) {
		for raw, want := range map[string]int{
			"on-failure":    0, // unlimited
			"on-failure:0":  0, // docker reads an explicit 0 as unlimited too
			"on-failure:1":  1,
			"on-failure:12": 12,
		} {
			p, err := parseRestartPolicy(raw)
			if err != nil {
				t.Fatalf("parseRestartPolicy(%q) = %v", raw, err)
			}
			if p.MaxAttempts != want {
				t.Errorf("parseRestartPolicy(%q).MaxAttempts = %d, want %d", raw, p.MaxAttempts, want)
			}
		}
	})

	t.Run("an unknown value is a loud error naming the accepted set", func(t *testing.T) {
		// 'false' matters: YAML's 'restart: no' is a bool, and the sibling
		// spelling must not be quietly read as "no restarts".
		for _, raw := range []string{"sometimes", "true", "false", "onfailure", "on-failure:", "on-failure:x", "on-failure:-1", "Always", "unless_stopped"} {
			t.Run(raw, func(t *testing.T) {
				p, err := parseRestartPolicy(raw)
				if err == nil {
					t.Fatalf("parseRestartPolicy(%q) accepted it as %+v", raw, p)
				}
				if !strings.Contains(err.Error(), restartAccepted) {
					t.Errorf("error %q does not name the accepted set %q", err, restartAccepted)
				}
				if !strings.Contains(err.Error(), raw) {
					t.Errorf("error %q does not quote the offending value", err)
				}
			})
		}
	})

	t.Run("an unknown value fails the load", func(t *testing.T) {
		src := "services:\n  web:\n    image: img:1\n    restart: sometimes\n"
		_, _, err := renderConfig(t, src)
		want := `service "web": invalid restart policy "sometimes" (accepted: ` + restartAccepted + `)`
		if err == nil || err.Error() != want {
			t.Fatalf("load error = %v, want %q", err, want)
		}
	})

	t.Run("on-failure warns that it degrades to always", func(t *testing.T) {
		src := `
services:
  tworker:
    image: img:1
    restart: on-failure
  capped:
    image: img:2
    restart: on-failure:5
  plain:
    image: img:3
    restart: always
`
		p, warn, err := loadTestProject(t, src)
		if err != nil {
			t.Fatal(err)
		}
		wantWarning(t, warn, `warning: compose: service "tworker": restart "on-failure" degrades to "always": `+
			`no exit code is observable for a plain service, so a clean exit cannot be distinguished from a failure`)
		wantWarning(t, warn, `warning: compose: service "capped": restart "on-failure:5" degrades to "always": `+
			`no exit code is observable for a plain service, so a clean exit cannot be distinguished from a failure (the 5-attempt cap is still honored)`)
		// Exactly the two on-failure services warn: 'always' is honored as
		// written and must stay silent.
		if got := warningCount(warn); got != 2 {
			t.Errorf("got %d warning lines, want 2:\n%s", got, warn)
		}
		// The degradation is what the supervisor enforces, and the cap
		// survives it: that is the observable half of on-failure.
		capped, err := parseRestartPolicy(p.Services["capped"].Restart)
		if err != nil {
			t.Fatal(err)
		}
		if capped.effectiveMode() != restartAlways || capped.MaxAttempts != 5 {
			t.Errorf("capped policy = %+v, want always with a 5-attempt cap", capped)
		}
	})

	t.Run("a job's policy is ignored and says so", func(t *testing.T) {
		src := "services:\n  seed:\n    image: img:1\n    command: [\"/app\", \"seed\"]\n    x-oneshot: true\n    restart: always\n"
		_, warnings, err := renderConfig(t, src)
		if err != nil {
			t.Fatal(err)
		}
		wantWarning(t, warnings, `warning: compose: service "seed": restart "always" is ignored: the service is a job, and a completed job stays completed`)
	})

	t.Run("config renders the declared policy", func(t *testing.T) {
		src := `
services:
  web:
    image: img:1
    restart: unless-stopped
  worker:
    image: img:2
    restart: on-failure:4
  once:
    image: img:3
    restart: "no"
  quiet:
    image: img:4
`
		out, warnings, err := renderConfig(t, src)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"restart: unless-stopped", "restart: on-failure:4", `restart: "no"`} {
			mustContain(t, out, want, "the declared policy must be rendered")
		}
		// A service that declares nothing renders nothing: config shows the
		// file's effective content, not a wall of defaults.
		if got := strings.Count(out, "restart:"); got != 3 {
			t.Errorf("got %d restart keys, want 3:\n%s", got, out)
		}
		// 'restart' is a supported key now; warning about it would be a lie.
		mustNotContain(t, warnings, "unsupported key", "restart must not warn as unsupported")
	})
}

// TestSupervisorBackoff drives the supervision loop against a fake world: the
// backoff schedule, the policies that must never restart anything, the
// on-failure attempt cap, and the teardown gate. None of it boots a VM.
func TestSupervisorBackoff(t *testing.T) {
	t.Run("the schedule is capped and monotonic", func(t *testing.T) {
		if got := restartBackoff(1); got != restartBackoffBase {
			t.Errorf("restartBackoff(1) = %s, want the base %s", got, restartBackoffBase)
		}
		// A zero or negative attempt must not produce a zero wait: a hot loop
		// of VM boots is the worst failure mode this schedule prevents.
		for _, attempt := range []int{-1, 0} {
			if got := restartBackoff(attempt); got != restartBackoffBase {
				t.Errorf("restartBackoff(%d) = %s, want the base %s", attempt, got, restartBackoffBase)
			}
		}
		prev := time.Duration(0)
		for attempt := 1; attempt <= 200; attempt++ {
			got := restartBackoff(attempt)
			if got < prev {
				t.Fatalf("restartBackoff(%d) = %s, less than the previous %s", attempt, got, prev)
			}
			if got > restartBackoffCap {
				t.Fatalf("restartBackoff(%d) = %s, over the cap %s", attempt, got, restartBackoffCap)
			}
			prev = got
		}
		if got := restartBackoff(200); got != restartBackoffCap {
			t.Errorf("restartBackoff(200) = %s, want the cap %s", got, restartBackoffCap)
		}
	})

	t.Run("restarts are spaced by the backoff", func(t *testing.T) {
		w := newFakeWorld(supervised("web", restartPolicy{Mode: restartAlways, Declared: true}))
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 400)
		if len(w.restartAt) < 5 {
			t.Fatalf("an 'always' service that stays down must keep being restarted, got %d", len(w.restartAt))
		}
		prev := w.restartAt[0]
		for i := 1; i < len(w.restartAt); i++ {
			gap := w.restartAt[i].Sub(prev)
			// The i-th restart may not run before its backoff has elapsed,
			// and may not be delayed by more than one poll beyond it.
			want := restartBackoff(i)
			if gap < want {
				t.Errorf("restart %d came %s after the previous one, before its %s backoff", i+1, gap, want)
			}
			if gap > want+sv.interval {
				t.Errorf("restart %d came %s after the previous one, more than %s + one poll", i+1, gap, want)
			}
			prev = w.restartAt[i]
		}
		// The tail of a long outage retries at the cap, not faster and not
		// ever more slowly.
		last := w.restartAt[len(w.restartAt)-1].Sub(w.restartAt[len(w.restartAt)-2])
		if last < restartBackoffCap || last > restartBackoffCap+sv.interval {
			t.Errorf("the last gap is %s, want the %s cap (+ at most one poll)", last, restartBackoffCap)
		}
	})

	t.Run("policy no never restarts", func(t *testing.T) {
		// Both an explicit 'restart: no' and an absent key (the zero policy).
		w := newFakeWorld(
			supervised("explicit", restartPolicy{Mode: restartNo, Declared: true}),
			supervised("absent", restartPolicy{}),
		)
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 100)
		if len(w.restarts) != 0 {
			t.Errorf("a service that declares no restart policy must stay down, got %v", w.restarts)
		}
	})

	t.Run("on-failure:N stops after N attempts", func(t *testing.T) {
		w := newFakeWorld(supervised("worker", restartPolicy{Mode: restartOnFailure, MaxAttempts: 3, Declared: true}))
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 400)
		if len(w.restarts) != 3 {
			t.Errorf("on-failure:3 restarted %d times, want exactly 3: %v", len(w.restarts), w.restarts)
		}
		// Giving up is reported once, not re-decided on every poll.
		var gaveUp int
		for _, line := range w.warnings {
			if strings.Contains(line, "gives up") {
				gaveUp++
			}
		}
		if gaveUp != 1 {
			t.Errorf("got %d give-up warnings, want exactly 1:\n%s", gaveUp, strings.Join(w.warnings, "\n"))
		}
	})

	t.Run("a job is never restarted", func(t *testing.T) {
		// ADR-0007: a completed job stays completed, whatever its policy says.
		// The completion gate other services passed was about that run.
		job := supervised("migrate", restartPolicy{Mode: restartAlways, Declared: true})
		job.job = true
		unlessStopped := supervised("seed", restartPolicy{Mode: restartUnlessStopped, Declared: true})
		unlessStopped.job = true
		w := newFakeWorld(job, unlessStopped)
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 100)
		if len(w.restarts) != 0 {
			t.Errorf("a job must never be restarted, got %v", w.restarts)
		}
	})

	t.Run("a live service is not restarted", func(t *testing.T) {
		w := newFakeWorld(supervised("web", restartPolicy{Mode: restartAlways, Declared: true}))
		w.alive["web-instance"] = true
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 100)
		if len(w.restarts) != 0 {
			t.Errorf("a running service must not be started a second time, got %v", w.restarts)
		}
	})

	t.Run("a stable recovery forgives the attempt budget", func(t *testing.T) {
		// on-failure:1 that already gave up: once the service has been up for
		// restartStableUptime, a later crash is a new incident, not a
		// continuation of the exhausted one.
		w := newFakeWorld(supervised("worker", restartPolicy{Mode: restartOnFailure, MaxAttempts: 1, Declared: true}))
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 10)
		if len(w.restarts) != 1 {
			t.Fatalf("on-failure:1 restarted %d times, want 1", len(w.restarts))
		}
		w.alive["worker-instance"] = true
		w.drive(sv, int(restartStableUptime/sv.interval)+2)
		w.alive["worker-instance"] = false
		w.drive(sv, 10)
		if len(w.restarts) != 2 {
			t.Errorf("a crash after a stable run must get its own budget, got %d restart(s)", len(w.restarts))
		}
		// A flap that never reaches the stable window must NOT refill it.
		w = newFakeWorld(supervised("flap", restartPolicy{Mode: restartOnFailure, MaxAttempts: 1, Declared: true}))
		sv = newSupervisor(supervisorPollInterval, w.hooks())
		for i := 0; i < 20; i++ {
			w.alive["flap-instance"] = i%2 == 0
			w.drive(sv, 1)
		}
		if len(w.restarts) != 1 {
			t.Errorf("a flapping service refilled its budget: %d restarts", len(w.restarts))
		}
	})

	t.Run("supervision paused for teardown restarts nothing", func(t *testing.T) {
		w := newFakeWorld(supervised("web", restartPolicy{Mode: restartAlways, Declared: true}))
		w.project.Supervise = false
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		w.drive(sv, 100)
		if len(w.restarts) != 0 {
			t.Errorf("a torn-down project must not be resurrected, got %v", w.restarts)
		}
		// The flag is the only thing that was holding it back: with it set,
		// the same loop restarts the same service.
		w.project.Supervise = true
		w.drive(sv, 10)
		if len(w.restarts) == 0 {
			t.Error("supervision did not resume once the project asked for it")
		}
	})

	t.Run("a missing project stops the loop only once it was seen", func(t *testing.T) {
		w := newFakeWorld(supervised("web", restartPolicy{Mode: restartAlways, Declared: true}))
		w.projectErr = os.ErrNotExist
		sv := newSupervisor(supervisorPollInterval, w.hooks())
		// 'up' starts the gateway before it writes the project state, so a
		// missing file at startup must not end supervision for good.
		if err := sv.tick(context.Background()); err == nil || !strings.Contains(err.Error(), errProjectGone.Error()) {
			t.Fatalf("tick error = %v, want a project-gone error", err)
		}
		if sv.sawProject {
			t.Error("the project was never read; sawProject must stay false")
		}
		w.projectErr = nil
		if err := sv.tick(context.Background()); err != nil {
			t.Fatalf("tick with state present = %v", err)
		}
		if !sv.sawProject {
			t.Fatal("a successful read must latch sawProject")
		}
		// Once it has been seen, its disappearance is 'down' having finished.
		w.projectErr = os.ErrNotExist
		fast := newSupervisor(time.Millisecond, w.hooks())
		fast.sawProject = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { fast.run(ctx); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("run kept polling a project that is gone")
		}
	})

	// The teardown gate against real on-disk state: 'pauseSupervision' is what
	// 'down' and 'up's unwind call before they stop anything, and the
	// supervisor reads that same file.
	t.Run("pauseSupervision closes the teardown race on disk", func(t *testing.T) {
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		proj := &composeProject{
			Name:      "proj",
			Order:     []string{"web"},
			Services:  map[string]string{"web": "proj-web"},
			IPs:       map[string]string{"web": "10.87.0.10"},
			Supervise: true,
		}
		if err := saveProject(s, proj); err != nil {
			t.Fatal(err)
		}
		if !supervisionActive(s, "proj") {
			t.Fatal("a project that just came up must be supervised")
		}

		var restarts int
		hooks := supervisorHooks{
			now:         time.Now,
			loadProject: func() (*composeProject, error) { return loadProject(s, "proj") },
			targets: func(*composeProject) ([]supervisedService, error) {
				return []supervisedService{supervised("web", restartPolicy{Mode: restartAlways, Declared: true})}, nil
			},
			alive:   func(string) bool { return false },
			restart: func(supervisedService) error { restarts++; return nil },
			warnf:   func(string, ...any) {},
		}
		sv := newSupervisor(supervisorPollInterval, hooks)
		if err := sv.tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if restarts != 1 {
			t.Fatalf("a dead service of a live project must be restarted, got %d", restarts)
		}

		// What 'down' does before it stops the first service. The flag has to
		// be on disk: the supervisor is another process.
		reloaded, err := loadProject(s, "proj")
		if err != nil {
			t.Fatal(err)
		}
		pauseSupervision(s, reloaded, io.Discard)
		if supervisionActive(s, "proj") {
			t.Fatal("pauseSupervision did not persist the pause")
		}
		before := restarts
		for i := 0; i < 20; i++ {
			if err := sv.tick(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if restarts != before {
			t.Errorf("%d restart(s) raced the teardown", restarts-before)
		}
		// And once 'down' has removed the state entirely there is nothing to
		// restart and nothing to re-check against.
		if err := os.Remove(projectStatePath(s, "proj")); err != nil {
			t.Fatal(err)
		}
		if supervisionActive(s, "proj") {
			t.Error("a removed project must not report itself supervised")
		}
		if err := sv.tick(context.Background()); err == nil || !strings.Contains(err.Error(), errProjectGone.Error()) {
			t.Errorf("tick error = %v, want a project-gone error", err)
		}
	})
}

// TestSupervisorRestartArgvMatchesUp is the anti-drift guard for the restart
// path: a restarted service must be launched with exactly the argv 'up' first
// gave it, rebuilt from the persisted project state alone.
func TestSupervisorRestartArgvMatchesUp(t *testing.T) {
	src := `
services:
  web:
    image: img:1
    command: ["/app", "--serve"]
    mem_limit: 512m
    cpus: 2
    environment:
      B: 2
      A: 1
    volumes:
      - /srv/data:/data
      - vol:/var/lib/x
    restart: always
  db:
    image: img:2
    container_name: thedb
volumes:
  vol: {}
`
	p, _, err := loadTestProject(t, src)
	if err != nil {
		t.Fatal(err)
	}
	path := p.ComposeFiles[0]
	const project, instance, sock, volRoot = "proj", "proj-web", "/tmp/gw.sock", "/store/volumes"
	order := []string{"web", "db"}
	ips := map[string]string{"web": "10.87.0.10", "db": "10.87.0.11"}

	// What 'up' builds, from the live invocation.
	upArgs, err := serviceRunArgs("web", instance, p.Services["web"], serviceLaunch{
		project:     project,
		gatewaySock: sock,
		maskBits:    24,
		ip:          ips["web"],
		hostEntries: projectHostEntries(order, ips, project, p.Services),
		volumesRoot: volRoot,
	})
	if err != nil {
		t.Fatal(err)
	}

	// What the supervisor rebuilds, from the state file alone.
	proj := &composeProject{
		Name:       project,
		File:       path,
		Order:      order,
		Services:   map[string]string{"web": instance, "db": "thedb"},
		IPs:        ips,
		SwitchSock: sock,
		Subnet:     "10.87.0.0/24",
		Supervise:  true,
	}
	ps := &projectSupervisor{project: project, volRoot: volRoot, confirmed: map[string]confirmedPID{}}
	supArgs, err := serviceRunArgs("web", instance, p.Services["web"], ps.launch(proj, p, "web"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(upArgs, supArgs) {
		t.Errorf("a restart would launch a different VM than up did:\nup:         %v\nsupervisor: %v", upArgs, supArgs)
	}
	// The container_name of a sibling must be in the hosts list, or a
	// restarted service would resolve fewer names than its siblings.
	mustContain(t, strings.Join(supArgs, " "), "thedb:10.87.0.11", "instance names must be resolvable")
	// A job is never restarted, so the restart path never asks for the
	// benign-init form.
	mustContain(t, strings.Join(supArgs, " "), "-- img:1 /app --serve", "the service's own command must be launched")

	t.Run("the subnet prefix comes from the project state", func(t *testing.T) {
		if got := (&composeProject{Subnet: "10.9.0.0/16"}).maskBits(); got != 16 {
			t.Errorf("maskBits = %d, want 16", got)
		}
		// State written before the subnet was recorded, and unparseable
		// state, both fall back to the /24 'up --subnet' defaults to.
		for _, subnet := range []string{"", "bogus"} {
			if got := (&composeProject{Subnet: subnet}).maskBits(); got != 24 {
				t.Errorf("maskBits for %q = %d, want the /24 default", subnet, got)
			}
		}
	})
}

// supervised is a poll target for a service whose instance is named
// "<service>-instance".
func supervised(name string, p restartPolicy) supervisedService {
	return supervisedService{name: name, instance: name + "-instance", policy: p}
}

// fakeSupervisorWorld is the injected world the supervision loop acts on: a
// clock that only the test moves, a project state, and counters instead of
// VMs.
type fakeSupervisorWorld struct {
	clock      time.Time
	project    *composeProject
	projectErr error
	targets    []supervisedService
	alive      map[string]bool
	restarts   []string
	restartAt  []time.Time
	warnings   []string
}

func newFakeWorld(targets ...supervisedService) *fakeSupervisorWorld {
	services := map[string]string{}
	var order []string
	for _, t := range targets {
		order = append(order, t.name)
		services[t.name] = t.instance
	}
	return &fakeSupervisorWorld{
		// A fixed, non-zero start: the loop must not depend on wall time.
		clock:   time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		project: &composeProject{Name: "proj", Order: order, Services: services, Supervise: true},
		targets: targets,
		alive:   map[string]bool{},
	}
}

func (w *fakeSupervisorWorld) hooks() supervisorHooks {
	return supervisorHooks{
		now: func() time.Time { return w.clock },
		loadProject: func() (*composeProject, error) {
			if w.projectErr != nil {
				return nil, w.projectErr
			}
			return w.project, nil
		},
		targets: func(*composeProject) ([]supervisedService, error) { return w.targets, nil },
		alive:   func(instance string) bool { return w.alive[instance] },
		restart: func(t supervisedService) error {
			w.restarts = append(w.restarts, t.name)
			w.restartAt = append(w.restartAt, w.clock)
			return nil
		},
		warnf: func(format string, args ...any) {
			w.warnings = append(w.warnings, fmt.Sprintf(format, args...))
		},
	}
}

// drive advances the fake clock one poll interval at a time and ticks the
// supervisor, the way the real ticker would.
func (w *fakeSupervisorWorld) drive(sv *supervisor, ticks int) {
	for i := 0; i < ticks; i++ {
		w.clock = w.clock.Add(sv.interval)
		if err := sv.tick(context.Background()); err != nil {
			return
		}
	}
}

// TestRestartRefusesDeliberateStop exercises the guard itself: restart()
// must return before it clears the instance record, so the record
// surviving is the proof that nothing was re-run. Deleting the guard from
// restart() must fail this test.
func TestRestartRefusesDeliberateStop(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const instance = "proj-web"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "stopped", StoppedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Supervision is deliberately ACTIVE and the service is declared with a
	// restart policy: the deliberate stop is the only reason to decline.
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(
		"services:\n  web:\n    image: img:1\n    restart: always\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveProject(s, &composeProject{
		Name:      "proj",
		File:      composePath,
		Order:     []string{"web"},
		Services:  map[string]string{"web": instance},
		Supervise: true,
	}); err != nil {
		t.Fatal(err)
	}

	ps := &projectSupervisor{
		store:     s,
		project:   "proj",
		confirmed: map[string]confirmedPID{},
	}
	if err := ps.restart(supervisedService{name: "web", instance: instance}); err != nil {
		t.Fatalf("restart must decline quietly, got %v", err)
	}
	// clearInstance is the first effect of a real restart; the record still
	// being there is what proves the guard returned first.
	if _, err := s.GetInstance(instance); err != nil {
		t.Errorf("a deliberately stopped service must not be re-run: its record was cleared (%v)", err)
	}
}

// TestAliveRevalidatesExpiredPID drives alive() through the cache: a fresh
// verification is trusted, an expired one is re-checked against the real
// process argv and fails, which is what keeps a recycled pid from reading
// as alive forever. Dropping the TTL from alive() must fail this test.
func TestAliveRevalidatesExpiredPID(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const instance = "proj-web"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	// This test binary is alive to Kill(pid, 0) but its argv is neither
	// vz-runner nor qemu-system, so vmmProcessMatches must reject it.
	pid := os.Getpid()
	if err := s.SaveInstance(&store.InstanceState{ID: instance, Status: "running", PID: pid}); err != nil {
		t.Fatal(err)
	}
	ps := &projectSupervisor{store: s, project: "proj", confirmed: map[string]confirmedPID{}}

	ps.confirmed[instance] = confirmedPID{pid: pid, at: time.Now()}
	if !ps.alive(instance) {
		t.Error("a fresh verification must be trusted without re-running the argv check")
	}

	ps.confirmed[instance] = confirmedPID{pid: pid, at: time.Now().Add(-2 * confirmTTL)}
	if ps.alive(instance) {
		t.Error("an expired verification must be re-checked; a recycled pid must not read as alive")
	}
	if _, still := ps.confirmed[instance]; still {
		t.Error("a failed re-check must drop the cache entry")
	}
}

// TestMarkStoppedRecordsIntent pins the contract every stop path relies on:
// stopping records that a person asked for it, which is what stops the
// supervisor from undoing the stop within a poll. Every path in stop.go
// funnels through markStopped precisely so this cannot be true on some
// paths and not others.
func TestMarkStoppedRecordsIntent(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id       string
		clearPID bool
	}{
		{"kept-pid", false},
		{"cleared-pid", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if _, err := s.CreateInstance(tc.id); err != nil {
				t.Fatal(err)
			}
			markStopped(s, &store.InstanceState{ID: tc.id, Status: "running", PID: 4242}, tc.clearPID)
			got, err := s.GetInstance(tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "stopped" {
				t.Errorf("status = %q, want stopped", got.Status)
			}
			if !got.StoppedByUser {
				t.Error("a stop must record that it was deliberate, or the supervisor undoes it")
			}
			if got.ExitedAt.IsZero() {
				t.Error("a stop must record when the instance ended")
			}
			if tc.clearPID && got.PID != 0 {
				t.Errorf("PID = %d, want cleared", got.PID)
			}
		})
	}
}

// TestStopInstanceMarksIntentOnTheSignalPath drives the real stop path a
// running VM takes: a live process whose argv passes vmmProcessMatches, so
// stopInstance signals it rather than taking one of the already-dead
// shortcuts. It exists because that path once saved a record WITHOUT the
// deliberate-stop marker, and the supervisor then undid the stop within a
// poll; a structural funnel is only a guarantee while something drives it.
func TestStopInstanceMarksIntentOnTheSignalPath(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// vmmProcessMatches looks for "qemu-system" in the argv, so give a
	// harmless sleeper that name.
	// A script rather than a copied binary: macOS code signing kills a
	// copy of a system binary, and ps reports the script path, which is
	// what vmmProcessMatches reads.
	fake := filepath.Join(dir, "qemu-system-fake")
	if err := os.WriteFile(fake, []byte(
		"#!/bin/sh\n"+
			"# Dies on SIGTERM like a real VMM, and stays as the process ps\n"+
			"# reports so vmmProcessMatches keeps seeing this script's name.\n"+
			"trap 'exit 0' TERM\nsleep 60 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := exec.Command("/bin/sh", fake)
	if err := proc.Start(); err != nil {
		t.Skipf("cannot start the fake VMM: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if !vmmProcessMatches(proc.Process.Pid) {
		t.Skip("the staged process is not recognized as a VMM on this host")
	}

	const instance = "signal-path"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "running", PID: proc.Process.Pid,
	}); err != nil {
		t.Fatal(err)
	}

	if err := stopInstanceIn(s, instance, 2); err != nil {
		t.Fatalf("stopInstanceIn: %v", err)
	}
	got, err := s.GetInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if !got.StoppedByUser {
		t.Error("the signal path must record a deliberate stop, or the supervisor restarts what the user stopped")
	}
}

// TestStopMarksIntentBeforeSignalling pins the fix for the window review
// found: the marker has to be on disk before the first signal, because the
// gap between signalling and confirming death is a whole poll interval wide
// and the supervisor restarts anything it sees dead without a marker.
func TestStopMarksIntentBeforeSignalling(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "qemu-system-fake")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := exec.Command("/bin/sh", fake)
	if err := proc.Start(); err != nil {
		t.Skipf("cannot start the fake VMM: %v", err)
	}
	defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()
	if !vmmProcessMatches(proc.Process.Pid) {
		t.Skip("the staged process is not recognized as a VMM on this host")
	}
	const instance = "intent-first"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "running", PID: proc.Process.Pid,
	}); err != nil {
		t.Fatal(err)
	}

	// Watch the record while the stop runs: the marker must appear while the
	// instance still reads "running", i.e. before death is confirmed.
	seenEarly := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := s.GetInstance(instance); err == nil && st.StoppedByUser && st.Status == "running" {
				seenEarly <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		seenEarly <- false
	}()
	if err := stopInstanceIn(s, instance, 2); err != nil {
		t.Fatalf("stopInstanceIn: %v", err)
	}
	if !<-seenEarly {
		t.Error("the deliberate-stop marker must be recorded before the VM is confirmed dead")
	}
}

// TestClearIntentOnUnfinishedStop pins the compensator: nothing else in the
// codebase clears the marker, so a stop that records intent and then fails
// must not leave the service permanently exempt from its restart policy.
func TestClearIntentOnUnfinishedStop(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const instance = "unfinished"
	if _, err := s.CreateInstance(instance); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "running", PID: 424242, StoppedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	clearIntent(s, instance)
	got, err := s.GetInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoppedByUser {
		t.Error("an unfinished stop must not leave the marker behind")
	}

	// A completed stop keeps it: the instance really is stopped on purpose.
	if err := s.SaveInstance(&store.InstanceState{
		ID: instance, Status: "stopped", StoppedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	clearIntent(s, instance)
	got, _ = s.GetInstance(instance)
	if !got.StoppedByUser {
		t.Error("a completed stop must keep its marker")
	}
}

// TestSaveProjectIsAtomic pins the fix for the torn read that used to kill
// supervision for a project's whole life: 'up' saves after every service
// start while the daemon polls, so a reader must never see a partial file.
func TestSaveProjectIsAtomic(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := &composeProject{
		Name: "atomic", File: "/tmp/compose.yaml",
		Order: []string{"a"}, Services: map[string]string{"a": "atomic-a"},
		Supervise: true,
	}
	stop := make(chan struct{})
	bad := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				bad <- nil
				return
			default:
				if _, err := loadProject(s, "atomic"); err != nil && !os.IsNotExist(err) {
					bad <- err
					return
				}
			}
		}
	}()
	for i := 0; i < 200; i++ {
		proj.Services[fmt.Sprintf("svc%d", i)] = fmt.Sprintf("atomic-svc%d", i)
		if err := saveProject(s, proj); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-bad; err != nil {
		t.Errorf("a concurrent reader saw a torn project state: %v", err)
	}
}
