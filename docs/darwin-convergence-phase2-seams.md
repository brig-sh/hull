# Phase 2 design proposal: platform seams for a shared Exec

Status: design only (2026-07-09), for review before any `Exec`-path code
changes. Follows `darwin-upstreaming-analysis.md` and the completed spikes in
`darwin-convergence-progress.md` (unified QEMU backend, unified VMM factory,
shared OCI-spec/annotation parsing).

## Goal

Let darwin run the **same** `Unikontainer.Create`/`Exec` flow as Linux, so the
orchestration engine (`unikontainers.go`) stops being `//go:build linux`-only
and darwin stops carrying the parallel `run_darwin.go`. The parts that
genuinely differ per platform become three interfaces; everything else — spec
parsing (already shared), annotation handling (already shared), unikernel
params, VMM command building (already shared) — is common code.

## Anatomy of today's Linux `Exec` (what must be factored)

From `pkg/unikontainers/unikontainers.go:309` (`Exec`), in order:

| Step | Current call | Nature |
|---|---|---|
| 1 | `u.SetupNet()` → `network.NewNetworkManager` + `NetworkSetup` | **network seam (exists)** |
| 2 | `ChooseRootfs(...)` | rootfs *selection* — mostly neutral, but pulls `rootfsSelector` from the linux-tagged `rootfs.go` |
| 3 | `rfsBuilder.preSetup()` / `postSetup()` (a `rootfsBuilder` interface already at `rootfs.go:38`) | **rootfs seam** — bind mounts |
| 4 | `prepareMonRootfs(monRootfs, monitorPath, dataPath, needsKVM, needsTAP)` | **rootfs seam** — copy monitor in, mknod `/dev/kvm` `/dev/net/tun` |
| 5 | mount-namespace check → `pivot_root` | **rootfs seam** — Linux-only |
| 6 | `setupUser(u.Spec.Process.User)` (setuid/setgid) | **launcher seam** — Linux-only |
| 7 | `u.ExecuteHooks("StartContainer")` | OCI hooks — neutral (spawns hook binaries) |
| 8 | `vmm.BuildExecCmd(...)` | **shared** (already unified) |
| 9 | `u.SendMessage(StartSuccess)` (IPC to the containerd shim) | **launcher seam** — Linux-only |
| 10 | `vmm.PreExec(...)` then `syscall.Exec(vmm.Path(), argv, env)` | **launcher seam** — Linux replaces the process; darwin spawns |

Darwin equivalents: step 1 → vmnet / user-mode gateway (impl exists,
`network_darwin.go`); steps 2–5 → stage a virtiofs/9p directory or ext4 image,
no mknod, no pivot; step 6 → the guest's `urunit` handles the user; step 9 →
no shim; step 10 → `exec.Command(...).Start()` + wait (or spawn `vz-runner`),
not `syscall.Exec`.

## The three seams

### 1. Network — already the right shape, just needs to be reachable
`network.Manager` already exists and `SetupNet` already consumes it; the
Linux (`network_linux.go`, netlink/TAP) and darwin (`network_darwin.go`,
vmnet/gateway) implementations already satisfy it. The only work is that
`SetupNet` + `getNetworkType` currently live in the linux-tagged
`unikontainers.go`; they move to shared code unchanged.

```go
// exists today in pkg/network
type Manager interface {
    NetworkSetup(uid, gid uint32) (*UnikernelNetworkInfo, error)
}
```

### 2. Rootfs — new interface (Linux already has an internal `rootfsBuilder`)
Owns realizing the chosen rootfs and undoing it. Linux implements bind-mount +
`prepareMonRootfs` + `pivot_root`; darwin stages a virtiofs/9p dir or ext4
image and mounts nothing on the host.

```go
type RootfsPreparer interface {
    // Realize prepares the guest root for the selected RootfsParams and
    // returns the params the VMM should use (paths may change after staging).
    Realize(sel types.RootfsParams, spec *specs.Spec, vmm types.VMM) (types.RootfsParams, error)
    // Cleanup reverses Realize (unmounts, removes staged images). Called on
    // teardown / error.
    Cleanup() error
}
```
`ChooseRootfs` (selection) stays shared; its only Linux coupling is the
`rootfsSelector` helper in `rootfs.go`, which moves to shared code (it is
pure decision logic over annotations + `SupportsFS`/`SupportsBlock`).

### 3. Launcher — new interface, the deepest split
Owns "make the monitor run", including the process model.

```go
type Launcher interface {
    // Launch runs the monitor built by vmm.BuildExecCmd. On Linux it applies
    // setupUser, notifies the shim (StartSuccess), runs PreExec, and
    // syscall.Exec's into the monitor (replacing the reexec'd, namespaced
    // process). On darwin it spawns the monitor (or vz-runner) as a child,
    // wires the console/QMP, and waits.
    Launch(spec LaunchSpec) error
}

type LaunchSpec struct {
    VMM     types.VMM
    Argv    []string
    Env     []string
    Process specs.Process   // for setupUser (Linux)
    Detach  bool            // darwin foreground vs detached
}
```

## Target shared `Exec` (sketch)

```go
func (u *Unikontainer) Exec(metrics m.Writer) error {
    net, err := u.SetupNet()                 // seam 1 (exists)
    ...
    sel, err := ChooseRootfs(...)            // shared selection
    eff, err := u.rootfs.Realize(sel, ...)   // seam 2
    ...
    ukParams := buildUnikernelParams(...)    // shared (uses shared config)
    argv, err := vmm.BuildExecCmd(vmmArgs, unikernel)  // shared (unified)
    _ = u.ExecuteHooks("StartContainer")     // shared
    return u.launcher.Launch(LaunchSpec{...})// seam 3
}
```
`u.rootfs` / `u.launcher` are set by a platform constructor
(`newLinuxPlatform()` / `newDarwinPlatform()` in `_linux.go`/`_darwin.go`),
selected in `New`/`Get`.

## The `Create` / reexec asymmetry (must be explicit)

On Linux, `Exec` runs **inside** a reexec'd, namespaced process; `Create`
does the namespace/reexec setup (`FormatNsenterInfo`, `CreateConn`, the
init-socket dance). Darwin has none of this — no namespaces, no reexec, no
shim. Two honest options:

- **A. `Create` becomes a platform seam too.** `LinuxCreate` does the reexec
  plumbing; `DarwinCreate` is near-empty (write state, prepare bundle). `Exec`
  is then genuinely shared. Cleanest end state; most work.
- **B. Keep the reexec/IPC path Linux-only and have darwin call `Exec`
  directly** (no `Create` reexec). `Exec` is shared; the pre-`Exec` context
  setup is platform-specific and lives outside the shared function.

Recommend **B** first (smaller, lower risk): the shared surface is `Exec`
minus the shim IPC (which moves into the Launcher). `Create`/`kill`/`delete`
stay platform-specific until/unless a macOS OCI-lifecycle consumer appears
(the S1/S2 question) — at which point **A** is the finish.

## Migration plan (strangler — no behavior change per step)

1. **Introduce the interfaces**; implement `linuxRootfs` / `linuxLauncher`
   that call the *existing* code verbatim. Route the current `Exec` through
   them. Linux output/behavior is byte-identical — guarded by the
   unikontainers unit tests + the e2e suite. No darwin change yet.
2. **Move the now-neutral helpers** (`SetupNet`, `getNetworkType`,
   `ChooseRootfs`, `rootfsSelector`, `buildUnikernelParams`) out of the
   linux-tagged file into shared files. Still Linux-only-reachable until 3.
3. **Add `darwinRootfs` / `darwinLauncher`** (stage 9p/virtiofs/ext4; spawn
   monitor / vz-runner; build the `root=` cmdline that `run_darwin` is missing
   today). Wire `newDarwinPlatform`.
4. **Route the darwin path through the shared `Exec`**; delete
   `run_darwin.go`'s duplicated body (keep the CLI command shell). Optionally
   implement `create/start/delete/kill` on darwin if S2.
5. Each step: `go build` darwin + `GOOS=linux go build ./pkg/...`
   (amd64/arm64); `go test`/`go vet` both; boot a QEMU **and** a Vz guest on
   macOS under SIP; run the Linux hypervisor + unikontainers unit tests.

## What stays platform-specific (by design)
Network realization, rootfs realization, and the launch/process model — the
three seams. That is correct: darwin genuinely cannot use netlink,
`pivot_root`, `mknod /dev/kvm`, the shim IPC, or `syscall.Exec`-into-init. The
win is that these are *implementations behind shared interfaces*, not a second
copy of the engine.

## Risks
- `Exec` is the hot path every Linux monitor depends on; step 1 (verbatim
  wrap) is the risk-controlling move — it must not change a byte of Linux
  behavior. Strong reliance on the existing unit + e2e tests.
- `pivot_root`/mount-namespace logic is subtle; keep it entirely inside
  `linuxRootfs`, moved not rewritten.
- The darwin Launcher must reproduce the terminal/stop-grace handling already
  solved in the product's `run.go` (raw tty, SIGTTOU, `--stop-grace`) — reuse
  that logic rather than reinventing.

## Open questions for review
1. **S1 vs S2**: should darwin implement the full OCI lifecycle
   (`create/start/delete/kill`), or stay `run`-scoped? Decides whether we go
   to migration step 4-with-lifecycle or stop at a shared `Exec`.
2. Is option **B** (reexec stays Linux-only, darwin calls `Exec` directly)
   acceptable as the first milestone, with **A** deferred?
3. Should `cmd/urunc` on darwin be a real runtime at all, or should the
   product keep driving via the private `run.go` and `cmd/urunc` darwin be
   dropped? (If dropped, Phase 2 is "make the engine buildable/usable on
   darwin for the product to call", not "make `cmd/urunc` boot".)
