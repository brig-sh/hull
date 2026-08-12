# ADR 0008: First-party usage and crash telemetry

**Status**: Accepted
**Date**: 2026-08-04
**Context**: What urunc-macos, urunc-claude and urunc-claude-desktop report
about usage and crashes, to whom, and how users turn it off

---

## Context

We are launching urunc-macos together with the urunc-claude and
urunc-claude-desktop wrappers, and at the moment we have zero visibility on
usage and crashes. We want to answer concrete release questions: which
backend people select (qemu vs vz) and whether boots succeed, which commands
and products get used, and which versions crash where -- without asking
users to dig into `~/Library/Logs` and attach reports by hand.

Downloads alone do not tell us this, and brew's public analytics API only
covers homebrew-core and homebrew-cask, so a third-party tap gets nothing
there.

The prior art is well established (surveyed in NOFireAI/engineering#1008):
Homebrew, the .NET CLI, Next.js, Turborepo, DVC and others all ship opt-out
telemetry, and the community accepts it under a specific set of conditions.
The backlash cases (Homebrew 2016, Audacity 2021, GitHub CLI 2026) were
about silent enablement and third-party recipients, not about measurement
itself. The conditions, which this ADR adopts wholesale:

1. A notice is printed before the first event is ever sent.
2. No PII, no file paths, no argument values, no third-party analytics
   backend. Events go to an endpoint we host.
3. At most a random, locally-generated identifier.
4. `DO_NOT_TRACK` is honored in addition to our own env var and a
   `telemetry` subcommand.
5. The full schema is documented publicly, with a debug mode that prints
   payloads without sending, so anyone can verify what leaves the machine.

## Decision

### 1. Events, schema version 1

Five event types. The full field-by-field reference lives in
[docs/telemetry.md](../telemetry.md); the ADR-level summary:

- **command**: schema version, product (`urunc-macos` | `urunc-claude` |
  `urunc-claude-desktop`), tool version, macOS major.minor, arch, top-level
  command name (eg. `run`, `compose`; never arguments), outcome (`ok` |
  `error`), and a coarse error class on failure. Never the error message
  itself -- messages can embed paths and image references.
- **start**: selected backend (`qemu` | `vz`), how it was selected
  (`flag` | `annotation` | `default`) and boot outcome, emitted when a
  VMM launch is attempted. **end** reports the instance lifetime when
  the exit is observed (the foreground `run`, or `stop` for a detached
  VM). **metrics** samples the VMM process (RSS, CPU) every 30s while a
  CLI is attached to it -- the foreground `run`, or an `exec` session on
  a detached VM, which is how a urunc-claude sandbox is sampled while a
  session uses it; an idle detached VM reports nothing. A per-instance
  flock keeps this to one sampler per VM. These are disjoint events, per
  review, so each stage of a run is analyzable on its own. No image
  names or digests anywhere: registry references can identify a user or
  an internal system.
- **crash**: tool version, product, macOS major.minor, arch, command name,
  backend if known, and the Go stack trace with file paths trimmed to
  module-relative form. Panics only get captured from our own binary; we do
  not read macOS DiagnosticReports or anything about other processes.

The common envelope carries product, version, arch, the full uname
(explicitly excluding the hostname), a `captured_at` timestamp, and an
integrity checksum: SHA-256 over a few envelope fields with a fixed
salt. Ingestion drops payloads whose checksum does not match. This is a
soft control against naive endpoint abuse -- the salt ships in the
binary, so it is deliberately not treated as a security boundary.

### 2. Identifier: a random install UUID

We keep one random UUID, generated locally on first run and stored in
`~/.urunc-macos/telemetry.json`. It is not derived from hardware, username
or anything else about the machine, and deleting the file rotates it.

Homebrew goes further and sends no identifier at all. We considered that,
but crash rate per install and version-adoption curves need to distinguish
"one install crashing 100 times" from "100 installs crashing once", and
that distinction is most of the value of crash telemetry. A random local
UUID is the established middle ground (DVC, Next.js); machine-derived IDs
(hashed MAC, persistent device IDs) are the variant that draws criticism,
and we do not use one.

Server side, IPs are stripped at ingestion and never stored
(NOFireAI/engineering#1002).

### 3. Consent flow: an explicit Y/n ask, default yes

The first interactive invocation (stdin and stderr on a TTY) prints the
consent ask (text in [docs/telemetry.md](../telemetry.md)) and waits for an
answer: `[Y/n]`, so a single enter approves. The answer is persisted in
`telemetry.json` together with a consent version, and nothing is sent
before the user answers. The prompt names the product the user actually
installed (the wrappers set `URUNC_TELEMETRY_PRODUCT`).

Non-interactive invocations never block on a prompt: telemetry defaults to
on, which is what unattended setups (eg. an agent installing us from the
tap) expect. For explicit control in scripts and installers:

- `--unattended` skips the y/n even on a TTY;
- `--dnt` records an opt-out, so `--unattended --dnt` is the silent
  opted-out install;
- the env vars below work everywhere, no flags needed.

If we ever want to collect more than schema v1, we do not just start
sending it: the consent version bumps, the next interactive invocation
re-asks with the expanded ask, and until the user re-consents the client
sends nothing at all -- a yes to an older ask never covers an expanded
schema, and we prefer silence over guessing. Installs that never see a
TTY stay silent until one does. This is the answer to "how do we grow
collection without the usual backlash": the community objects to silent
expansion, not to being asked again.

### 4. Opt-out surface

Any one of the following disables all telemetry, usage and crash alike:

- `urunc-macos telemetry off` (persisted in `telemetry.json`; `on` and
  `status` complete the subcommand)
- the `--dnt` flag
- `URUNC_TELEMETRY_DISABLED=1`
- `DO_NOT_TRACK=1` (the consoledonottrack.com convention)

`URUNC_TELEMETRY_DEBUG=1` prints every payload to stderr instead of
sending it.

### 5. Transport: fire-and-forget, never in the way

The ingestion endpoint is a stock OpenTelemetry collector (OTLP/HTTP);
each event ships as one log record with the flat payload as its body,
so the server side needs no custom code. Usage events are a single POST
with a 2s timeout, no retries, and silent failure, delivered from a
background goroutine so an unreachable endpoint never stalls the
command; exit paths wait for in-flight events up to that same 2s (the
flush returns the instant delivery completes, so a reachable endpoint
adds no perceptible delay) and then drop them. Matching the flush to
the send timeout matters for short-lived invocations -- a detached
`run` exits right after launch, and a shorter grace dropped its start
and command events when a cold TLS POST to the CDN-fronted endpoint ran
long. Only a 2xx response counts as delivered. Telemetry must never
slow down, block, or break a user command, and must never print errors.

Crash reports are written locally to `~/.urunc-macos/crashes/` at panic
time and uploaded in the background on the next invocation; a report
leaves the queue only on a 2xx response, so rejected or interrupted
uploads retry later. This sidesteps flushing on the
exit path (urunc-macos has exit paths that bypass defers) and works even
when the crash killed networking. The local queue is capped; oldest files
are dropped first.

Child invocations of our own binary (the network-gateway daemon, compose
self-exec) suppress their own events via an internal env var, so one
`compose up` counts once.

### 6. Retention and disclosure

Events and crash reports are retained for 365 days; only aggregates
survive beyond that. The schema page is the canonical disclosure and every
schema change bumps `schema_version` and updates it; changes that expand
what we collect also bump the consent version (§3). urunc-macos is a
private repo, so the schema page will be mirrored somewhere public --
the homebrew-nofire tap is the natural home once it opens
(NOFireAI/engineering#1001 tracks the final location).

## Threat model

The endpoint cannot authenticate its clients, by construction: any
credential we ship -- an API key in a formula, a token or client
certificate in the binary, the checksum salt -- is extractable from a
public artifact, and macOS offers no practical remote attestation for
a CLI. Every client-side telemetry system has this property; ours is
not special. The design question is therefore not how to prevent
forged submissions, but what a forger gains and how we bound it.

**Assets and adversary.** The only asset behind the endpoint is the
analytics data itself. Telemetry is write-only and advisory: nothing
sensitive is stored, nothing about users can be read back, and no
product behavior depends on it. An attacker who uses the endpoint can
pollute dashboards (fake installs, skewed distributions) or burn
storage. The worst case is lies in a Grafana panel, not a breach.

**Defenses, in depth.**

1. The salted checksum (section 1) drops naive garbage at ingestion:
   accidental posts and low-effort scripts that never read the binary.
2. Schema validation and payload size caps at the collector bound what
   any single record can carry.
3. The production exposure (NOFireAI/engineering#1002) must put per-IP
   rate limiting and body-size limits in front of the collector. This
   kills flood and storage-cost attacks with no client change.
4. Analysis treats telemetry as statistical evidence, never as
   authoritative records: dedupe by install ID, cap per-install volume
   at query time, cross-check `version` against real releases, alert
   on volume anomalies. Moving a conclusion requires mimicking
   plausible distributions across many install IDs -- expensive, and
   for no prize.

**Escalation path, deliberately not built.** If real abuse shows up, a
first-run registration handshake (the server issues a token bound to
the install ID; ingest requires it) converts anonymous forgery into
visible, throttleable, revocable identity. Still spoofable -- an
attacker can register installs -- but the server then sees every actor.
The client's first-run state machinery makes this cheap to retrofit;
we build it when abuse is observed, not before.

## Consequences

- Users get a documented, verifiable, multi-way-off telemetry surface, and
  interactive users are explicitly asked before the first byte leaves the
  machine.
- We get backend distribution, boot success rate and crash-per-version --
  the numbers we expect to act on first when refining releases.
- The client must be wired carefully around two quirks: compose self-exec
  and the gateway daemon (suppression), and the bare `os.Exit` in `exec`
  (no flush-at-exit assumptions). Both are recorded in
  NOFireAI/engineering#1004.
- Default-on for non-interactive invocations is the assertive end of
  community practice. We accept it because unattended installs are a core
  use case for us, and we pair it with an explicit interactive ask, a
  one-flag silent opt-out (`--unattended --dnt`), `DO_NOT_TRACK`, and a
  re-consent gate on any future expansion of collection.
- The ingestion endpoint (NOFireAI/engineering#1002) becomes a small piece
  of prod surface we operate, with size limits, rate limiting and schema
  validation, since it is public by nature.
