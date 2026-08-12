# ADR 0002: Compose-spec conformance suite with percentage scoring

**Status**: Accepted
**Date**: 2026-07-29
**Context**: How to measure and publish, continuously and honestly, how much of the docker-compose spec `urunc-macos compose` supports, what diverges, and what the security tradeoffs of each supported capability are

---

## Context

`urunc-macos compose` implements a deliberate subset of the compose spec: a
hand-rolled YAML parser (`cmd/urunc-macos/compose.go`) with an 11-key service
allowlist, one flat network per project, bind mounts only, TCP-only port
publishing, and urunc-specific extensions (`x-hypervisor`,
`x-healthcheck-tcp`). That subset is documented prose-style in
`docs/compose.md` and `docs/compose-support.md`, but nothing enforces the
docs, and they have already drifted (docs claim `x-hypervisor` defaults to the
image annotation; the code forces `vz`).

Three problems compound:

1. **No canonical capability map.** The compose spec defines ~70 service-level
   keys, 8 top-level elements, and a CLI verb set. Nobody can say today what
   fraction we support, and new spec keys arrive silently.
2. **Unsupported is sometimes silent.** `warnIgnoredKeys` only walks
   `services.<name>.*`; top-level `networks:`, `volumes:`, `secrets:`,
   `configs:`, `include:`, `profiles:` are dropped with no warning. A user's
   compose file can lose whole sections without a whisper.
3. **Supported capabilities carry security tradeoffs nobody has written
   down.** `:ro` bind mounts are accepted but mounted read-write (a warn, then
   guest writes reach the host). Ports bind `127.0.0.1` by default (stricter
   than Docker's `0.0.0.0` — a divergence that is a security *positive*).
   Environment values are persisted in plaintext state JSON. None of this is
   surfaced where a user deciding whether to trust a workload would look.

Existing test infrastructure: `go test ./...` runs in CI on `macos-15`
(GitHub-hosted, no VM boot); Python e2e harnesses (`test/share-test.py` et
al.) require a self-hosted runner that is currently offline. Only 4 compose
unit tests exist. Any conformance suite must therefore have a tier that runs
without booting VMs, or it will not run at all in practice.

## Decision

Build a manifest-driven, black-box conformance suite with percentage scoring,
in four parts.

### 1. Capability manifest — the single source of truth

`test/conformance/capabilities.yaml` enumerates the full compose-spec surface,
pinned to a spec version. Three areas: top-level elements, service-level keys,
and compose CLI verbs. Each entry:

```yaml
- id: service.volumes.bind
  area: volumes
  status: partial            # supported | partial | unsupported
  notes: >
    Bind mounts only, short syntax only. Guest path must be absolute,
    no spaces. ":ro" accepted but NOT enforced (mounted rw, warns).
  security: >
    Host filesystem exposed to guest via virtiofs. Read-only intent is
    not enforced: a compromised guest can write to any bound host path.
  test: TestConformance/service.volumes.bind
```

Every `supported` or `partial` entry MUST carry a `security` field (may be
"none identified", never absent). Every entry MUST name a test.

A schema-sync guard test diffs the manifest's key coverage against a vendored
snapshot of the compose-spec JSON schema (`test/conformance/spec/`), so a new
spec key that isn't in the manifest fails CI instead of going unmapped.

### 2. Conformance invariant — every claim is executable

One Go table-driven suite (`test/conformance/`) runs the real binary
black-box, one case per manifest entry:

- `supported` → the capability behaves per spec.
- `partial` → the *exact documented divergence* reproduces (e.g. `cpus: 0.5`
  rounds up to 1 vCPU; `ports` binds `127.0.0.1`). If the implementation
  later improves, the test fails and forces a manifest promotion.
- `unsupported` → the binary produces a loud, actionable error or warning.
  Silent dropping is a conformance *failure*.

Two tiers, selected by environment:

- **Static tier** (default, runs in CI on GitHub-hosted macOS): exercises
  parsing, validation, and normalization without booting VMs, via a new
  `compose config` subcommand (validate + print effective config), which this
  epic adds. Most of the matrix — key acceptance, rejection messages,
  divergent value normalization — is verifiable here.
- **Runtime tier** (`URUNC_CONFORMANCE_RUNTIME=1`, self-hosted runner or a
  developer Mac): boots real VMs to verify behavior that parsing can't —
  port publishing, DNS/service discovery, healthcheck gating, depends_on
  ordering, volume writes. Uses the existing `URUNC_TEST_IMAGE` /
  `URUNC_STORE_DIR` env contract from `test/share-test.py`.

To make the "unsupported = loud" invariant true, the epic also fixes the
silent top-level key drop: `warnIgnoredKeys` (or its replacement) must warn on
every unrecognized or unsupported key at any level.

### 3. Percentage scoring

`score = (supported×1 + partial×0.5) / total`, computed over manifest entries
whose backing test passed — a claim that doesn't verify contributes 0.
Reported overall and per-area (services, networking, volumes, lifecycle, CLI,
…). No importance weighting.

### 4. Generated conformance report

A generator (`go run ./test/conformance/cmd/report`) renders
`docs/compose-conformance.md` from the manifest: percentage banner, per-area
tables of capability / status / divergence notes / security tradeoffs. CI
regenerates and diffs — a stale report fails the build. `docs/compose.md`
keeps the narrative and links to the generated matrix instead of duplicating
it.

## Rejected alternatives

- **Adopt `compose-spec/compose-go` as the parser and inherit its
  validation.** Replacing the parser is a separate, much larger decision
  (already an open question in `docs/compose-support.md`). A conformance
  suite must measure the implementation we have, not presuppose a rewrite.
  Vendoring only the spec's JSON schema gives the canonical key enumeration
  without the dependency. If compose-go is adopted later, the manifest and
  suite carry over unchanged — that's the point of black-box.
- **Docs-only compatibility matrix.** Already tried implicitly
  (`docs/compose.md`) and already drifted. Unenforced docs are the problem,
  not the solution.
- **Reuse docker compose's own e2e suite.** It is tied to the Docker daemon
  API, assumes `build`/`exec`/multi-network everywhere, and is pass/fail per
  scenario, not percentage-per-capability. Near-zero salvage.
- **Python harness (extend `test/share-test.py` style).** The manifest diff,
  schema-sync guard, and report generator are natural Go; `go test ./...`
  already runs in CI while the Python harnesses are gated on an offline
  self-hosted runner. Runtime-tier cases still shell out to the real binary,
  preserving the black-box property.
- **Importance-weighted scoring.** Subjective, unstable, and invites gaming.
  Per-area breakdown conveys the same nuance objectively.

## Consequences

- New user-visible subcommand `compose config` (spec-standard verb; also
  independently useful for debugging compose files).
- Behavior change: previously silent top-level key drops become warnings.
- CI gains two gates: schema-sync (new spec keys must be mapped) and report
  freshness (docs can't drift from the manifest).
- The manifest becomes the place where every future compose feature lands
  first: status flip + security note + test, then implementation.
- Runtime tier stays skippable until the self-hosted runner returns; the
  published percentage will note which entries were verified statically vs
  at runtime.
- Fixture needs for the runtime tier (TCP listener, stdout emitter,
  two-service pair) come from existing `urunc-images` artifacts where
  possible; net-new fixture images are out of scope for this epic and the
  affected runtime cases skip with an explicit reason.
