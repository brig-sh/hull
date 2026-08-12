# Darwin support: convergence analysis & upstreaming plan

Status: analysis only (2026-07-09). No code changes proposed as committed —
this is the response to review feedback that the darwin port is a parallel
rewrite rather than genuine darwin support of upstream urunc.

## Verdict

The review is correct. On `darwin-integration`, the darwin build of `urunc`
shares with upstream only: the `types` interfaces, three of five unikernel
types, `UruncConfig` parsing, and `pkg/qmp`. Everything else — the OCI
lifecycle, spec/annotation handling, network, rootfs/mounts, guest-rootfs
selection, and even the hypervisor backends — is a separate, darwin-tagged
implementation. It is, structurally, a second program in the same repo.

## Evidence

| Concern | Linux (upstream) | Darwin (fork) | Shared? |
|---|---|---|---|
| Orchestration engine | `unikontainers.go` (`New`/`Create`/`Exec`, 1200+ lines) | **excluded** — file is `//go:build linux` | ✗ |
| OCI lifecycle | `create`/`start`/`delete`/`kill` → `Unikontainer` | `run_darwin.go` standalone; `create/start/delete/kill_darwin.go` are 21-line **error stubs** | ✗ |
| OCI spec / annotations | parsed in the linux create path + `urunc_config.go` | re-read minimally in `run_darwin.go` | ✗ |
| Guest-rootfs selection | in `unikontainers.go` (linux) | reimplemented in the run path | ✗ |
| Network | `network_linux.go` (netlink/TAP/tc) | `network_darwin.go` (6 stub funcs) — but both satisfy the shared `network.Manager` interface | interface only |
| Rootfs / mounts | `mount_linux.go` (bind + pivot_root) | `mount_darwin.go` | ✗ |
| Hypervisor backend | `type Qemu` (`qemu.go`) | **`type QemuDarwin`** (`qemu_darwin.go`) — a parallel type, not a method | interface only |
| VMM factory | `vmm.go` registry (linux) | `vmm_factory_darwin.go` (separate `NewVMM`) | ✗ |
| Unikernel types | `linux.go`, `unikraft.go` (linux); `mewz/rumprun/mirage` (shared) | `MinimalLinux` substitutes for both linux & unikraft; mewz/rumprun/mirage reused | 3 of 5 |
| VMM / Unikernel interfaces, `ExecArgs` | `types/types.go` | same | ✓ |
| Config parsing | `urunc_config.go` | `urunc_config_darwin.go` (+ shared struct) | mostly ✓ |
| QMP | `pkg/qmp` (new) | `pkg/qmp` | ✓ |

Net: the common surface is the **leaf abstractions** (VMM, Unikernel, config,
QMP). The **engine** that turns an OCI bundle into a running unikernel is not
shared at all.

## Why it looks like this (fair reading)

`unikontainers.go` is deeply Linux-coupled: network namespaces, netlink TAP +
tc redirect, `pivot_root`, `sethostname`, seccomp, `/proc` plumbing, the
reexec-into-namespaced-init dance, and unix-socket IPC with the containerd
shim. None of that exists or applies on macOS, where the model is: prepare a
rootfs (virtiofs/9p/ext4 image), then spawn a VMM process (QEMU, or the
`vz-runner` helper). Porting the engine wholesale is genuinely hard, so the
author took the pragmatic path — a standalone `run` reusing only the leaf
pieces. That is fine for shipping a macOS product fast; it is not fine for
upstream cohesion.

## A strategic question the plan must answer first

**Who drives `create`/`start`/`delete`/`kill` on macOS?** On Linux it is
containerd via the shim. macOS has no containerd; the product CLI
(`hull`, private repo) calls a `run` path directly. So implementing the
full OCI lifecycle on darwin has, today, **no consumer**. Two honest stances:

- **(S1) Scope darwin to `run`** and say so — the binary is a launcher, not an
  OCI-compliant runtime, on macOS. Less code, honest, but the reviewer's
  "different main, nothing common" stands for `cmd/urunc`.
- **(S2) Implement the lifecycle for parity** so a future macOS driver (a
  containerd-on-mac, or the product CLI speaking OCI) can use it. More code,
  justifies convergence, but speculative until such a driver exists.

The convergence value is highest if we pursue **S2 for the engine** (shared
spec/rootfs/config logic) while accepting darwin will not replicate the
Linux *process/namespace* model.

## What SHOULD stay platform-specific (not a defect)

- Network implementation (vmnet / user-mode gateway vs netlink/TAP) — already
  correctly behind `network.Manager`.
- Rootfs realization (virtiofs/9p/ext4-image vs bind+pivot_root).
- Process launch (spawn VMM / `vz-runner` helper vs `syscall.Exec` into a
  namespaced init). darwin cannot reexec into an in-guest init; forcing the
  Linux model would be wrong.

The goal is not to delete darwin-specific code — it is to make it **plug into
shared orchestration behind interfaces** instead of duplicating the engine.

## Plan — layered, smallest-useful-first

### Phase 0 — upstream only what stands alone (low risk, do first)
Split the current darwin PR stack so the genuinely platform-neutral,
independently-valuable pieces can merge without dragging the parallel runtime:
- `pkg/qmp` promotion (useful on Linux too).
- `types` additions used by both eventually (`SharedDirs`, `BlockDevPath`,
  `UnixSocket`) — additive, no behavior change.
- Pure build-tag hygiene where a file genuinely is Linux-only and the tag
  doesn't change Linux behavior.
Keep `qemu_darwin.go`, `vz_darwin.go`, `run_darwin.go`, and the lifecycle
stubs **out of upstream** for now (they remain in the fork / private repo).
This directly matches the reviewer's "as a reference it's OK."

### Phase 1 — unify the hypervisor backends
Collapse `QemuDarwin` back into `Qemu` with platform seams instead of a
parallel type:
- Keep one `type Qemu`. Move the ~3 genuinely-different concerns —
  accelerator (`-accel hvf` vs `kvm`), netdev (vmnet/stream vs tap), binary
  discovery, no-vhost — behind small `_linux.go`/`_darwin.go` helpers or a
  `platformQemuOpts()` seam.
- The `VMM` interface already matches; this is a merge of two implementations
  of the same interface, ~300 lines → one type + two small platform files.
- Do the same assessment for `vmm_factory`: one registry, platform-tagged
  entries, rather than a separate darwin `NewVMM`.
Medium effort, clear win, removes the most obvious "why two QEMUs" objection.

### Phase 2 — extract a platform-neutral orchestration core
The substantive work that makes darwin a *platform of urunc*, not a fork.
Decompose `unikontainers.go`:
- **Neutral (new, untagged) core**: OCI spec load, annotation/`urunc.json`
  parsing, guest-rootfs-type selection, `UruncConfig` resolution, instance
  state. Today these are trapped inside the `//go:build linux` file.
- **Platform seams (interfaces)** for the parts that must differ:
  - `NetworkManager` — already exists; reuse.
  - `RootfsPreparer` — bind+pivot_root (linux) vs virtiofs/9p/ext4-image
    staging (darwin).
  - `Launcher` — `syscall.Exec` into namespaced init (linux) vs spawn VMM /
    `vz-runner` (darwin).
- `Unikontainer.Create`/`Exec` then call the neutral core + the seams. The
  darwin `run` path becomes a thin caller of the same core, and the lifecycle
  stubs can be implemented (S2) or the binary honestly scoped to `run` (S1).
- Fold `MinimalLinux` back toward the real `Linux` unikernel: the darwin
  cmdline differences (console device, root=, no initrd assumptions) should be
  parameters/branches in `linux.go`, not a separate unikernel type, so darwin
  reuses upstream's `ip=` contract and cmdline builder.

High effort, and the point of highest reviewer value. It is also where the
real blocker lives: the reexec/namespace/IPC model is Linux-only, so
`Exec` must be restructured around the `Launcher` seam rather than tagged.

### Sequencing
Phase 0 is independent and shippable now. Phase 1 is a self-contained refactor
of the fork before re-proposing. Phase 2 is a design spike (define the three
interfaces, prove them by moving the QEMU-on-Linux and QEMU-on-darwin paths
onto them) before committing to the full port.

## Effort / risk

- Phase 0: ~days. Risk: low (additive). Value: gets *something* upstream now.
- Phase 1: ~1–2 weeks. Risk: low-medium (one shared type must keep Linux
  behavior byte-identical; existing hypervisor unit tests guard it). Value:
  removes the duplicate-backend objection.
- Phase 2: ~weeks, plus a design review. Risk: medium-high — touches the core
  `Exec`/`Create` path that every Linux backend depends on; needs strong test
  coverage (the existing unikontainers unit tests + the e2e suite) before and
  after. Value: darwin becomes first-class; this is what "merge" should mean.

## Recommendation

1. Re-scope the upstream PRs to **Phase 0 only**; land the neutral bits.
2. Treat the darwin runtime as fork/product code (reference), explicitly, per
   the reviewer.
3. Do **Phase 1** in the fork as the next step and re-propose the unified
   QEMU backend.
4. Gate **Phase 2** on a decision about S1 vs S2 (does a macOS OCI-lifecycle
   consumer exist or is planned?). Only pursue the full engine extraction if
   S2 is real; otherwise keep darwin honestly `run`-scoped and share only the
   Phase-0/1 surface.

Do not attempt to merge the current darwin runtime as-is — the reviewer is
right that it would add a disconnected second codebase.
