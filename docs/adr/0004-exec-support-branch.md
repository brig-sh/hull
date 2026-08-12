# ADR 0004: exec-support experiment branch to measure exec-driven conformance gain

**Status**: Accepted
**Date**: 2026-07-30
**Context**: Test the hypothesis that guest exec support (upstream branch `feat/exec-runc-delegate`) unlocks significant compose-spec coverage, on a long-lived `exec-support` branch, without destabilizing main

---

## Context

The conformance report (ADR-0002/0003) scores urunc-macos at 9.3% of
in-scope capabilities. A cluster of misses blames the same gap: the manifest
says "there is no guest exec facility" for `cli.exec` and exec-form
`service.healthcheck`, and the lifecycle hooks (`post_start`, `pre_start`,
`pre_stop`) and `cli.top` are likewise exec-shaped.

Two facts complicate the simple story:

1. **Instance-level exec already exists on main.** `urunc-macos exec` talks
   agentproto to `/urunit-agent` in the guest (vsock port 1024 on Vz,
   virtio-serial `io.urunc.agent.0` on QEMU), with `--tty/--cwd/--user/--env`.
   The manifest notes predate it and are stale. What is genuinely missing is
   the **compose layer** (no `compose exec`, healthcheck exec form warns and
   is dropped, hooks unimplemented) and **reliability**: each exec opens a
   fresh connection on stream 1; on QEMU's single-peer virtio-serial bridge,
   reconnects lose frames.
2. **Upstream fixes exist but were built for the Linux path.**
   `NOFireAI/urunc@feat/exec-runc-delegate` (0271adb, from engineering#568)
   adds the per-container exec multiplexer (one owner of the port, stream-id
   fan-out — exactly the QEMU reconnect fix), the agent exit/output-drain
   race fix, hostname via the kernel `ip=` parameter, and directory volume
   copies. It took upstream critest from ~nothing to 45/49 conformance specs.
   But it builds on the pre-review exec commit, is explicitly not PR-ready
   upstream, and urunc-macos pins the fork's `darwin-integration` branch —
   compatibility of the two trees is unproven. The only agent-bearing
   published image (`busybox-agent-qemu-linux-raw`) is x86_64/QEMU, which on
   Apple Silicon means TCG emulation.

The user's hypothesis: exec support yields significant coverage gain. That
is a measurable claim, and the conformance suite exists precisely to measure
it. It should be measured on a branch, because the dependency is an
unfinished upstream experiment that must not reach main's go.mod.

## Decision

Run the experiment on a long-lived `exec-support` branch; main receives only
this ADR and a stale-note correction. The suite is the measuring instrument;
the deliverable is a measured before/after conformance delta.

### 1. Branch discipline

- `exec-support` branches from current main. All epic work lands there via
  PRs targeting `exec-support` (issue-first, CI green — the repo rules apply
  unchanged). CI MUST run on PRs against `exec-support` exactly as it does
  against main: the workflow's unfiltered `pull_request` trigger already
  covers PRs to any base, `exec-support` is added to the `push` trigger so
  the branch itself gets post-merge runs, and the branch-protection ruleset
  is extended to `exec-support` so merging requires a PR with the same
  three checks green.
- Main gets: this ADR, plus a small main-targeted fix correcting the stale
  "there is no guest exec facility" manifest notes (instance exec exists;
  the compose layer does not). Honest notes are a main concern, not an
  experiment.
- The branch's go.mod pins `github.com/nofireai/urunc` at
  `feat/exec-runc-delegate` (0271adb). If that tree does not build against
  darwin-integration expectations, the reconciliation happens in the urunc
  fork (a `darwin-exec` integration branch there), never by vendoring hacks
  here. If reconciliation exceeds the epic, the epic ends with the blocker
  documented and the compose layer still landed against the current pin —
  the compose layer depends on agentproto, which main's pin already ships.

### 2. What gets built on the branch

- `compose exec [-T] [--user] [--env] [--workdir] SERVICE CMD...` —
  resolves project service → instance, delegates to the existing exec path.
- Spec-standard `healthcheck:` (exec form): `test`/`interval`/`retries`/
  `start_period`/`timeout` via the agent, gating `service_healthy` exactly
  like x-healthcheck-tcp does today. `x-healthcheck-tcp` stays; exec form is
  used when the image carries `/urunit-agent`, and a service whose
  healthcheck cannot run (no agent) fails `up` loudly rather than passing
  vacuously.
- Lifecycle hooks `post_start` / `pre_stop` (exec-based, spec semantics);
  `pre_start` only if it falls out naturally.
- `compose top` if trivial on the agent (`ps` via exec); otherwise left.
- Manifest + conformance cases updated on the branch for every capability
  that moves; runtime-tier cases for exec paths, gated on an agent-bearing
  image (`URUNC_EXEC_TEST_IMAGE`), skipping with explicit reasons otherwise.

### 3. The measurement — the actual deliverable

Published as [`docs/exec-support-conformance-delta.md`](../exec-support-conformance-delta.md)
on the exec-support branch.

- Before/after conformance report on the branch: headline in-scope score,
  per-capability flips, and which flips were verified statically vs at
  runtime vs blocked on fixtures.
- Runtime verification order: aarch64 image with `/urunit-agent` if one can
  be produced from existing urunc-images recipes (Bunny-built, per the
  fixture rule); else the published x86_64 image under QEMU/TCG (slow is
  acceptable for a measurement); else static-only with the gap stated.
- The epic's closing report states plainly whether the "significant
  coverage" hypothesis held: expected motion is cli.exec, healthcheck,
  post_start/pre_stop, possibly top — roughly +3.5 to +5 points on a
  113-capability denominator, i.e. ~9.3% → ~12-14%. If that is the outcome,
  the honest conclusion is "meaningful but not transformative", and the
  report says so.

## Rejected alternatives

- **Do it on main behind a flag.** Main's go.mod would pin an upstream
  branch its own author calls not-PR-ready, whose namespace-confinement
  rework is known-unreconciled. A branch pin is reversible; a main pin is a
  support commitment.
- **Wait for upstream to finish the exec rework first.** The hypothesis is
  about coverage economics — whether exec is worth prioritizing. Measuring
  now, on a branch, is the input to that prioritization; waiting inverts it.
- **Declare exec-dependent capabilities supported based on upstream critest
  results.** critest measures upstream urunc on Linux with containerd; the
  suite here measures this binary's compose layer. ADR-0002's invariant is
  executable claims, not inherited ones.
- **Skip the compose layer, only bump the dependency.** The dependency bump
  alone moves zero manifest entries: every missing capability is missing in
  cmd/urunc-macos, not in the library. The bump buys reliability
  (multiplexer), not coverage.

## Consequences

- A standing `exec-support` branch that tracks main (rebased or merged
  forward by whoever works on it) until upstream exec lands properly, at
  which point the branch's compose layer becomes an ordinary PR series to
  main.
- The conformance manifest forks between main and the branch for the
  duration; the branch's generated report is branch-truth and is not
  published over main's.
- CI runs and is required on exec-support PRs exactly as on main; the
  runtime exec cases skip in CI (no agent image there) and run on the mbp.
- Risk accepted: the upstream branch may not reconcile with
  darwin-integration inside this epic; the fallback (compose layer on the
  current pin, single-stream exec, documented) still answers the coverage
  question, since stream multiplexing affects reliability, not capability
  count.
