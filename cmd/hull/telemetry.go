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

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/hull/internal/telemetry"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// telemetryClient is initialized in the root Before hook and used by the
// event call sites. Nil-safe: all methods no-op on nil.
var telemetryClient *telemetry.Client

// telemetryCmdName is the top-level subcommand of this invocation, for
// the command event.
var telemetryCmdName string

// topLevelCommand scans os.Args for the first non-flag token, skipping
// the one global flag that takes a separate value.
func topLevelCommand() string {
	skipNext := false
	for _, arg := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if arg == "--store-dir" || arg == "-store-dir" {
				skipNext = true
			}
			continue
		}
		return arg
	}
	return ""
}

// initTelemetry runs the consent flow. Invoked from the root
// Before hook, except for the telemetry subcommand itself (prompting for
// consent on the way to `telemetry off` would be absurd, so that path
// manages state directly) and the network-gateway daemon (a supervised
// child; its parent's command already counts).
func initTelemetry(cmd *cli.Command) {
	telemetryCmdName = topLevelCommand()
	if telemetryCmdName == "telemetry" || telemetryCmdName == "network-gateway" {
		return
	}
	telemetryClient = telemetry.Init(telemetry.Config{
		StoreDir:  cmd.String("store-dir"),
		Version:   version,
		OSVersion: osProductVersion(),
		Uname:     unameString(),
		// CI counts as non-interactive (the conventional CI env var,
		// set by GitHub runners and most others): never prompt there,
		// even when the harness allocates a pty.
		Interactive: os.Getenv("CI") == "" && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())),
		Unattended:  cmd.Bool("unattended"),
		DNT:         cmd.Bool("dnt"),
		Stdin:       os.Stdin,
		Stderr:      os.Stderr,
	})
	if telemetryClient.Enabled() {
		log.Debug("telemetry enabled")
	}
	// Crashes are uploaded in the background on the invocation after
	// they happen, so the report survives even when the crash killed
	// networking or skipped every defer -- and init never waits on it.
	telemetryClient.UploadPendingCrashesAsync()
}

// telemetryBackend is set at backend resolution in run.go so a crash
// report can say which VMM was in play.
var telemetryBackend string

// handlePanic is deferred first thing in main: it queues a crash report
// (panic type and scrubbed stack only), then preserves the familiar
// crash behavior -- stack on stderr, exit 2. Only main-goroutine panics
// arrive here; a panic on another goroutine still crashes uncaught.
func handlePanic(recovered any, stack []byte) {
	telemetryClient.CapturePanic(recovered, stack, telemetryCmdName, telemetryBackend)
	fmt.Fprintf(os.Stderr, "panic: %v\n\n%s", recovered, stack)
	os.Exit(2)
}

// sendCommandEvent reports how this invocation ended. Called from the
// two exit funnels in main.go and from the exec frame loop, whose bare
// os.Exit skips both. Delivery is asynchronous; the exit path grants a
// bounded grace here so a short-lived invocation (a detached `run`)
// does not exit before its events -- including the earlier start event
// -- reach a slow, CDN-fronted endpoint. Flush returns the instant
// delivery completes, so a reachable endpoint adds no perceptible
// delay.
func sendCommandEvent(outcome, errClass string) {
	if telemetryCmdName == "" {
		return
	}
	fields := map[string]string{
		"command": telemetryCmdName,
		"outcome": outcome,
	}
	if errClass != "" {
		fields["error_class"] = errClass
	}
	telemetryClient.Send("command", fields)
	telemetryClient.Flush(telemetry.FlushTimeout)
}

// errorClass buckets a command failure into the coarse, stable classes
// the schema documents -- Go error semantics only, never message text.
func errorClass(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, os.ErrNotExist):
		return "not-found"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case errors.As(err, &netErr):
		return "network"
	default:
		return "other"
	}
}

// telemetryBackendSource records how the backend was chosen (flag,
// annotation or default), set at resolution in run.go.
var telemetryBackendSource string

// sendStartEvent reports a VMM launch attempt: which backend, how it
// was chosen, and whether the process started.
func sendStartEvent(backend string, started bool) {
	boot := "ok"
	if !started {
		boot = "fail"
	}
	telemetryClient.Send("start", map[string]string{
		"backend":        backend,
		"backend_source": telemetryBackendSource,
		"boot":           boot,
	})
}

// sendEndEvent reports the instance lifetime.
func sendEndEvent(backend string, lifetime time.Duration) {
	telemetryClient.Send("end", map[string]string{
		"backend":    backend,
		"duration_s": strconv.FormatInt(int64(lifetime.Seconds()), 10),
	})
}

// sendEndOnce emits the `end` event for an instance exactly once. The
// guard is persisted in InstanceState, so the foreground exit path and
// a `stop` (or a retried stop) never double-report the same VM.
func sendEndOnce(s *store.Store, state *store.InstanceState) {
	if state == nil || state.TelemetryEndSent {
		return
	}
	state.TelemetryEndSent = true
	_ = s.SaveInstance(state)
	end := state.ExitedAt
	if end.IsZero() {
		end = time.Now()
	}
	var dur time.Duration
	if !state.StartTime.IsZero() {
		dur = end.Sub(state.StartTime)
	}
	sendEndEvent(state.Backend, dur)
}

// Metrics sampling cadence. A rate needs two readings, so the first tick
// only takes a baseline; both of the first two come from the short warm-up
// rather than the full interval. A VM or an exec session that ended inside
// thirty seconds used to emit nothing at all, which biased everything we
// collected towards long-lived, mostly idle VMs.
//
// Variables rather than constants so a test can drive the sampler through
// both ticks without spending thirty-five seconds doing it.
var (
	metricsInterval       = 30 * time.Second
	metricsWarmupInterval = 5 * time.Second
)

// vmmCPUSample is one reading of a VMM process' cumulative CPU time, with
// the clock at the moment it was taken and the PID it came from.
//
// Utilization is the difference between two of these over the real time
// that passed. A single reading is not one: hull used to forward `ps -o
// %cpu` as `cpu_pct`, and that column is a decaying average over up to a
// minute, summed over the process' threads -- so four busy vCPUs read
// about 400, not 100, over a window that overlapped the next sample's.
type vmmCPUSample struct {
	pid  int
	cpu  time.Duration
	when time.Time
}

// sampleVMMProcess reads one process' RSS and cumulative CPU time.
func sampleVMMProcess(pid int) (rssKB string, cpu time.Duration, err error) {
	out, err := exec.Command("ps", "-o", "rss=,time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("ps: short output for pid %d", pid)
	}
	cpu, err = parsePSCPUTime(fields[1])
	if err != nil {
		return "", 0, err
	}
	return fields[0], cpu, nil
}

// parsePSCPUTime parses the `ps -o time=` column: CPU time accumulated
// across a process' threads, written as [minutes:]seconds.hundredths.
//
// The minutes field is unbounded -- Darwin never rolls it up into hours,
// so a VM with 41 hours on the clock reads "2463:28.96", not "41:03:28.96".
// (The day-and-hour shape belongs to `etime`, which is elapsed real time,
// not CPU time.) Reading from the last colon keeps this correct whatever
// the magnitude, and the hundredths survive at every size.
func parsePSCPUTime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	secs := s
	var mins float64
	if i := strings.LastIndex(s, ":"); i >= 0 {
		m, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: %w", s, err)
		}
		mins, secs = m, s[i+1:]
	}
	sec, err := strconv.ParseFloat(secs, 64)
	if err != nil {
		return 0, fmt.Errorf("ps time %q: %w", s, err)
	}
	return time.Duration((mins*60 + sec) * float64(time.Second)), nil
}

// cpuPercent turns two readings into the share of the guest's vCPUs that
// were busy between them, so 100 means every vCPU fully busy for the whole
// interval. The bool reports whether the two are comparable at all.
//
// Each rejection has a real path to it. Differing PIDs mean the vz helper
// cycled or a PID was reused, so the readings belong to two processes and
// their difference is meaningless (and frequently negative). An unknown
// vCPU count must not divide -- in Go that produces +Inf, and since
// cpu_pct ships as a string, "+Inf" would land in the data unremarked. A
// negative delta means the same thing a PID change does, arrived at
// without our noticing. None of these could happen while the sampler
// forwarded a string and did no arithmetic at all.
func cpuPercent(prev, cur vmmCPUSample, vcpus int) (float64, bool) {
	if prev.pid == 0 || prev.pid != cur.pid || vcpus <= 0 {
		return 0, false
	}
	elapsed := cur.when.Sub(prev.when).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	pct := (cur.cpu - prev.cpu).Seconds() / elapsed * 100 / float64(vcpus)
	if pct < 0 {
		return 0, false
	}
	return pct, true
}

// vcpusFromCmdLine reads the guest's vCPU count out of the VMM argv.
//
// The count is not a field on the instance record, but the argv that
// launched the VM is, and every backend carries the number in it: qemu
// takes `-smp`, vz and hvi take `--cpus`. Reading it from there rather
// than adding a field means an instance that was already running when hull
// was upgraded still normalises correctly, and it keeps the telemetry
// event's field list unchanged -- widening that list is what re-opens the
// consent question, and a vCPU count is not worth re-asking the world.
func vcpusFromCmdLine(argv []string) int {
	for i, a := range argv {
		var v string
		switch {
		case a == "-smp" || a == "--cpus":
			if i+1 < len(argv) {
				v = argv[i+1]
			}
		case strings.HasPrefix(a, "--cpus="):
			v = strings.TrimPrefix(a, "--cpus=")
		case strings.HasPrefix(a, "-smp="):
			v = strings.TrimPrefix(a, "-smp=")
		default:
			continue
		}
		if n, ok := parseCPUCount(v); ok {
			return n
		}
	}
	return 0
}

// parseCPUCount reads a vCPU count from a flag value. urunc always writes a
// bare integer, but qemu's own -smp accepts `cpus=4,sockets=1` too, so read
// that shape rather than silently taking zero from it.
func parseCPUCount(v string) (int, bool) {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n, true
	}
	for _, part := range strings.Split(v, ",") {
		if after, ok := strings.CutPrefix(part, "cpus="); ok {
			if n, err := strconv.Atoi(after); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// startVMMMetricsSampler samples the VMM process while a CLI is attached to
// it -- the foreground `run`, or an `exec` session on a detached VM (which
// is how a wrapped sandbox gets sampled while an agent session is active).
// startTime is the VM's launch time, so uptime is the VM's and not the
// sampler's; vcpus normalises the CPU reading and comes from the argv. A
// non-blocking per-instance flock keeps it to one sampler per VM, so a
// second attach never double-counts. Sampling stops when done closes, when
// the flock is dropped with the process, or when the VMM PID disappears.
func startVMMMetricsSampler(launcherPID int, backend string, vcpus int, startTime time.Time, instanceDir string, done <-chan struct{}) {
	if !telemetryClient.Enabled() {
		return
	}
	lock, err := os.OpenFile(filepath.Join(instanceDir, ".metrics.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		// Another attach already samples this instance.
		_ = lock.Close()
		return
	}
	go func() {
		defer func() {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			_ = lock.Close()
		}()
		// The warm-up also covers a vz helper that is not resolvable the
		// instant we attach: we retry every few seconds rather than every
		// thirty, and only settle to the full interval once a sample has
		// actually gone out.
		//
		// Both are read once here rather than per tick, so the goroutine
		// never races a test that is restoring them.
		warmup, steady := metricsWarmupInterval, metricsInterval
		interval := warmup
		timer := time.NewTimer(interval)
		defer timer.Stop()
		var prev vmmCPUSample
		vzHelper := 0

		// tick takes one sample and reports whether sampling continues. It
		// is a closure rather than the body of the select so that the timer
		// is rearmed exactly once per tick, after interval may have changed:
		// rearming before that kept the sampler on the warm-up cadence for
		// one extra sample, and rearming after an early `continue` would
		// have left the timer dead and the sampler silent.
		tick := func() bool {
			// Which PID actually is the VM? qemu and hvi run the guest
			// in-process, so the launcher is it. vz (Apple
			// Virtualization.framework) runs the guest in a separate
			// launchd-owned XPC helper -- the launcher is a thin wrapper
			// reading ~0% CPU -- so find the helper holding this
			// instance's files open and sample that.
			pid := launcherPID
			if backend == "vz" {
				if vzHelper == 0 || !pidAlive(vzHelper) {
					vzHelper = resolveVzHelper(instanceDir)
				}
				if vzHelper == 0 {
					return pidAlive(launcherPID) // helper not up yet, or VM gone
				}
				pid = vzHelper
			}
			rssKB, cpu, err := sampleVMMProcess(pid)
			if err != nil {
				if backend == "vz" {
					vzHelper = 0 // helper cycled; re-resolve next tick
				}
				// One ps failure is not the end of the VM. Only a PID that
				// is actually gone ends the sampler: qemu and hvi used to
				// return here and stop sampling for the rest of the attach,
				// where vz alone recovered.
				return pidAlive(launcherPID)
			}
			cur := vmmCPUSample{pid: pid, cpu: cpu, when: time.Now()}
			pct, ok := cpuPercent(prev, cur, vcpus)
			prev = cur
			if !ok {
				return true // baseline taken, or the two are not comparable
			}
			telemetryClient.Send("metrics", map[string]string{
				"backend":  backend,
				"rss_kb":   rssKB,
				"cpu_pct":  strconv.FormatFloat(pct, 'f', 1, 64),
				"uptime_s": strconv.FormatInt(int64(time.Since(startTime).Seconds()), 10),
			})
			interval = steady
			return true
		}

		for {
			select {
			case <-done:
				return
			case <-timer.C:
				if !tick() {
					return
				}
				timer.Reset(interval)
			}
		}
	}()
}

// resolveVzHelper finds the com.apple.Virtualization.VirtualMachine XPC
// helper process that runs this instance's guest -- the one holding the
// instance's files open. Returns 0 if none is found (VM not up yet, or
// gone). Correlating on the instance dir is what keeps concurrent vz VMs
// mapped to the right helper, since the helpers are all reparented to
// launchd and cannot be told apart by ppid.
func resolveVzHelper(instanceDir string) int {
	out, err := exec.Command("pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(out)) {
		pid, perr := strconv.Atoi(f)
		if perr != nil || pid <= 0 {
			continue
		}
		files, ferr := exec.Command("lsof", "-p", f, "-Fn").Output()
		if ferr != nil {
			continue
		}
		if strings.Contains(string(files), instanceDir) {
			return pid
		}
	}
	return 0
}

// pidAlive reports whether pid is a live process (signal 0 probe).
func pidAlive(pid int) bool {
	return pid > 0 && unix.Kill(pid, 0) == nil
}

// osProductVersion returns the macOS version (major.minor) for the event
// envelope; empty on failure, never an error.
func osProductVersion() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}

// unameString renders the full uname for the event envelope, explicitly
// excluding the hostname (Nodename).
func unameString() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	b := func(f []byte) string { return unix.ByteSliceToString(f) }
	return strings.Join([]string{
		b(u.Sysname[:]), b(u.Release[:]), b(u.Version[:]), b(u.Machine[:]),
	}, " ")
}

func telemetryCommand() *cli.Command {
	return &cli.Command{
		Name:  "telemetry",
		Usage: "control anonymous usage and crash telemetry (see docs/telemetry.md)",
		Commands: []*cli.Command{
			{
				Name:  "on",
				Usage: "enable telemetry",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := telemetry.SetConsent(cmd.String("store-dir"), true); err != nil {
						return fmt.Errorf("failed to record telemetry consent: %w", err)
					}
					fmt.Println("telemetry enabled")
					return nil
				},
			},
			{
				Name:  "off",
				Usage: "disable telemetry",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := telemetry.SetConsent(cmd.String("store-dir"), false); err != nil {
						return fmt.Errorf("failed to record telemetry opt-out: %w", err)
					}
					fmt.Println("telemetry disabled")
					return nil
				},
			},
			{
				Name:  "status",
				Usage: "show the effective telemetry state",
				Action: func(_ context.Context, cmd *cli.Command) error {
					fmt.Printf("telemetry: %s\n", telemetry.Status(cmd.String("store-dir")))
					return nil
				},
			},
		},
	}
}
