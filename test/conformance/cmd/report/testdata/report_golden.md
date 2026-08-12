# Compose-spec conformance

<!-- GENERATED FILE — DO NOT EDIT BY HAND. -->
<!-- Regenerate with `make conformance-report`; edit test/conformance/capabilities.yaml instead. -->

This report is generated from [`test/conformance/capabilities.yaml`](../test/conformance/capabilities.yaml) by `go run ./test/conformance/cmd/report`. Do not edit it by hand — change the manifest and run `make conformance-report`.

Measured against the pinned compose-spec schema snapshot (commit `0123456789abcdef`, fetched 2026-07-29); see [`test/conformance/spec/PROVENANCE.md`](../test/conformance/spec/PROVENANCE.md).

## Overall: 37.5% of in-scope capabilities

`score = (supported + 0.5 × partial) / in-scope total` over 4 in-scope capabilities: **1 supported**, **1 partial**, **2 unsupported**.

Full-spec coverage: **30.0%** of all 5 capabilities; 1 are declared out of scope by [ADR-0003](adr/0003-compose-out-of-scope.md) (structurally foreclosed: orchestration, cross-VM namespace sharing, Windows-only, Docker-platform machinery, device passthrough). Out-of-scope keys still warn loudly and stay tested.

**Tier** — `static` entries are verified in CI without booting a VM; `runtime` entries are verified only where the runtime suite runs (`HULL_CONFORMANCE_RUNTIME=1`, self-hosted runner or a developer Mac).

## Scores by area

Scores are over in-scope entries; the out-of-scope column is excluded from the denominator.

| Area | Supported | Partial | Unsupported | Out of scope | Score |
|------|-----------|---------|-------------|--------------|-------|
| cli | 0 | 0 | 2 | 0 | 0.0% |
| deploy | 0 | 0 | 0 | 1 | — |
| services | 1 | 1 | 0 | 0 | 75.0% |

## Out of scope (ADR-0003)

These capabilities are structurally foreclosed, not backlog. They still warn loudly and keep their conformance tests.

| Capability | Area | Category | Notes |
|------------|------|----------|-------|
| `service.deploy` | deploy | orchestration | Swarm-style deployment. Out of scope (ADR-0003): orchestration. |

## Capabilities

### cli (0.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `cli.down` | unsupported | static | Also not a subcommand. | — |
| `cli.up` | unsupported | static | Not a subcommand. | none identified |

### deploy (—)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.deploy` | out-of-scope | static | Swarm-style deployment. Out of scope (ADR-0003): orchestration. | — |

### services (75.0%)

| Capability | Status | Tier | Divergence notes | Security tradeoffs |
|------------|--------|------|------------------|--------------------|
| `service.alpha` | partial | runtime | Folded scalar with a newline and extra spaces. Pipe a\|b stays literal. | Host path exposed; read-only intent not enforced. |
| `service.zeta` | supported | static | — | none identified |

