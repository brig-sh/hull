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
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/telemetry"
	"golang.org/x/sys/unix"
)

// TestParsePSCPUTimeReadsDarwinsUnboundedMinutes pins the one format
// assumption the whole measurement rests on.
//
// `ps -o time=` writes CPU time as minutes:seconds.hundredths and never
// rolls the minutes up into hours: a VM with 41 hours on the clock reads
// "2463:28.96". The day-and-hour shape ("17-01:11:39") belongs to `etime`,
// which is elapsed real time, and reading one as the other silently
// divides the number by sixty.
func TestParsePSCPUTimeReadsDarwinsUnboundedMinutes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"0:00.00", 0},
		{"0:03.10", 3100 * time.Millisecond},
		{"1:03.10", 63100 * time.Millisecond},
		{"  2463:28.96  ", 2463*time.Minute + 28960*time.Millisecond},
		{"45.50", 45500 * time.Millisecond},
	} {
		got, err := parsePSCPUTime(tc.in)
		if err != nil {
			t.Errorf("parsePSCPUTime(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parsePSCPUTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "nonsense", "x:03.10", "0:zz"} {
		if _, err := parsePSCPUTime(bad); err == nil {
			t.Errorf("parsePSCPUTime(%q) must not pass a value it cannot read", bad)
		}
	}
}

// TestCPUPercentNeverShipsAValueItCannotStandBehind covers the states a
// stateful sampler has and a stateless one could not.
//
// The old sampler forwarded a string and did no arithmetic, so none of
// these could arise. Now a missing vCPU count divides by zero (Go yields
// +Inf, and since cpu_pct ships as a string, "+Inf" would land in the data
// unremarked), and readings from two different processes -- a reused PID,
// or a vz helper that cycled -- subtract to something negative.
func TestCPUPercentNeverShipsAValueItCannotStandBehind(t *testing.T) {
	base := time.Now()
	at := func(pid int, cpu time.Duration, offset time.Duration) vmmCPUSample {
		return vmmCPUSample{pid: pid, cpu: cpu, when: base.Add(offset)}
	}

	// Four vCPUs, all busy for the whole interval: 30s of wall clock buys
	// 120s of CPU, and that is what 100 means.
	prev, cur := at(42, 10*time.Second, 0), at(42, 130*time.Second, 30*time.Second)
	if pct, ok := cpuPercent(prev, cur, 4); !ok || math.Abs(pct-100) > 0.01 {
		t.Errorf("all four vCPUs busy = %v (ok=%v), want 100", pct, ok)
	}
	// Half of them: the number the old code would have reported here is
	// 200, because ps sums over threads.
	cur = at(42, 70*time.Second, 30*time.Second)
	if pct, ok := cpuPercent(prev, cur, 4); !ok || math.Abs(pct-50) > 0.01 {
		t.Errorf("two of four vCPUs busy = %v (ok=%v), want 50", pct, ok)
	}
	// An idle VM reads 0, not the ~60s of history ps would still be
	// averaging over.
	cur = at(42, 10*time.Second, 30*time.Second)
	if pct, ok := cpuPercent(prev, cur, 4); !ok || pct != 0 {
		t.Errorf("idle VM = %v (ok=%v), want 0", pct, ok)
	}

	for _, tc := range []struct {
		name      string
		prev, cur vmmCPUSample
		vcpus     int
	}{
		{"no baseline yet", vmmCPUSample{}, at(42, 10*time.Second, 0), 4},
		{"vz helper cycled", at(42, 10*time.Second, 0), at(43, 1*time.Second, 30*time.Second), 4},
		{"unknown vCPU count", prev, at(42, 130*time.Second, 30*time.Second), 0},
		{"negative vCPU count", prev, at(42, 130*time.Second, 30*time.Second), -1},
		{"CPU time went backwards", at(42, 10*time.Second, 0), at(42, 1*time.Second, 30*time.Second), 4},
		{"no time passed", at(42, 10*time.Second, 0), at(42, 10*time.Second, 0), 4},
	} {
		pct, ok := cpuPercent(tc.prev, tc.cur, tc.vcpus)
		if ok {
			t.Errorf("%s: reported %v, want the sample dropped", tc.name, pct)
		}
		if math.IsInf(pct, 0) || math.IsNaN(pct) || pct < 0 {
			t.Errorf("%s: produced %v, which must never reach the payload", tc.name, pct)
		}
	}
}

// TestVcpusFromCmdLineCoversEveryBackend reads the count out of the argv
// each backend actually gets launched with.
func TestVcpusFromCmdLineCoversEveryBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want int
	}{
		{"qemu", []string{"qemu-system-aarch64", "-m", "2048", "-smp", "4", "-nographic"}, 4},
		{"vz", []string{"vz-runner", "--mem", "2048", "--cpus", "2"}, 2},
		{"hvi", []string{"hvi", "boot", "--kernel", "/k", "--cpus", "8"}, 8},
		{"joined form", []string{"vz-runner", "--cpus=3"}, 3},
		{"qemu cpus= key", []string{"qemu", "-smp", "cpus=6,sockets=1"}, 6},
		{"absent", []string{"qemu-system-aarch64", "-m", "2048"}, 0},
		{"flag with no value", []string{"vz-runner", "--cpus"}, 0},
		{"unparseable", []string{"vz-runner", "--cpus", "many"}, 0},
		{"zero is not a count", []string{"vz-runner", "--cpus", "0"}, 0},
		{"empty", nil, 0},
	} {
		if got := vcpusFromCmdLine(tc.argv); got != tc.want {
			t.Errorf("%s: vcpusFromCmdLine(%v) = %d, want %d", tc.name, tc.argv, got, tc.want)
		}
	}
}

// TestMetricsEventCarriesAUtilizationNotPsPercent drives the real sampler
// against a stand-in `ps` and reads the event off the wire.
//
// The stand-in reports CPU time growing at two cores' worth of real time.
// With a four-vCPU guest that is 50, whatever the tick actually took --
// which is the point of measuring a delta rather than forwarding a column
// whose window is fixed at about a minute.
func TestMetricsEventCarriesAUtilizationNotPsPercent(t *testing.T) {
	fakePS(t, 2.0)

	var mu sync.Mutex
	var events []map[string]any
	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		var e map[string]any
		if json.Unmarshal(buf.Bytes(), &e) == nil {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
			select {
			case got <- struct{}{}:
			default:
			}
		}
	}))
	defer srv.Close()

	withFastSampler(t, 500*time.Millisecond)
	withTelemetryClient(t, srv.URL)

	done := make(chan struct{})
	defer close(done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 4, time.Now(), t.TempDir(), done)

	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("sampler emitted no metrics event")
	}
	telemetryClient.Flush(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	e := events[0]
	if e["event"] != "metrics" {
		t.Fatalf("event = %v, want metrics", e["event"])
	}
	raw, _ := e["cpu_pct"].(string)
	pct, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("cpu_pct %q does not parse as a number: %v", raw, err)
	}
	// Two cores of four. The old sampler would have reported ps' own
	// column here, which for a process this busy reads about 200.
	if pct < 40 || pct > 60 {
		t.Errorf("cpu_pct = %v, want ~50 (two of four vCPUs busy)", pct)
	}
	if e["backend"] != "qemu" {
		t.Errorf("backend = %v, want qemu", e["backend"])
	}
	if rss, _ := e["rss_kb"].(string); rss == "" {
		t.Error("rss_kb must still be reported")
	}
}

// TestSamplerEmitsBeforeAFullIntervalHasPassed pins the bias fix: the
// first event must not wait for a whole tick, or every VM and exec session
// shorter than one reports nothing and the data skews to long-lived, idle
// VMs.
func TestSamplerEmitsBeforeAFullIntervalHasPassed(t *testing.T) {
	fakePS(t, 1.0)

	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	// A warm-up an order of magnitude below the steady interval: an event
	// inside the deadline can only have come from the warm-up.
	metricsIntervalWas, warmupWas := metricsInterval, metricsWarmupInterval
	t.Cleanup(func() { metricsInterval, metricsWarmupInterval = metricsIntervalWas, warmupWas })
	metricsWarmupInterval = 300 * time.Millisecond
	metricsInterval = 60 * time.Second
	withTelemetryClient(t, srv.URL)

	done := make(chan struct{})
	defer close(done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 1, time.Now(), t.TempDir(), done)

	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no event before the steady interval elapsed")
	}
}

// TestSecondAttachDoesNotDoubleCount keeps the per-instance flock honest:
// two samplers on one instance would report the VM twice.
//
// The assertion is on the lock rather than on a count of events in a
// window, because the sampler takes the lock synchronously before starting
// its goroutine -- so the state is settled the moment the second call
// returns, and the test does not depend on how many ticks a loaded machine
// managed to fit into a sleep.
func TestSecondAttachDoesNotDoubleCount(t *testing.T) {
	fakePS(t, 1.0)

	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	withFastSampler(t, 200*time.Millisecond)
	withTelemetryClient(t, srv.URL)

	instanceDir := t.TempDir()
	done := make(chan struct{})
	defer close(done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 1, time.Now(), instanceDir, done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 1, time.Now(), instanceDir, done)

	// Whoever holds this refuses everyone else, which is what keeps a
	// second attach from sampling a VM the first is already reporting.
	lock, err := os.OpenFile(filepath.Join(instanceDir, ".metrics.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Error("instance lock is free: nothing stops a second attach from double-counting")
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	}

	// And the sampler that did win the lock is really running.
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("the attach that took the lock emitted nothing")
	}
}

// withFastSampler shortens both intervals so a test can watch two ticks.
// The interval stays long enough that the hundredths `ps` reports are a
// negligible share of the delta.
func withFastSampler(t *testing.T, d time.Duration) {
	t.Helper()
	interval, warmup := metricsInterval, metricsWarmupInterval
	t.Cleanup(func() { metricsInterval, metricsWarmupInterval = interval, warmup })
	metricsInterval, metricsWarmupInterval = d, d
}

// withTelemetryClient points the package client at a test endpoint. A
// fresh store has no consent recorded, which is the on-by-default case, and
// the environment opt-outs are cleared so a developer machine that has one
// set does not silently turn the test into a no-op.
func withTelemetryClient(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv(telemetry.EnvDisabled, "")
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv(telemetry.EnvSuppress, "")
	t.Setenv(telemetry.EnvEndpoint, endpoint)
	prev := telemetryClient
	t.Cleanup(func() { telemetryClient = prev })
	telemetryClient = telemetry.Init(telemetry.Config{StoreDir: t.TempDir(), Version: "0.0.0-test"})
	if !telemetryClient.Enabled() {
		t.Fatal("test telemetry client is disabled; it would assert nothing")
	}
}

// fakePS puts a stand-in `ps` first on PATH that reports CPU time growing
// at cores times real time, so the utilization the sampler computes is
// `cores` regardless of how long the tick actually took. It re-invokes
// this test binary rather than shelling out, so the arithmetic keeps full
// precision instead of a shell's.
func fakePS(t *testing.T, cores float64) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestFakePSHelperProcess$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HULL_FAKE_PS_CORES", strconv.FormatFloat(cores, 'f', -1, 64))
	t.Setenv("HULL_FAKE_PS_START", strconv.FormatInt(time.Now().UnixNano(), 10))
}

// TestFakePSHelperProcess is the stand-in `ps`, not a test. It runs only
// when fakePS has re-invoked this binary as one.
//
// It honours the column it was asked for, which is what makes the sampler
// tests able to fail: asked for `time`, it reports CPU time accumulating at
// `cores` times real time; asked for `%cpu`, it reports what Darwin's ps
// really would -- a near-constant number around cores*100, summed over the
// process' threads. Reading that column as though it were a utilization is
// the bug, so a sampler that goes back to it stops producing a sane rate.
func TestFakePSHelperProcess(t *testing.T) {
	cores := os.Getenv("HULL_FAKE_PS_CORES")
	if cores == "" {
		t.Skip("not running as the stand-in ps")
	}
	rate, _ := strconv.ParseFloat(cores, 64)
	format := ""
	for i, a := range os.Args {
		if a == "-o" && i+1 < len(os.Args) {
			format = os.Args[i+1]
		}
	}
	switch {
	case strings.Contains(format, "%cpu"):
		fmt.Printf("  524288 %.1f\n", rate*100)
	default:
		startNanos, _ := strconv.ParseInt(os.Getenv("HULL_FAKE_PS_START"), 10, 64)
		cpu := rate * time.Since(time.Unix(0, startNanos)).Seconds()
		// Darwin's shape: unbounded minutes, then seconds to hundredths.
		fmt.Printf("  524288 %d:%05.2f\n", int(cpu)/60, math.Mod(cpu, 60))
	}
	os.Exit(0)
}

// TestSamplerSettlesToTheSteadyIntervalAfterTheFirstSample pins the hand-off
// out of the warm-up.
//
// The warm-up exists only to get the first sample out quickly. Once one has
// gone, the cadence is the steady interval -- if the rearm still reads the
// warm-up, every attach emits an extra sample a few seconds after its first
// and the documented "every 30 seconds" is not what ships.
//
// The second sample is the signal, so the test waits for the first rather
// than sleeping a fixed window: the stand-in `ps` is a process spawn, which
// costs over a second under -race, and a fixed window measures the machine
// rather than the sampler.
func TestSamplerSettlesToTheSteadyIntervalAfterTheFirstSample(t *testing.T) {
	fakePS(t, 1.0)

	events := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events <- struct{}{}
	}))
	defer srv.Close()

	// Three orders of magnitude between the two, so which one the rearm
	// used is never a judgement call.
	intervalWas, warmupWas := metricsInterval, metricsWarmupInterval
	t.Cleanup(func() { metricsInterval, metricsWarmupInterval = intervalWas, warmupWas })
	metricsWarmupInterval = 200 * time.Millisecond
	metricsInterval = 60 * time.Second
	withTelemetryClient(t, srv.URL)

	done := make(chan struct{})
	defer close(done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 1, time.Now(), t.TempDir(), done)

	select {
	case <-events:
	case <-time.After(30 * time.Second):
		t.Fatal("no sample at all: the warm-up never produced one")
	}
	// The next one is 60s out. A second inside this window can only have
	// come from a rearm that was still on the warm-up.
	select {
	case <-events:
		t.Error("a second sample arrived far inside the steady interval: the sampler never left the warm-up")
	case <-time.After(5 * time.Second):
	}
}
