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

// Command report renders docs/compose-conformance.md from the compose-spec
// capability manifest (test/conformance/capabilities.yaml). It is the "generated
// conformance report" of ADR 0002 (part 4): the manifest is the single source of
// truth and this tool projects it into the published matrix.
//
// The render is deterministic (stable ordering: area, then id) so CI can
// regenerate and diff. Run it directly, or with -check to fail when the checked-in
// doc has drifted from the manifest:
//
//	go run ./test/conformance/cmd/report            # regenerate docs/compose-conformance.md
//	go run ./test/conformance/cmd/report -check      # exit non-zero if it would change
//
// Only the standard library plus the conformance package (LoadManifest) are used;
// no new module dependencies.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	conformance "github.com/nofireai/urunc-macos/test/conformance"
)

func main() {
	root := repoRoot()
	defManifest := filepath.Join(root, "test", "conformance", "capabilities.yaml")
	defOut := filepath.Join(root, "docs", "compose-conformance.md")

	manifestPath := flag.String("manifest", defManifest, "path to the capability manifest (capabilities.yaml)")
	outPath := flag.String("out", defOut, "path to the generated report (docs/compose-conformance.md)")
	check := flag.Bool("check", false, "do not write; exit non-zero if the existing report differs from the render")
	flag.Parse()

	if err := run(*manifestPath, *outPath, *check); err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		os.Exit(1)
	}
}

func run(manifestPath, outPath string, check bool) error {
	m, err := conformance.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	if len(m.Capabilities) == 0 {
		return fmt.Errorf("manifest %s has no capabilities", manifestPath)
	}

	prov, err := loadProvenance(filepath.Join(filepath.Dir(manifestPath), "spec", "PROVENANCE.md"))
	if err != nil {
		return err
	}

	rendered := render(m, prov)

	if check {
		existing, err := os.ReadFile(outPath)
		if err != nil {
			return fmt.Errorf("read %s for -check: %w", outPath, err)
		}
		if !bytes.Equal(existing, []byte(rendered)) {
			return fmt.Errorf("%s is stale; regenerate with `make conformance-report`", outPath)
		}
		return nil
	}

	if err := os.WriteFile(outPath, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}

// repoRoot resolves the module root relative to this source file so the -manifest
// and -out defaults hold regardless of the caller's working directory. This file
// lives at test/conformance/cmd/report/main.go, so the root is four levels up.
func repoRoot() string {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", "..", ".."))
}

// provenance is the pinned compose-spec snapshot metadata, surfaced in the report
// header so a reader knows which spec version the percentages are measured against.
type provenance struct {
	Source  string
	Commit  string
	Fetched string
}

// loadProvenance extracts the "Source", "Pinned commit", and "Fetched" bullets from
// spec/PROVENANCE.md. Missing fields degrade to a placeholder rather than failing:
// the report is still meaningful without a perfectly-shaped provenance file.
func loadProvenance(path string) (provenance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provenance{}, fmt.Errorf("read provenance %s: %w", path, err)
	}
	prov := provenance{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		switch {
		case strings.HasPrefix(line, "Source:"):
			prov.Source = strings.TrimSpace(strings.TrimPrefix(line, "Source:"))
		case strings.HasPrefix(line, "Pinned commit:"):
			prov.Commit = strings.TrimSpace(strings.TrimPrefix(line, "Pinned commit:"))
		case strings.HasPrefix(line, "Fetched:"):
			prov.Fetched = strings.TrimSpace(strings.TrimPrefix(line, "Fetched:"))
		}
	}
	return prov, nil
}

// score is the ADR 0002 scoring (supported counts 1, partial 0.5) with the
// ADR 0003 refinement: out-of-scope entries are excluded from the headline
// (in-scope) denominator but kept in the full-spec one.
type score struct {
	supported   int
	partial     int
	unsupported int
	outOfScope  int
}

func (s score) total() int { return s.supported + s.partial + s.unsupported + s.outOfScope }

// inScopeTotal excludes out-of-scope entries (ADR 0003).
func (s score) inScopeTotal() int { return s.supported + s.partial + s.unsupported }

func (s score) points() float64 { return float64(s.supported) + 0.5*float64(s.partial) }

// pct is the headline in-scope score.
func (s score) pct() float64 {
	t := s.inScopeTotal()
	if t == 0 {
		return 0
	}
	return s.points() / float64(t) * 100
}

// fullSpecPct scores over every capability, out-of-scope included, so the
// denominator change from ADR 0003 stays visible.
func (s score) fullSpecPct() float64 {
	t := s.total()
	if t == 0 {
		return 0
	}
	return s.points() / float64(t) * 100
}

func (s *score) add(status conformance.Status) {
	switch status {
	case conformance.StatusSupported:
		s.supported++
	case conformance.StatusPartial:
		s.partial++
	case conformance.StatusOutOfScope:
		s.outOfScope++
	default:
		s.unsupported++
	}
}

const regenCmd = "make conformance-report"

// render projects the manifest into the report markdown. It is pure and
// deterministic: identical (manifest, provenance) input yields byte-identical
// output, with areas sorted alphabetically and entries within an area by id.
func render(m *conformance.Manifest, prov provenance) string {
	// Group by area and compute per-area + overall scores.
	byArea := map[string][]conformance.Capability{}
	areaScore := map[string]*score{}
	overall := score{}
	for _, c := range m.Capabilities {
		byArea[c.Area] = append(byArea[c.Area], c)
		if areaScore[c.Area] == nil {
			areaScore[c.Area] = &score{}
		}
		areaScore[c.Area].add(c.Status)
		overall.add(c.Status)
	}

	areas := make([]string, 0, len(byArea))
	for a := range byArea {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		sort.Slice(byArea[a], func(i, j int) bool { return byArea[a][i].ID < byArea[a][j].ID })
	}

	var b strings.Builder

	// Header: generated notice, regen command, pinned spec snapshot.
	b.WriteString("# Compose-spec conformance\n\n")
	b.WriteString("<!-- GENERATED FILE — DO NOT EDIT BY HAND. -->\n")
	fmt.Fprintf(&b, "<!-- Regenerate with `%s`; edit test/conformance/capabilities.yaml instead. -->\n\n", regenCmd)
	fmt.Fprintf(&b, "This report is generated from [`test/conformance/capabilities.yaml`](../test/conformance/capabilities.yaml) "+
		"by `go run ./test/conformance/cmd/report`. Do not edit it by hand — change the manifest and run `%s`.\n\n", regenCmd)

	b.WriteString("Measured against the pinned compose-spec schema snapshot")
	if prov.Commit != "" {
		fmt.Fprintf(&b, " (commit `%s`", prov.Commit)
		if prov.Fetched != "" {
			fmt.Fprintf(&b, ", fetched %s", prov.Fetched)
		}
		b.WriteString(")")
	}
	b.WriteString("; see [`test/conformance/spec/PROVENANCE.md`](../test/conformance/spec/PROVENANCE.md).\n\n")

	// Overall banner: headline is the in-scope score (ADR 0003); the
	// full-spec line keeps the denominator change visible.
	fmt.Fprintf(&b, "## Overall: %s of in-scope capabilities\n\n", pctString(overall.pct()))
	fmt.Fprintf(&b, "`score = (supported + 0.5 × partial) / in-scope total` over %d in-scope capabilities: "+
		"**%d supported**, **%d partial**, **%d unsupported**.\n\n",
		overall.inScopeTotal(), overall.supported, overall.partial, overall.unsupported)
	fmt.Fprintf(&b, "Full-spec coverage: **%s** of all %d capabilities; %d are declared out of scope by "+
		"[ADR-0003](adr/0003-compose-out-of-scope.md) (structurally foreclosed: orchestration, cross-VM "+
		"namespace sharing, Windows-only, Docker-platform machinery, device passthrough). "+
		"Out-of-scope keys still warn loudly and stay tested.\n\n",
		pctString(overall.fullSpecPct()), overall.total(), overall.outOfScope)

	// Tier legend.
	b.WriteString("**Tier** — `static` entries are verified in CI without booting a VM; " +
		"`runtime` entries are verified only where the runtime suite runs " +
		"(`URUNC_CONFORMANCE_RUNTIME=1`, self-hosted runner or a developer Mac).\n\n")

	// Per-area score table.
	b.WriteString("## Scores by area\n\n")
	b.WriteString("Scores are over in-scope entries; the out-of-scope column is excluded from the denominator.\n\n")
	b.WriteString("| Area | Supported | Partial | Unsupported | Out of scope | Score |\n")
	b.WriteString("|------|-----------|---------|-------------|--------------|-------|\n")
	for _, a := range areas {
		s := areaScore[a]
		sc := "—"
		if s.inScopeTotal() > 0 {
			sc = pctString(s.pct())
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s |\n",
			a, s.supported, s.partial, s.unsupported, s.outOfScope, sc)
	}
	b.WriteString("\n")

	// Out-of-scope section: every excluded entry with its ADR-0003 category.
	b.WriteString("## Out of scope (ADR-0003)\n\n")
	b.WriteString("These capabilities are structurally foreclosed, not backlog. They still warn loudly and keep their conformance tests.\n\n")
	b.WriteString("| Capability | Area | Category | Notes |\n")
	b.WriteString("|------------|------|----------|-------|\n")
	var oos []conformance.Capability
	for _, c := range m.Capabilities {
		if c.Status == conformance.StatusOutOfScope {
			oos = append(oos, c)
		}
	}
	sort.Slice(oos, func(i, j int) bool { return oos[i].ID < oos[j].ID })
	for _, c := range oos {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", c.ID, c.Area, outOfScopeCategory(c.Notes), cell(c.Notes))
	}
	b.WriteString("\n")

	// One table per area.
	b.WriteString("## Capabilities\n\n")
	for _, a := range areas {
		hdr := "—"
		if areaScore[a].inScopeTotal() > 0 {
			hdr = pctString(areaScore[a].pct())
		}
		fmt.Fprintf(&b, "### %s (%s)\n\n", a, hdr)
		b.WriteString("| Capability | Status | Tier | Divergence notes | Security tradeoffs |\n")
		b.WriteString("|------------|--------|------|------------------|--------------------|\n")
		for _, c := range byArea[a] {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
				c.ID, string(c.Status), string(c.Tier), cell(c.Notes), cell(c.Security))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// pctString formats a percentage to one decimal, e.g. "7.8%".
func pctString(p float64) string {
	return fmt.Sprintf("%.1f%%", p)
}

// outOfScopeCategories mirror the ADR-0003 list enforced by the manifest
// well-formedness guard; the first one found in the notes names the entry's
// exclusion reason in the report.
var outOfScopeCategories = []string{
	"orchestration",
	"cross-VM namespace sharing",
	"Windows containers only",
	"Docker-platform machinery",
	"no device passthrough",
}

func outOfScopeCategory(notes string) string {
	for _, c := range outOfScopeCategories {
		if strings.Contains(notes, c) {
			return c
		}
	}
	return "—"
}

// cell renders a manifest text field into a single markdown table cell: folded
// YAML scalars keep embedded newlines and run-on whitespace, which would break a
// one-line cell, so collapse all whitespace to single spaces and escape the pipe.
// An empty field (supported entries carry no notes; some unsupported entries carry
// no security) renders as an em dash rather than a blank cell.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}
