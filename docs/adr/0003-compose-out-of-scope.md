# ADR 0003: Declare permanently out-of-scope compose capabilities

**Status**: Accepted
**Date**: 2026-07-29
**Context**: Which compose-spec capabilities hull will never pursue, so the conformance score measures intent instead of the whole spec

---

## Context

ADR-0002's conformance suite scores hull against the full compose-spec
surface: 134 capabilities, 7.8% today. That denominator includes capabilities
we will never build, for structural reasons, not effort reasons:

- hull is a single-host macOS dev tool. There is no swarm, no
  orchestrator, and no plan for one.
- Every service is its own VM. Cross-container namespace sharing (ipc:,
  pid:, network_mode: service:/container:/host) cannot exist across VMs.
- Some keys are Windows-container-only or assume the Docker daemon platform.

(Image building and the shared-kernel cgroup/capability knobs were
considered for this list and deliberately kept in scope for now: Bunny
integration is plausible roadmap, and some kernel knobs may map onto per-VM
sizing or in-guest enforcement.)

Leaving the structural exclusions as "unsupported" makes 7.8% read as a
backlog. It is not: a meaningful slice of the misses are things we are
deliberately not doing. The score should
separate "not yet" from "not ever", without weakening ADR-0002's invariants —
in particular, every key in a user's file must still warn loudly, and every
schema key must still be mapped in the manifest.

## Decision

Add a fourth manifest status, `out-of-scope`, and re-base the headline score
on in-scope capabilities only.

### 1. The out-of-scope set (21 of 134)

Five categories, each the reason recorded in the entry's notes:

1. **Orchestration** — this is a single-host tool: `service.deploy`,
   `service.scale`, `cli.scale`.
2. **Cross-container namespace sharing; impossible across VMs**:
   `service.ipc`, `service.pid`, `service.uts`, `service.network_mode`
   (host/service:/container: modes), `service.links`,
   `service.external_links` (the latter two also deprecated by the spec).
3. **Windows containers only**: `service.isolation`,
   `service.credential_spec`, `service.cpu_count`, `service.cpu_percent`,
   `service.storage_opt`.
4. **Docker-platform machinery that has no counterpart here**:
   `service.use_api_socket` (Docker API socket mount), `service.provider`
   (provider plugins), `service.runtime` (alternate OCI runtime — urunc *is*
   the runtime), `service.models`, `toplevel.models` (Docker Model Runner).
5. **No device passthrough in Apple Virtualization.framework**:
   `service.devices`, `service.gpus`. Revisit if Apple ships VM device/GPU
   passthrough for Linux guests.

Everything else stays in scope, including things that are hard but
legitimate roadmap: image building via Bunny integration (`build`,
`cli.build`, `cli.push`), the shared-kernel resource and capability knobs
(cgroup limits, `cap_add`/`cap_drop`, `privileged`, `security_opt` — some
may map onto per-VM sizing or in-guest enforcement later), secrets/configs,
restart policies, exec healthchecks, env_file/interpolation, custom
networks, hostname/dns keys, `user`, `read_only`, `sysctls`,
`develop`/`cli.watch`, `platform`, and the remaining CLI verbs.

### 2. Semantics — scoping is not silencing

`out-of-scope` changes bookkeeping, nothing user-visible:

- The binary still warns loudly on every out-of-scope key (ADR-0002's
  "unsupported = loud" invariant applies verbatim; the warning hints may say
  "not planned" instead of "not supported yet").
- The static conformance tests for these keys remain: they assert the loud
  warning, exactly as for unsupported.
- The schema-sync guard still requires every schema key to have a manifest
  entry; `out-of-scope` satisfies it.
- Well-formedness guard: an `out-of-scope` entry MUST name one of the five
  categories above in its notes; `security` stays mandatory only where it is
  today (supported/partial).

### 3. Scoring

- **Headline (in-scope) score** = (supported + 0.5×partial) / in-scope
  total, where in-scope = total − out-of-scope. Today: 10.5 / 113 = **9.3%**.
- The report keeps a **full-spec coverage** line ((supported + 0.5×partial) /
  134 = 7.8%) so nobody can accuse the number of quietly shrinking its
  denominator, plus a dedicated out-of-scope section listing every entry with
  its category.

## Rejected alternatives

- **Delete out-of-scope entries from the manifest.** Breaks the schema-sync
  guard's purpose (every spec key mapped), erases the reasoning, and lets
  the score inflate invisibly. The whole point is a recorded, auditable
  decision.
- **Keep them `unsupported` with an annotation field.** Leaves the headline
  number at 7.8% and the "backlog" misreading intact; a status the guards and
  the report can act on is strictly clearer.
- **Weighted scoring by importance.** Already rejected in ADR-0002;
  subjective and gameable. Scoping is a binary product decision, not a
  weight.
- **A larger cut (secrets, restart, healthcheck, networks, env_file).**
  Those are plausible roadmap for a dev tool; declaring them out-of-scope
  would misrepresent intent and require walking the ADR back later. Only
  structurally-foreclosed capabilities make the list.

## Consequences

- Headline conformance becomes 9.3% today and now moves only when we ship
  in-scope work; full-spec coverage stays printed alongside.
- The manifest records *why* each exclusion exists; a future maintainer can
  re-scope by editing one entry and its notes (category 5 has an explicit
  revisit trigger).
- Manifest loader, well-formedness guard, static harness registry (the
  data-driven unsupported-key cases must also cover out-of-scope entries),
  report generator, and the generated doc all change; `docs/compose.md`
  gains one paragraph on the scoping policy.
- No behavior change in the binary beyond optional wording tweaks to
  warning hints.
