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
	"flag"
	"os"
	"path/filepath"
	"testing"

	conformance "github.com/brig-sh/hull/test/conformance"
)

// update regenerates the golden file instead of comparing against it.
var update = flag.Bool("update", false, "update the golden report file")

// fixtureManifest is a small hand-built manifest exercising every field
// combination the renderer special-cases: supported (no notes), partial (notes +
// security), unsupported with a security field, unsupported without one, a folded
// scalar with run-on whitespace and newlines, and a value containing a pipe. Two
// areas out of alphabetical order and ids out of order within an area prove the
// stable (area, then id) sort.
func fixtureManifest() *conformance.Manifest {
	return &conformance.Manifest{Capabilities: []conformance.Capability{
		{
			ID:       "service.zeta",
			Area:     "services",
			Status:   conformance.StatusSupported,
			Tier:     conformance.TierStatic,
			Security: "none identified",
		},
		{
			ID:       "service.alpha",
			Area:     "services",
			Status:   conformance.StatusPartial,
			Tier:     conformance.TierRuntime,
			Notes:    "Folded scalar\nwith a newline   and  extra spaces. Pipe a|b stays literal.",
			Security: "Host path exposed;\nread-only intent not enforced.",
		},
		{
			ID:       "cli.up",
			Area:     "cli",
			Status:   conformance.StatusUnsupported,
			Tier:     conformance.TierStatic,
			Notes:    "Not a subcommand.",
			Security: "none identified",
		},
		{
			ID:     "cli.down",
			Area:   "cli",
			Status: conformance.StatusUnsupported,
			Tier:   conformance.TierStatic,
			Notes:  "Also not a subcommand.",
			// no security field -> renders as em dash
		},
		{
			ID:     "service.deploy",
			Area:   "deploy",
			Status: conformance.StatusOutOfScope,
			Tier:   conformance.TierStatic,
			Notes:  "Swarm-style deployment. Out of scope (ADR-0003): orchestration.",
			// excluded from the in-scope denominator; its area score renders as a dash
		},
	}}
}

func fixtureProvenance() provenance {
	return provenance{
		Source:  "https://example.invalid/compose-spec.json",
		Commit:  "0123456789abcdef",
		Fetched: "2026-07-29",
	}
}

const goldenPath = "testdata/report_golden.md"

// TestReportGolden renders the in-test fixture and compares against the checked-in
// golden. Run `go test ./test/conformance/cmd/report -update` to refresh it.
func TestReportGolden(t *testing.T) {
	got := render(fixtureManifest(), fixtureProvenance())

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("render does not match %s (run with -update to refresh).\n--- got ---\n%s", goldenPath, got)
	}
}

// TestRenderDeterministic renders the real capabilities.yaml twice and asserts the
// two outputs are byte-identical — the freshness gate in CI relies on this.
func TestRenderDeterministic(t *testing.T) {
	path := filepath.Join("..", "..", "capabilities.yaml")
	m, err := conformance.LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	prov, err := loadProvenance(filepath.Join("..", "..", "spec", "PROVENANCE.md"))
	if err != nil {
		t.Fatalf("loadProvenance: %v", err)
	}
	a := render(m, prov)
	b := render(m, prov)
	if a != b {
		t.Error("render of the real manifest is not deterministic across two calls")
	}
	if len(a) == 0 {
		t.Error("render produced empty output")
	}
}
