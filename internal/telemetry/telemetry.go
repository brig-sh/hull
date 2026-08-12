// Copyright (c) 2023-2026, Nubificus LTD
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

// Package telemetry implements the anonymous usage and crash telemetry
// described in docs/adr/0008-telemetry.md: an explicit consent ask on the
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
// -X github.com/nofireai/urunc-macos/internal/telemetry.Endpoint=...
// When empty (dev builds, or until the prod endpoint ships), nothing is
// ever sent; the debug mode still prints payloads.
var Endpoint = ""

// Environment variables honored by the package. The two opt-outs
// disable usage and crash telemetry alike, everywhere, no flags needed.
const (
	// EnvDisabled is our own opt-out switch.
	EnvDisabled = "URUNC_TELEMETRY_DISABLED"
	// EnvDoNotTrack is the consoledonottrack.com convention.
	EnvDoNotTrack = "DO_NOT_TRACK"
	// EnvDebug prints every payload to stderr instead of sending it.
	EnvDebug = "URUNC_TELEMETRY_DEBUG"
	// EnvProduct lets the urunc-claude and urunc-claude-desktop
	// wrappers attribute events to the product the user installed.
	EnvProduct = "URUNC_TELEMETRY_PRODUCT"
	// EnvEndpoint overrides the build-time Endpoint (tests, staging).
	EnvEndpoint = "URUNC_TELEMETRY_ENDPOINT"
	// EnvSuppress is internal: set on child invocations of our own
	// binary (compose self-exec, the network-gateway daemon) so one
	// user command counts once. Not a user-facing opt-out.
	EnvSuppress = "URUNC_TELEMETRY_SUPPRESS"
)

// DefaultProduct is used when EnvProduct is not set.
const DefaultProduct = "urunc-macos"

// checksumSalt seeds the envelope integrity checksum (see Checksum).
// Overridable at build time next to Endpoint if we ever rotate it.
var checksumSalt = "urunc-telemetry-v1"

// DocsURL is the public schema disclosure referenced by the consent
// prompt. Final location is tracked in NOFireAI/engineering#1001.
const DocsURL = "https://github.com/NOFireAI/homebrew-nofire/blob/main/TELEMETRY.md"
