# exec-support: measured conformance delta (ADR-0004)

Historical measurement from the `exec-support` experiment (epic #69),
taken on that branch before it landed on main. The numbers below are the
branch's own before/after at the commits named; they are NOT main's current
score, which has since moved with other epics (see
`docs/compose-conformance.md` for today's figure). Runtime results are from
live VM runs on an Apple Silicon host (Vz backend).

The exec layer landed on main by replaying these commits onto it, without
the branch's experimental `nofireai/urunc@darwin-exec` pin: the layer needs
only `agentproto`, which main's pin already ships, so the reconciliation
that pin carried remains a separate upstream decision.

## The question

Engineering#568's hypothesis: guest exec support unlocks *significant*
compose-spec coverage.

## Before / after

| | main (`f6bf27f`) | exec-support (this branch) |
|---|---|---|
| In-scope score | **9.3%** (4 supported, 13 partial / 113) | **11.5%** (4 supported, 18 partial / 113) |
| Full-spec coverage | 7.8% | 9.7% |
| Runtime-verified capabilities | 6 of 8 cases (2 fixture skips) | 11 of 13 cases (same 2 skips) |

Five capabilities moved, all `unsupported → partial`, all five **verified by
live VM runs** against an agent-bearing image (not just statically):

| Capability | What works now | Live evidence |
|---|---|---|
| `cli.exec` | `compose exec [-T] [-u] [-e] [-w] SERVICE CMD...` through the guest agent | stdout relay, guest exit-code propagation (exit 7 observed as CLI exit 7), unknown-service rejection |
| `service.healthcheck` | spec-standard exec probes (CMD/CMD-SHELL/NONE + tuning) gating `service_healthy` | dependent created only after the probe passes; a never-passing probe fails `up` with the probe's exit status after its retry budget |
| `service.post_start` | hooks after service start; failure fails `up` | hook write observed on the host through a bind mount; `/bin/false` hook tears the project down |
| `service.pre_stop` | hooks during `down`; failure degrades to a warning | hook artifact written during `down` before the stop |
| `cli.top` | guest process listing via `/bin/ps` through the agent | live listing with PID column |

The measurement also flushed out and fixed a real UX defect class: the
agent starts a beat after the VMM, so every single-shot exec surface
(compose exec, top) needed the same bounded transport retry the probes and
hooks already had.

## What made the runtime verification possible

The published agent image is x86_64-only and hull launches
`qemu-system-aarch64`/HVF exclusively, so it cannot boot here. The fixture
used instead: the Bunny-built `urunc-ubuntu-vz:aarch64` base (its
`urunc.json` identity rides the rootfs) plus `/urunit-agent` cross-built
for linux/arm64 from `nofireai/urunc@darwin-exec` — a one-COPY derived
image. Recipe:

```dockerfile
FROM harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64
COPY urunit-agent /urunit-agent
```

Follow-up worth filing in urunc-images: publish an aarch64 agent-bearing
variant from Bunny so the fixture is a first-class published image instead
of a local graft.

## Verdict on the hypothesis

**Meaningful, not transformative — and below the ADR's own projection.**
ADR-0004 predicted +3.5 to +5 points (~12-14%); the measured gain is +2.2
points (9.3% → 11.5% in-scope), under the projected range, because
`pre_start` was not implemented (nothing needed it) and `cli.top` and the
hooks each carry the 0.5 weight of a partial rather than full support.
Exec support converted the entire exec-shaped capability cluster with real
runtime evidence, but the coverage economics are more modest than the
hypothesis assumed. The remaining
distance to docker-compose parity is dominated by capabilities exec cannot
touch: variable interpolation and env_file, custom networks, restart
policies, and run-path spec application (entrypoint, user, working_dir).

One indirect payoff exceeds the score motion: the `darwin-exec`
reconciliation brought `ProcessConfig` (uid/gid/cwd) and guest-hostname
plumbing into the shared library (`buildExecContext` on the linux path;
hull reaches the same plumbing through `BuildUrunitConfig`, which it
already calls). That is exactly the foundation `service.user`,
`service.working_dir`, `service.entrypoint` and `service.hostname` need —
four more capabilities, without new upstream work. That is the
highest-leverage next step this measurement surfaced.
