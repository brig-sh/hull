// Copyright (c) 2023-2026, Nubificus LTD
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

// Package telemetry implements the anonymous usage and crash telemetry
// described in docs/telemetry.md: an explicit consent ask on the
// first interactive invocation (default yes, single enter approves),
// default-on for unattended invocations, and fire-and-forget delivery
// that can never slow down or break a user command.
//
// The full field-by-field schema and the opt-out surface are documented
// in docs/telemetry.md. If a field is not listed there, it must not be
// sent from here.
package telemetry

// SchemaVersion is the version of the event schema. Any change to what
// is collected bumps it, together with docs/telemetry.md.
const SchemaVersion = 1

// ConsentVersion is the version of the consent ask. Expanding what we
// collect bumps it, which re-triggers the prompt on the next interactive
// invocation; until the user re-consents, only previously consented
// fields may be sent.
const ConsentVersion = 1

// Endpoint is the ingestion endpoint URL, injected at build time via
// -X github.com/brig-sh/hull/internal/telemetry.Endpoint=...
// When empty (dev builds, or until the prod endpoint ships), nothing is
// ever sent; the debug mode still prints payloads.
var Endpoint = ""

// Environment variables honored by the package. The two opt-outs
// disable usage and crash telemetry alike, everywhere, no flags needed.
const (
	// EnvDisabled is our own opt-out switch.
	EnvDisabled = "HULL_TELEMETRY_DISABLED"
	// EnvDoNotTrack is the consoledonottrack.com convention.
	EnvDoNotTrack = "DO_NOT_TRACK"
	// EnvDebug prints every payload to stderr instead of sending it.
	EnvDebug = "HULL_TELEMETRY_DEBUG"
	// EnvProduct lets a wrapper driving hull -- brig, today -- attribute
	// events to the product the user actually installed.
	EnvProduct = "HULL_TELEMETRY_PRODUCT"
	// EnvEndpoint overrides the build-time Endpoint (tests, staging).
	EnvEndpoint = "HULL_TELEMETRY_ENDPOINT"
	// EnvSuppress is internal: set on child invocations of our own
	// binary (compose self-exec, the network-gateway daemon) so one
	// user command counts once. Not a user-facing opt-out.
	EnvSuppress = "HULL_TELEMETRY_SUPPRESS"
)

// ServiceName is the OTLP resource and scope name every event carries. It
// identifies the telemetry client, not the product: hull, brig and the retired
// urunc- wrappers all report under it, and `product` is what distinguishes
// them.
//
// It was "urunc-telemetry" until the brig rename, taken as a clean cutover
// before launch: the collector selects on the new name only, so anything
// still emitting the old one is not ingested.
const ServiceName = "brig-telemetry"

// DefaultProduct is used when EnvProduct is not set.
const DefaultProduct = "hull"

// checksumSalt seeds the envelope integrity checksum (see Checksum).
// Overridable at build time next to Endpoint if we ever rotate it.
//
// This is a wire contract, not a name: the collector recomputes the checksum
// with the same literal (telemetry-gate values.yaml in NOFireAI/gitops) and
// drops anything that does not match. Changing it on one side alone silently
// discards every event, so the two move together or not at all. The rename
// from urunc-telemetry-v1 was a clean cutover taken before launch, which
// costs the events from any client still carrying the old salt.
var checksumSalt = "brig-telemetry-v1"

// DocsURL is the public schema disclosure referenced by the consent prompt.
// It has to be readable by whoever is being asked to consent: this previously
// pointed into the private homebrew tap and 404'd for everyone.
const DocsURL = "https://github.com/brig-sh/hull/blob/main/docs/telemetry.md"
