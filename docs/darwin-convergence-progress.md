# Darwin convergence — implementation log

Tracks the alternative approach from `docs/darwin-upstreaming-analysis.md`:
bring the darwin port *into* upstream urunc rather than shipping a parallel
one. Work happens on branch `darwin/converge`; the working
`darwin-integration` branch and the private `urunc-macos` repo are left
intact. Each step keeps Linux behavior byte-identical (guarded by the
existing hypervisor unit tests) and is boot-verified on macOS under SIP.

## Method

1. Read both implementations; identify the true shared surface vs the
   duplicated surface.
2. Collapse the duplication into one type/driver whose **zero value
   reproduces today's Linux output exactly**, so the Linux factory and the
   untagged unit tests are unchanged.
3. Localize the genuine platform differences to explicit fields/branches or
   small per-platform files.
4. Verify: `go build` on darwin + `GOOS=linux go build ./pkg/...`
   (amd64+arm64); `go test` + `go vet` the hypervisor package on both; boot a
   real QEMU guest under HVF via the changed path.

## Phase 1 — unify the QEMU backend (commit: refactor hypervisors QEMU)

### Before
Two independent types implementing the same `types.VMM` interface:

- `Qemu` (`qemu.go`, untagged) — the Linux/KVM backend. ~160 lines.
- `QemuDarwin` (`qemu_darwin.go`, `//go:build darwin`) — a parallel HVF
  backend. ~290 lines, its own `BuildExecCmd`, `Signal`, `Stop`, `Ok`,
  `UsesKVM`, `Path`, `PreExec`, plus QMP accessors.

~70% of the two `BuildExecCmd` bodies were copy-pasted (memory, cpu, smp,
machine type, block-device loop, initrd, extra monitor args, vsock). The
darwin one additionally set the accelerator, firmware path, network backend,
directory-rootfs 9p share, and a QMP socket. Nothing but the interface was
shared; a reader could not tell what QEMU-on-mac actually did differently
without diffing 450 lines.

### After
One `Qemu` type. New platform fields whose **zero values are the Linux
backend**:

```go
darwin    bool   // HVF target vs KVM
accel     string // "" => "-enable-kvm"
firmware  string // "" => "/usr/share/qemu"
qmpSocket string // "" => no -qmp
```

- `NewQemuDarwin` sets these (`-accel hvf`, Homebrew firmware, QMP socket).
- `BuildExecCmd` is one shared skeleton; the four real differences live in
  `q.netCli()` (tap/vhost vs gateway-stream/vmnet), `q.darwinRootfsArgs()`
  (directory-9p / initrd / block), and small `if q.darwin` guards for the
  disk-image boot and QMP socket.
- `UsesKVM()` is `!q.darwin`; QMP accessors are plain methods.
- `qemu_darwin.go` shrinks to just `NewQemuDarwin` + `IsHVFAvailable`.

Net: `-290 / +duplication removed`; the darwin-specific behavior is now
visible as ~30 lines of explicit branches instead of a shadow file.

### Difference vs the previous implementation
- The Linux command is produced by the identical code path as before — the
  pinning test (`qemu_test.go`, which builds a bare `Qemu{}` and asserts
  `-enable-kvm`, tap networking, etc., and runs on *both* platforms) passes
  unchanged.
- A new `qemu_darwin_test.go` locks the darwin output (`-accel hvf`, stream
  netdev, `-qmp`, and the *absence* of `-enable-kvm`/tap).
- Behavior on macOS is unchanged: the emitted QEMU argv is the same string
  the old `QemuDarwin` produced (verified by booting a Linux guest under HVF).

## Phase 1b — unify the VMM factory (commit: refactor hypervisors VMM factory)

### Before
Two `NewVMM` functions and duplicate declarations:

- `vmm.go` (`//go:build linux`) — registry map `vmmFactories` of five
  monitors + an inline Hedge special-case; `NewVMM`; `getVMMPath`; plus
  `DefaultMemory`, `VmmType`, `ErrVMMNotInstalled`, `vmmLog`.
- `vmm_factory_darwin.go` (`//go:build darwin`) — a **second** `NewVMM`
  (switch over QEMU/Vz with a QEMU binary search + HVF check), and **copies**
  of `DefaultMemory`, `VmmType`, `ErrVMMNotInstalled`, `vmmLog`.

Two drivers, two copies of the shared constants; adding a monitor or changing
the driver meant editing both.

### After
- `vmm.go` becomes **untagged**: the shared constants, the `VMMFactory` type
  (extended with optional `precheck` and `pathFunc` hooks), one `NewVMM`
  driver, and `getVMMPath`. It consumes a package-level `vmmFactories` map.
- `vmm_linux.go` (`//go:build linux`) declares the Linux registry (the five
  monitors, with Hedge folded in as a `precheck`+`createFunc` entry — the
  special-case is gone).
- `vmm_factory_darwin.go` (`//go:build darwin`) declares only the darwin
  registry (QEMU with `precheck: requireHVF` + `pathFunc: darwinQemuPath`,
  and Vz), plus those two small helpers.

Constraint honored: `hvt.go`/`firecracker.go` are Linux-only types, so the
registry *map* must stay per-platform; only the driver is shared. The
Linux-tagged `vmm_test.go` reads `vmmFactories[QemuVmm|SptVmm|HvtVmm|
FirecrackerVmm]` directly — those entries are unchanged, so it still passes.

### Difference vs the previous implementation
- One `NewVMM` instead of two; the shared constants exist once.
- The darwin QEMU quirks (HVF gate, prefer re-signed binary copies) are now
  data on the registry entry (`precheck`/`pathFunc`) rather than a bespoke
  function — the same mechanism a future monitor would use.
- Hedge no longer needs a hand-written branch in the driver.
- Verified: Linux `go build`/`go vet` (amd64+arm64) and the darwin build both
  pass; a QEMU guest boots under HVF through the registry-driven `NewVMM`.

## Downstream cost (private repo)
The private `urunc-macos` CLI casts a monitor to `*hypervisors.QemuDarwin`
in one place (`cmd/urunc-macos/run.go`) to reach `GetQMPSocket`. After
convergence that becomes `*hypervisors.Qemu` — a one-line change, applied
temporarily during boot verification and reverted so the private repo stays
on the current pin until the convergence branch is adopted.

## Phase 2 (spike) — share the OCI spec + annotation parsing

The reviewer's first concrete complaint: darwin "doesn't use the OCI spec
parsing, nor the annotations parsing." It was literally true — `run_darwin.go`
hand-rolled an anonymous-struct JSON parser and read ad-hoc annotation keys
(`"vmm"`, `"unikernel"`) while the real parser sat in the `//go:build linux`
`unikontainers.go`/`config.go`.

### Before
- `loadSpec` lived in `utils.go` (`//go:build linux`); `GetUnikernelConfig` +
  the `UnikernelConfig` type + the `com.urunc.unikernel.*` annotation
  constants in `config.go` (`//go:build linux`). None available on darwin.
- `run_darwin.go` parsed `config.json` into a throwaway struct and keyed
  annotations by `"vmm"`/`"unikernel"` — a parallel, less-correct parser that
  ignored the canonical annotations and the `urunc.json` fallback.

### After
- New untagged `spec.go` holds the bundle filename constants, the package
  logger, and `loadSpec` + an exported `LoadSpec`.
- `config.go` is untagged (it was pure parsing: stdlib + runtime-spec only),
  so `GetUnikernelConfig`, `UnikernelConfig`, and the annotation constants are
  now shared. Linux is unaffected (additive).
- `run_darwin.go` parses via `LoadSpec` + `GetUnikernelConfig` — the same code
  as the Linux engine — reading the real `com.urunc.unikernel.*` annotations
  (with the `urunc.json` fallback and base64 decode) and resolving the kernel
  path from the `binary` annotation instead of a hardcoded path.

### Verified
- New untagged `spec_test.go` parses a bundle through `LoadSpec` +
  `GetUnikernelConfig` on darwin.
- Booting the fork's own `cmd/urunc` darwin binary against a real bundle: the
  guest **kernel loads from the annotation-resolved `/.boot/kernel`** — proof
  the shared parse path drives a real boot. (It then panics mounting root,
  because `run_darwin` still doesn't build a `root=…rootfstype=9p…` cmdline —
  see below.)

### Findings the convergence surfaced (value of doing it)
1. The macOS product's bundle records `hypervisor: "qemu-hvf"`, which is not a
   VMM factory key; the old parser hid this by ignoring the annotation. Added
   a `qemu-hvf` alias to the darwin registry.
2. That product bundle's `config.json` `binary` annotation (`"unikernel"`) is
   inconsistent with its own `urunc.json` (`/.boot/kernel`); `GetUnikernelConfig`
   prefers spec annotations, so darwin got the wrong kernel path until the
   annotation was corrected — a real bundle-generation bug in the private
   `ociclient`, invisible before.
3. The fork's `cmd/urunc` darwin `run` was never an end-to-end runner on its
   own — it lacks the rootfs `root=` cmdline and network setup that the Linux
   `Exec` builds. The shipping product boots via the private repo's `run.go`;
   `cmd/urunc` on darwin is the skeleton the review flagged.
4. `config.go`'s `tryDecode` logs at ERROR when a value isn't base64 (plain
   spec annotations trip it). Harmless, pre-existing, now visible on darwin —
   worth lowering to Debug upstream.

## Phase 2 — share the kernel command-line builder (commit: feat darwin boot via shared Linux unikernel builder)

### Before
The darwin unikernel factory mapped `unikernelType: "linux"` to `MinimalLinux`,
a stub whose `CommandString()` echoed only the raw cmdline — no `root=`,
console, `ip=` or urunit config. A container booted the kernel but panicked
mounting root: the "parallel universe" the review flagged, where darwin forked
the command-line logic instead of reusing urunc's.

### After
- `unikernels/linux.go` dropped its `//go:build linux` tag. The builder is pure
  string logic (fmt/net/filepath/runtime/strconv/strings + types/initrd), no
  syscalls, so it compiles unchanged on darwin.
- The darwin factory maps `"linux"` to the real `newLinux()`.
- `initrd/initrd_darwin.go` gained an `AddFileToInitrd` stub (errors) so the
  builder links; only reached for initrd-rootfs images, which darwin doesn't use.
- `hypervisors/qemu.go`: on darwin, resolve the unikernel's guest-absolute
  `ExtraInitrd` (urunit.conf) against the host rootfs dir — there is no
  container mount namespace to make `/urunit.conf` resolve.
- `cmd/urunc/run_darwin.go`: build `UnikernelParams` as the Linux engine does
  and use `CommandString()` for `-append`; realize the rootfs via `Sharedfs`
  (mount tag `fs0`) like the Linux 9pfs backend.

### Verified — first real boot through the shared path
A urunit ubuntu image boots on Apple Silicon under HVF, SIP enabled. The
assembled cmdline is byte-for-byte the shared builder's output
(`panic=-1 console=ttyAMA0 root=fs0 rw rootfstype=9p rootflags=… retain_initrd
URUNIT_CONFIG=/sys/firmware/initrd init=/urunit -- /bin/bash`). Guest:
`VFS: Mounted root (9p filesystem)`, `Run /urunit as init process`, live
`root@(none):/#` shell; QMP ACPI powerdown shuts it down cleanly.

## Phase 2b — share guest-rootfs selection (commit: refactor share guest-rootfs selection)

### Before
`run_darwin.go` decided the rootfs with a hand-rolled `mountRootfs -> 9pfs`
rule, duplicating (and diverging from) urunc's `ChooseRootfs` priority order.

### After
`ChooseRootfs` + `rootfsSelector` now live in an untagged `rootfs.go`; the
Linux-only mount ops (`pivotRootfs`, `changeRoot`, `prepareMonRootfs`,
`noRootfs`) moved to `rootfs_linux.go`. Pure path/stat helpers
(`resolveAgainstBase`, `fileExists`) moved to an untagged `fsutil.go`. A darwin
`getMountInfo` stub (`block_darwin.go`) makes the selector skip the block path
and fall through to a shared-fs, and `UruncConfig` on darwin gained `ExtraBins`
so the selector compiles. `run_darwin.go` now calls
`ChooseRootfs(bundle, spec.Root.Path, spec.Annotations, cfg)`.

### Verified
Re-booted the same image: the log shows the selector attempting the block path,
hitting the darwin stub (`block-device rootfs is not supported on darwin`),
falling through to 9pfs, and the container boots identically. darwin
`go build ./...` + `go test ./pkg/unikontainers/...` and linux `go build
./pkg/...` (amd64+arm64) all pass.

## Phase 2c — share the launch prologue (commit: refactor share the launch prologue)

### Before
`run_darwin.go` still re-derived the monitor, unikernel and base
ExecArgs/UnikernelParams itself (its own `NewVMM` + `unikernels.New` + manual
struct assembly), overlapping the identical prologue inside the Linux `Exec`.

### After
The platform-neutral prologue — resolve rootfs → construct monitor + unikernel
from the annotations → build base ExecArgs/UnikernelParams from the spec+config
— is extracted to an untagged `exec_context.go` (`buildExecContext`). Both sides
use it:
- Linux `Exec`: its inline prologue is replaced by a `buildExecContext` call
  whose result is unpacked into the existing locals; every downstream Linux-only
  step (`SetupNet`, rootfs builders, mount namespaces, vAccel, hooks, launch) is
  untouched. Spec.Linux memory/seccomp handling moved into the helper behind a
  nil guard (Spec.Linux is nil on darwin).
- darwin `run_darwin.go`: drops the hand-rolled construction and calls the
  exported `PrepareExec` (which wraps `GetUnikernelConfig` → `buildExecContext`
  → `ChooseRootfs`), then adds only the darwin realization (kernel path resolved
  against the host rootfs, rootfs shared directly as fs0, QEMU child launch).
- `monitorFamily` normalizes the darwin `qemu-hvf` alias to the `qemu` family
  the unikernel builders switch on (no-op on Linux).

### Verified
darwin `go build ./...` + `go test ./pkg/unikontainers/...` + `go vet`; linux
`go build ./pkg/...` (amd64+arm64). The urunit ubuntu image still boots on Apple
Silicon under HVF through the shared prologue. The Linux `Exec` change is
mechanical (prologue lifted, locals preserved) and rests on upstream CI / a
Linux e2e run for runtime verification — it cannot be exercised from macOS.

## Remaining — the lifecycle engine, the largest/riskiest step
Still `//go:build linux` and genuinely platform-specific: the `Unikontainer`
type and its `New`/`Get`/`Create`/`Kill`/`Delete`/`Signal` methods, the IPC
(containerd-shim unix-socket protocol), `FormatNsenterInfo`/reexec, OCI hooks,
and `SetupNet` (netlink/netns). darwin's `run` remains a one-shot launcher (no
persisted OCI state, no create/start split, no shim IPC). Converging this means
introducing `Network` and lifecycle/state seams and a darwin state store, and it
changes the Linux control path in ways only a Linux runtime/e2e can validate.
Gated on the S2 decision (a macOS OCI-lifecycle consumer) in
`darwin-upstreaming-analysis.md`; to be scheduled with a Linux CI/e2e loop, not
attempted blind from macOS.
