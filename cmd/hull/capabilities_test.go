// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldHviStderr is what hvi 0.1.0 (the rc29 release) prints for `hvi caps`,
// copied from a real run. It is one more failed probe, and must read as one.
const oldHviStderr = "hvi: unknown subcommand \"caps\"; expected `boot`, `dump-fdt`, `smoke`, `sandbox-selftest`, `seccomp-selftest` or `--version`"

// writeFakeHvi puts an executable shell script named hvi in dir and returns
// its path.
func writeFakeHvi(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "hvi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake hvi: %v", err)
	}
	return path
}

// capsAnswer is a fake hvi that answers `caps --json` with doc and exits 0.
func capsAnswer(doc string) string {
	return "if [ \"$1\" = caps ]; then cat <<'EOF'\n" + doc + "\nEOF\nexit 0; fi\nexit 2"
}

// onPath makes the fake hvi in dir the one hull resolves. The installed
// release sits on PATH on a developer Mac, and the test binary has no hvi
// beside it, so PATH decides.
func onPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

func TestProbeNestedVirtSupported(t *testing.T) {
	dir := t.TempDir()
	onPath(t, dir)
	hvi := writeFakeHvi(t, dir, capsAnswer(
		`{"schemaVersion":1,"nestedVirt":{"supported":true,"detail":"Hypervisor.framework reports EL2"}}`))
	got := probeNestedVirt(context.Background(), hvi)
	want := nestedSupport{Supported: true, Answered: true, Backend: "hvi", Detail: "Hypervisor.framework reports EL2"}
	if got != want {
		t.Fatalf("probe = %+v, want %+v", got, want)
	}
	if err := requireNestedVirt(context.Background()); err != nil {
		t.Fatalf("preflight refused a host hvi says supports EL2: %v", err)
	}
}

func TestProbeNestedVirtUnsupported(t *testing.T) {
	dir := t.TempDir()
	onPath(t, dir)
	hvi := writeFakeHvi(t, dir, capsAnswer(
		`{"schemaVersion":1,"nestedVirt":{"supported":false,"detail":"Hypervisor.framework reports no EL2"}}`))
	got := probeNestedVirt(context.Background(), hvi)
	want := nestedSupport{Supported: false, Answered: true, Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"}
	if got != want {
		t.Fatalf("probe = %+v, want %+v", got, want)
	}
	err := requireNestedVirt(context.Background())
	const wantErr = "nested virtualization requested but not supported by this host: Hypervisor.framework reports no EL2"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("preflight error = %v, want %q", err, wantErr)
	}
}

// Every way the probe can fail to answer must come back as "not supported"
// with a reason, and as not answered. A "could not tell" that read as "yes"
// would boot a guest that asked for EL2 on a host that cannot give it; one
// that read as answered would let a broken probe pass for a host without
// EL2, which a test harness skips instead of failing.
func TestProbeNestedVirtFailuresAreNotSupported(t *testing.T) {
	cases := []struct {
		name, body, detail string
	}{
		{"framework error", "echo 'hvi: hv_vm_config_get_el2_supported failed: HV_ERROR' >&2; exit 1",
			"hvi caps failed: hvi: hv_vm_config_get_el2_supported failed: HV_ERROR"},
		{"silent failure", "exit 3", "hvi caps failed: exit status 3"},
		{"not json", "echo 'nested: yes'", "hvi caps printed something other than its JSON document"},
		{"unknown schema", `echo '{"schemaVersion":2,"nestedVirt":{"supported":true}}'`,
			"hvi caps answered with schemaVersion 2; this hull reads 1"},
		{"no schema", `echo '{"nestedVirt":{"supported":true}}'`,
			"hvi caps answered with schemaVersion 0; this hull reads 1"},
		{"no nestedVirt", `echo '{"schemaVersion":1}'`, "hvi caps did not report nestedVirt"},
		{"hvi without caps", "echo '" + oldHviStderr + "' >&2; exit 1",
			`hvi caps failed: hvi: unknown subcommand "caps"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			onPath(t, dir)
			hvi := writeFakeHvi(t, dir, tc.body)
			got := probeNestedVirt(context.Background(), hvi)
			if got.Supported || got.Answered {
				t.Fatalf("probe = %+v, want not supported and not answered", got)
			}
			if got.Backend != "hvi" {
				t.Errorf("backend = %q, want hvi", got.Backend)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tc.detail)
			}
			if err := requireNestedVirt(context.Background()); err == nil ||
				!strings.HasPrefix(err.Error(), nestedUnsupported+": ") {
				t.Errorf("preflight error = %v, want the %q refusal", err, nestedUnsupported)
			}
		})
	}
}

func TestProbeNestedVirtMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hvi")
	got := probeNestedVirt(context.Background(), missing)
	if got.Supported || got.Answered || !strings.Contains(got.Detail, missing) {
		t.Fatalf("probe = %+v, want not supported, not answered, naming %s", got, missing)
	}
}

// A hung hvi must not hang `hull run` or brig's doctor behind it.
func TestProbeNestedVirtTimesOut(t *testing.T) {
	saved := hviProbeTimeout
	hviProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hviProbeTimeout = saved })

	hvi := writeFakeHvi(t, t.TempDir(), "exec sleep 30")
	start := time.Now()
	got := probeNestedVirt(context.Background(), hvi)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe took %s, the timeout did not bound it", elapsed)
	}
	if got.Supported || got.Answered || !strings.Contains(got.Detail, "did not answer within 200ms") {
		t.Fatalf("probe = %+v, want not supported with the timeout named", got)
	}
}

// `hull capabilities --json` is the document brig parses. Pin its shape,
// including the hvi it probes being the one found on the path urunc uses.
func TestCapabilitiesJSONShape(t *testing.T) {
	cases := []struct {
		name, body string
		want       nestedSupport
	}{
		{"supported", capsAnswer(`{"schemaVersion":1,"nestedVirt":{"supported":true,"detail":"EL2 available"}}`),
			nestedSupport{Supported: true, Answered: true, Backend: "hvi", Detail: "EL2 available"}},
		{"not supported", capsAnswer(`{"schemaVersion":1,"nestedVirt":{"supported":false,"detail":"no EL2"}}`),
			nestedSupport{Supported: false, Answered: true, Backend: "hvi", Detail: "no EL2"}},
		{"hvi without caps", "echo '" + oldHviStderr + "' >&2; exit 1",
			nestedSupport{Supported: false, Answered: false, Backend: "hvi",
				Detail: "hvi caps failed: " + oldHviStderr}},
		{"no hvi at all", "",
			nestedSupport{Supported: false, Answered: false, Backend: "hvi",
				Detail: "hvi not found next to hull or in PATH"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				writeFakeHvi(t, dir, tc.body)
			}
			onPath(t, dir)

			var runErr error
			out := captureVersionStdout(t, func() {
				runErr = capabilitiesCommand().Run(context.Background(), []string{"capabilities", "--json"})
			})
			if runErr != nil {
				t.Fatalf("capabilities --json: %v", runErr)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(out), &raw); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if raw["schemaVersion"] != float64(1) {
				t.Errorf("schemaVersion = %v, want 1", raw["schemaVersion"])
			}
			nested, ok := raw["nestedVirt"].(map[string]any)
			if !ok {
				t.Fatalf("nestedVirt missing or not an object: %s", out)
			}
			for _, key := range []string{"supported", "answered", "backend", "detail"} {
				if _, ok := nested[key]; !ok {
					t.Errorf("nestedVirt.%s missing: %s", key, out)
				}
			}
			var got capabilitiesPayload
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.NestedVirt != tc.want {
				t.Fatalf("nestedVirt = %+v, want %+v", got.NestedVirt, tc.want)
			}
		})
	}
}

func TestCapabilitiesTextLine(t *testing.T) {
	if got := nestedLine(nestedSupport{Supported: true, Backend: "hvi", Detail: "x"}); got != "nested virtualization: supported (hvi)" {
		t.Errorf("supported line = %q", got)
	}
	got := nestedLine(nestedSupport{Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"})
	if got != "nested virtualization: not supported (hvi did not answer): Hypervisor.framework reports no EL2" {
		t.Errorf("unanswered line = %q", got)
	}
	got = nestedLine(nestedSupport{Answered: true, Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"})
	if got != "nested virtualization: not supported (hvi): Hypervisor.framework reports no EL2" {
		t.Errorf("unsupported line = %q", got)
	}
}
