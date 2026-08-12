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

// Package conformance defines the typed loader for the compose-spec
// capability manifest (capabilities.yaml) that drives the conformance suite
// described in docs/adr/0002-compose-conformance-suite.md.
//
// This package is deliberately OS-agnostic (no darwin build tag) and depends
// only on the standard library plus gopkg.in/yaml.v3, so the manifest guards
// build and run on any host. Parsing here is validation-free; all validation
// lives in the guard tests.
package conformance

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Status is a capability's support level against the compose spec.
type Status string

// Support levels. supported = behaves per spec; partial = works with a
// documented divergence (see Capability.Notes); unsupported = not honored
// yet; out-of-scope = structurally foreclosed, never planned (ADR-0003) —
// still warned about loudly and still tested, but excluded from the
// headline score's denominator.
const (
	StatusSupported   Status = "supported"
	StatusPartial     Status = "partial"
	StatusUnsupported Status = "unsupported"
	StatusOutOfScope  Status = "out-of-scope"
)

// Tier is which conformance test tier backs a capability.
type Tier string

// Tiers. static = parse/validate/normalize without booting a VM; runtime =
// boots real VMs to observe behavior.
const (
	TierStatic  Tier = "static"
	TierRuntime Tier = "runtime"
)

// Capability is one entry in the manifest: a single compose-spec surface
// (top-level element, service key, extension, or CLI verb) and its status.
type Capability struct {
	// ID is the stable identifier, e.g. "service.volumes", "toplevel.networks",
	// "cli.up", "ext.x-healthcheck-tcp".
	ID string `yaml:"id"`
	// Area groups capabilities for the report (services, networking, volumes,
	// lifecycle, resources, security, cli, extensions, ...).
	Area string `yaml:"area"`
	// Status is supported | partial | unsupported.
	Status Status `yaml:"status"`
	// Tier is static | runtime.
	Tier Tier `yaml:"tier"`
	// Notes carries the divergence detail for partial entries and the
	// reason/alternative for unsupported entries.
	Notes string `yaml:"notes"`
	// Security is the security tradeoff. Mandatory for supported/partial
	// ("none identified" is acceptable, absence is not).
	Security string `yaml:"security"`
	// Test is the backing test name (TestConformanceStatic/<id> or
	// TestConformanceRuntime/<id>); a forward reference until #58/#59 land.
	Test string `yaml:"test"`
}

// Manifest is the parsed capabilities.yaml.
type Manifest struct {
	Capabilities []Capability `yaml:"capabilities"`
}

// LoadManifest reads and parses the manifest at path. It performs no
// validation beyond YAML well-formedness; the guard tests assert the
// invariants.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return &m, nil
}
