// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// The flag reaches hvi's argv and the instance record only when it was
// asked for, and never reaches another runner whatever was asked for. The
// record must agree with the argv: `hull inspect` is how a caller learns a
// guest holds EL2.
func TestLaunchRecordNestedVirt(t *testing.T) {
	base := []string{"/x/hvi", "boot", "--kernel", "k"}
	for _, vmm := range []hypervisors.VmmType{hypervisors.HviVmm, hypervisors.VzVmm, hypervisors.QemuVmm} {
		for _, nested := range []bool{false, true} {
			args, state := launchRecord(slices.Clone(base), store.InstanceState{ID: "i", Backend: string(vmm)}, vmm, nested)
			want := vmm == hypervisors.HviVmm && nested
			if has := slices.Contains(args, "--nested-virt"); has != want {
				t.Errorf("%s nested=%v: argv %q, want --nested-virt present: %v", vmm, nested, args, want)
			}
			if !slices.Equal(args[:len(base)], base) {
				t.Errorf("%s nested=%v: argv prefix changed: %q", vmm, nested, args)
			}
			if state.NestedVirt != want {
				t.Errorf("%s nested=%v: record NestedVirt=%v, want %v", vmm, nested, state.NestedVirt, want)
			}
			if state.ID != "i" || state.Backend != string(vmm) {
				t.Errorf("%s nested=%v: record lost its fields: %+v", vmm, nested, state)
			}
		}
	}
}

func TestResolveHypervisor(t *testing.T) {
	annotated := func(v string) map[string]string {
		return map[string]string{"com.urunc.unikernel.hypervisor": v}
	}
	cases := []struct {
		override    string
		annotations map[string]string
		name, src   string
	}{
		{"", nil, "qemu", "default"},
		{"", map[string]string{}, "qemu", "default"},
		{"", annotated("hvi"), "hvi", "annotation"},
		{"", annotated("apple"), "vz", "annotation"},
		{"", annotated("qemu-hvf"), "qemu", "annotation"},
		{"hvi", annotated("vz"), "hvi", "flag"},
		{"virtualization", nil, "vz", "flag"},
		{"qemu-hvf", annotated("hvi"), "qemu", "flag"},
	}
	for _, tc := range cases {
		name, src := resolveHypervisor(tc.override, tc.annotations)
		if name != tc.name || src != tc.src {
			t.Errorf("resolveHypervisor(%q, %v) = %q, %q; want %q, %q", tc.override, tc.annotations, name, src, tc.name, tc.src)
		}
	}
}

// Every path through the nested gate: where the backend comes from, whether
// the flag was given, and what the gate says at each of its two points. A
// run that asks for nested virtualization on hvi probes the host exactly
// once, as early as the backend is known; any other backend is refused
// without a probe; a run that did not ask never probes.
func TestNestedVirtGate(t *testing.T) {
	const refusal = "--nested-virt is only supported with the hvi hypervisor"
	cases := []struct {
		name        string
		requested   bool
		override    string
		annotations map[string]string
		earlyProbe  bool
		earlyErr    bool
		lateProbe   bool
		lateErr     bool
	}{
		{name: "not requested, hvi", override: "hvi"},
		{name: "not requested, default qemu"},
		{name: "flag hvi probes before the store", requested: true, override: "hvi", earlyProbe: true},
		{name: "flag vz refused before the store", requested: true, override: "vz", earlyErr: true},
		{name: "flag apple refused before the store", requested: true, override: "apple", earlyErr: true},
		{name: "flag qemu refused before the store", requested: true, override: "qemu", earlyErr: true},
		{name: "image hvi probes after the pull", requested: true,
			annotations: map[string]string{"com.urunc.unikernel.hypervisor": "hvi"}, lateProbe: true},
		{name: "image vz refused after the pull", requested: true,
			annotations: map[string]string{"com.urunc.unikernel.hypervisor": "vz"}, lateErr: true},
		{name: "default qemu refused after the pull", requested: true, lateErr: true},
		{name: "flag hvi beats image vz", requested: true, override: "hvi",
			annotations: map[string]string{"com.urunc.unikernel.hypervisor": "vz"}, earlyProbe: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := nestedVirtGate{requested: tc.requested}
			probe, err := g.beforeStore(tc.override)
			if probe != tc.earlyProbe || (err != nil) != tc.earlyErr {
				t.Fatalf("beforeStore = %v, %v; want probe %v, error %v", probe, err, tc.earlyProbe, tc.earlyErr)
			}
			if err != nil {
				if !strings.HasPrefix(err.Error(), refusal) {
					t.Fatalf("beforeStore error %q, want the backend refusal", err)
				}
				return // runInstance returns here
			}
			name, _ := resolveHypervisor(tc.override, tc.annotations)
			probe, err = g.atBackend(name)
			if probe != tc.lateProbe || (err != nil) != tc.lateErr {
				t.Fatalf("atBackend(%q) = %v, %v; want probe %v, error %v", name, probe, err, tc.lateProbe, tc.lateErr)
			}
			if err != nil && !strings.HasPrefix(err.Error(), refusal) {
				t.Fatalf("atBackend error %q, want the backend refusal", err)
			}
		})
	}
}

// nestedRunFixture puts a fake hvi first on PATH that records every
// invocation in a marker file and answers `caps --json` with capsDoc. It
// returns the marker path and a --store-dir whose parent is a regular file:
// any run that gets as far as opening the store fails there at once with
// ENOTDIR, so a regression past the preflight shows up as that error and
// never attaches a sparse image on the machine running the tests.
func nestedRunFixture(t *testing.T, capsDoc string) (marker, storeDir string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "hvi-called")
	writeFakeHvi(t, dir, `echo "$@" >> '`+marker+`'
if [ "$1" = caps ]; then cat <<'EOF'
`+capsDoc+`
EOF
exit 0; fi
exit 2`)
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return marker, filepath.Join(blocker, "store")
}

// runHull drives `hull --store-dir <storeDir> run <args...>` in process.
func runHull(t *testing.T, storeDir string, args ...string) error {
	t.Helper()
	root := &cli.Command{
		Name:     "hull",
		Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir"}},
		Commands: []*cli.Command{runCommand()},
	}
	full := append([]string{"hull", "--store-dir", storeDir, "run"}, args...)
	return root.Run(context.Background(), full)
}

// A host whose hvi says no EL2 is refused with the shared phrase before the
// store is opened or an image is pulled.
func TestRunNestedVirtUnsupportedRefusesBeforeSideEffects(t *testing.T) {
	marker, storeDir := nestedRunFixture(t,
		`{"schemaVersion":1,"nestedVirt":{"supported":false,"detail":"Hypervisor.framework reports no EL2"}}`)
	err := runHull(t, storeDir, "--nested-virt", "--hypervisor", "hvi", "docker.io/library/alpine:3")
	const want = "nested virtualization requested but not supported by this host: Hypervisor.framework reports no EL2"
	if err == nil || err.Error() != want {
		t.Fatalf("run error = %v, want %q", err, want)
	}
	if calls, _ := os.ReadFile(marker); !bytes.Contains(calls, []byte("caps --json")) {
		t.Fatalf("hvi caps --json was not run; hvi saw: %q", calls)
	}
}

// No hvi to ask is a refusal too, with the reason the lookup gave.
func TestRunNestedVirtMissingHviRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := runHull(t, filepath.Join(blocker, "store"), "--nested-virt", "--hypervisor", "hvi", "img")
	const want = "nested virtualization requested but not supported by this host: hvi not found next to hull or in PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("run error = %v, want %q", err, want)
	}
}

// Any other backend is refused by name, before hvi is even asked.
func TestRunNestedVirtOffHviRefusedWithoutProbe(t *testing.T) {
	for _, tc := range []struct{ flag, name string }{
		{"vz", "vz"}, {"apple", "vz"}, {"qemu", "qemu"}, {"qemu-hvf", "qemu"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			marker, storeDir := nestedRunFixture(t, `{"schemaVersion":1,"nestedVirt":{"supported":true}}`)
			err := runHull(t, storeDir, "--nested-virt", "--hypervisor", tc.flag, "img")
			want := `--nested-virt is only supported with the hvi hypervisor (got "` + tc.name + `")`
			if err == nil || err.Error() != want {
				t.Fatalf("run error = %v, want %q", err, want)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("hvi was run for a %s instance", tc.flag)
			}
		})
	}
}

// A supported host passes the preflight and the run carries on to the
// store, which is where this fixture stops it.
func TestRunNestedVirtSupportedPassesPreflight(t *testing.T) {
	marker, storeDir := nestedRunFixture(t,
		`{"schemaVersion":1,"nestedVirt":{"supported":true,"detail":"EL2 available"}}`)
	err := runHull(t, storeDir, "--nested-virt", "--hypervisor", "hvi", "img")
	if err == nil || strings.Contains(err.Error(), nestedUnsupported) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("run error = %v, want the fixture's store failure after a passed preflight", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("hvi caps was not run")
	}
}

// Without the flag nothing about nested virtualization happens: hvi is not
// probed at all.
func TestRunWithoutNestedVirtNeverProbes(t *testing.T) {
	marker, storeDir := nestedRunFixture(t,
		`{"schemaVersion":1,"nestedVirt":{"supported":false,"detail":"no"}}`)
	err := runHull(t, storeDir, "--hypervisor", "hvi", "img")
	if err == nil || strings.Contains(err.Error(), nestedUnsupported) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("run error = %v, want the fixture's store failure", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("hvi was probed for a run that did not ask for nested virtualization")
	}
}

// `hull inspect` prints the instance record as it is. nestedVirt is there
// when the guest has EL2 and absent otherwise, so a consumer tests for the
// key and every older record reads as "not nested".
func TestInspectRecordsNestedVirt(t *testing.T) {
	for _, nested := range []bool{false, true} {
		var buf bytes.Buffer
		if err := printJSON(&buf, &store.InstanceState{ID: "a", Backend: "hvi", NestedVirt: nested}); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		v, present := raw["nestedVirt"]
		if present != nested || (nested && v != true) {
			t.Errorf("NestedVirt=%v: inspect shows nestedVirt=%v (present %v)\n%s", nested, v, present, buf.String())
		}
	}
}
