# OCI Compatibility Plan for hull

## Research Findings

### OCI Runtime Spec

The spec defines a platform-agnostic lifecycle state machine: **create -> start -> kill -> delete**,
with a **state** query. The config.json has:

- **Platform-agnostic fields**: `ociVersion`, `root`, `process` (args, cwd, env, user), `mounts`
  (only `destination` required), `hostname`, `annotations`, `hooks`
- **Platform-specific sections**: `linux` (cgroups, namespaces, seccomp), `windows` (HCS),
  `solaris`, `freebsd`, `zos`
- **`vm` section** (config-vm.md): hypervisor path/params, kernel image/cmdline/initrd,
  root image format (raw, qcow2, etc.), vCPU count, memory size
- **No `darwin` section exists** — but the `vm` section already covers hypervisor-based execution

Key insight: the `vm` section was designed for exactly this use case — running workloads in
hypervisor-managed VMs rather than Linux namespaces. urunc on Linux already uses it.

Resource constraints are entirely platform-specific (Linux = cgroups, Windows = HCS).
macOS has no equivalent, but VM-level limits (vCPU count, memory) can be set at creation time.

### OCI Image Spec

- **Image Index** supports multiplatform via `os` + `architecture` + `variant`
- **`darwin` is a valid `os` value** (it's a Go `GOOS` value; containerd normalizes `macos` -> `darwin`)
- Each platform in an image index has **completely independent layers**
- Custom annotations (`com.urunc.*`) are fully supported at index, manifest, and descriptor levels
- On macOS, the default platform resolves to `darwin/arm64` — pulling `linux/arm64` requires
  explicit platform override (or the client must be configured to prefer it)

### Apple's Container Framework

Apple's `container` tool (WWDC 2025, v1.0.0 June 2026) takes a specific approach:

| Aspect | Apple's approach | hull current |
|--------|-----------------|-------------------|
| Image platform | `linux/arm64` (always Linux guests) | `linux/arm64` via Bunny |
| Rootfs | **ext4 block device** (layers flattened) | virtiofs/9pfs + optional ext4 block |
| Guest init | **vminitd** — gRPC over vsock | Shell init wrappers (`.vz-init`, `.qemu-init`) |
| Host-guest control | vsock gRPC channel | Serial console (stdin/stdout) |
| Runtime spec | **Not OCI runtime** — standalone daemon | Not OCI runtime — standalone CLI |
| Overlay | Guest-internal overlayfs (lower=rootfs, upper=writable) | Same pattern for Vz backend |
| containerd | Experimental CShim module | containerd-shim-urunc-v2 (Linux only) |

**Critical finding**: Apple does NOT use `darwin` images. They pull standard `linux/arm64` images
and run them in VMs. This validates the current hull approach.

### urunc Linux vs macOS Gap

| Capability | Linux urunc | macOS hull |
|-----------|-------------|-------------------|
| OCI lifecycle | create/start/kill/delete/state | `run` only (combined) |
| containerd shim | Yes (v2) | No |
| State persistence | `/run/urunc/` | `~/.hull/instances/` (ad-hoc) |
| Bundle input | Accepts pre-made OCI bundle | Pulls + generates bundle internally |
| Signal handling | Full signal forwarding | SIGTERM + force kill |
| Hypervisors | QEMU, Firecracker, Cloud Hypervisor, solo5 | QEMU+HVF, Vz |

### FreeBSD and Solaris OCI Spec Sections

Both map their native OS isolation primitive directly to the OCI spec:

**FreeBSD (`config-freebsd.md`)** — merged Oct 2025 (most recent addition):
- Maps `jail(2)` directly: hostname, IP restrictions, vnet, SysV IPC, allow flags
- Device list for devfs exposure beyond minimal ruleset
- ~15 fields total — compact, focused on the jail API

**Solaris (`config-solaris.md`)** — added Apr 2016 (early spec era):
- Maps Zones (`zonecfg`): capped CPU (fractional ncpus), capped memory, SMF milestones,
  privilege sets, automatic network (VNIC) creation
- ~10 fields — resource caps + networking + privileges

**VM section (`config-vm.md`)** — core merged Mar 2018, `hwConfig` merged Oct 2025:
- `hypervisor`: path + parameters (for external VMMs like QEMU, Firecracker)
- `kernel`: path + parameters + initrd (REQUIRED)
- `image`: path + format (raw/qcow2/vdi/vmdk/vhd)
- `hwConfig`: vCPUs, memory, device passthrough (Xen-oriented)

**Pattern**: Each platform section is minimal — it maps the native isolation API, nothing more.
FreeBSD took ~6 months from PR to merge. The `vm` hwConfig extension took ~2.3 years.

### macOS Containment Primitives

**XNU has no namespace mechanisms.** No mount, PID, network, user, or UTS namespaces.
No cgroups. FreeBSD jails were never ported to Darwin. `chroot` exists but is broken
by default (AMFI kills hardened-runtime binaries in chroot; requires root + SIP disabled).

What macOS DOES have:

| Primitive | What it does | Limitation |
|-----------|-------------|------------|
| **Sandbox (Seatbelt)** | ~300 kernel hooks: restrict FS, network, Mach IPC, exec, sysctl. Irrevocable, inherited by children. | Access control only — no isolation (no private mount/PID/net). Cannot be nested. APIs deprecated but functional. |
| **Jetsam / memorystatus** | Fatal memory limits per process via `memorystatus_control()` syscall or `taskpolicy -m <MiB>` | Per-process only, not per-group. Undocumented syscall. |
| **QoS classes** | CPU scheduling tiers. `taskpolicy -b` = background = E-cores only (~12-23% chip perf) | Coarse-grained. No precise CPU percentage caps. |
| **I/O policy** | `setiopolicy_np()` with 3 tiers (normal/throttle/passive) | No bandwidth numbers, just priority classes. |
| **POSIX rlimits** | `RLIMIT_NPROC`, `RLIMIT_NOFILE`, `RLIMIT_CPU` (cumulative CPU seconds) | `RLIMIT_RSS` unenforced. `RLIMIT_AS` doesn't exist. |
| **Virtualization.framework** | Hardware-enforced memory isolation via ARM64 EL2 stage-2 page tables. No public VM escape CVEs. | `cpuCount` and `memorySize` immutable post-creation. No network/disk I/O throttling. |
| **APFS clonefile(2)** | Instant copy-on-write filesystem copies | Not isolation — just efficient copying |

**Apple's container framework approach**: The VM IS the container boundary. They use ONLY
Virtualization.framework for isolation — no sandbox profiles, no undocumented kernel APIs.
Precise resource limits are delegated to **Linux cgroups inside the guest VM** (cgroups v2).
Network policy via `VZFileHandleNetworkDeviceAttachment` (custom userspace network stack).

### Should We Propose `config-darwin.md`?

**The case FOR a darwin spec section:**

1. FreeBSD was just added (Oct 2025) — precedent for new platforms is fresh
2. macOS has unique primitives worth standardizing: Sandbox profiles, jetsam limits,
   QoS classes, Vz-specific config (virtiofs shares, Rosetta, entitlements)
3. The `vm` section doesn't cover framework-managed VMs (Vz has no hypervisor binary
   path — the framework IS the hypervisor) or virtiofs share configuration
4. A spec section would enable interoperability between urunc, Apple's container tool,
   and any future macOS container runtimes

**The case AGAINST:**

1. macOS has almost nothing comparable to jails/zones/namespaces/cgroups — the section
   would be thin and mostly describe VM parameters already covered by `vm`
2. Apple's own container framework doesn't implement OCI runtime spec at all
3. The OCI spec review process is slow (hwConfig took 2.3 years to merge)
4. Practical value is low until there's a macOS containerd ecosystem

**Recommended approach**: Don't propose `config-darwin.md` yet. Instead:

1. Use the existing `vm` section for hypervisor/kernel/image config
2. Use **annotations** for darwin-specific config that `vm` doesn't cover:
   - `com.urunc.darwin.sandbox-profile` — Seatbelt profile for host VMM process
   - `com.urunc.darwin.jetsam-limit-mb` — memory limit via memorystatus
   - `com.urunc.darwin.qos-class` — CPU QoS tier (background/utility/default)
   - `com.urunc.darwin.rosetta` — enable Rosetta 2 for x86_64 guests
   - `com.urunc.darwin.virtiofs-shares` — additional virtiofs mount points
3. Apply Seatbelt + jetsam + QoS to the host-side VMM process for defense-in-depth
4. Use guest-side Linux cgroups for precise per-container resource limits (Apple's model)
5. If the annotation set stabilizes and other macOS runtimes emerge, THEN propose
   `config-darwin.md` to formalize it

---

## Proposed Plan

### Phase 1: OCI Runtime Lifecycle on macOS

Split the monolithic `run` command into proper OCI lifecycle operations.

**1a. Implement `create`**
- Accept a bundle directory (with config.json + rootfs/) as input
- Parse config.json using the standard OCI runtime-spec Go types (`specs.Spec`)
- Set up the instance: clone rootfs, generate init wrappers, prepare VM config
- Write state file (status: `created`, bundle path, annotations)
- Do NOT start the VM yet
- Return the container ID

**1b. Implement `start`**
- Look up container by ID, verify status is `created`
- Launch the VMM process (QEMU or vz-runner)
- Update state to `running` with host-side PID of the VMM process
- In foreground mode, wire up terminal; in detached mode, log to file

**1c. Implement `state`**
- Return JSON per OCI spec: `ociVersion`, `id`, `status`, `pid`, `bundle`, `annotations`
- The `pid` is the host-side VMM process PID (same as Kata Containers' approach)

**1d. Implement `kill`**
- Send signal to VMM process (default SIGTERM)
- Support arbitrary signals via `--signal` flag
- QMP graceful shutdown for QEMU before signal escalation

**1e. Implement `delete`**
- Verify container is `stopped`
- Clean up instance directory, rootfs clone, QMP socket
- Remove state file

**1f. Keep `run` as sugar**
- `run` = `create` + `start` + wait (same as `runc run`)

**Impact**: This makes hull a proper OCI runtime that external tools (containerd, nerdctl)
could invoke.

### Phase 2: config.json Compatibility

**2a. Parse standard OCI spec**
- Replace the ad-hoc JSON parsing in `run.go` with `specs.Spec` from `opencontainers/runtime-spec`
- Read `process.args`, `process.env`, `process.cwd`, `process.user` from spec
- Read `root.path` for rootfs location
- Read `mounts` for shared directories
- Read `annotations` for urunc-specific config (`com.urunc.unikernel.*`)

**2b. Support the `vm` section**
- Parse `vm.hypervisor` (path, parameters) -> VMM selection
- Parse `vm.kernel` (path, parameters, initrd) -> kernel/cmdline config
- Parse `vm.image` (path, format) -> rootfs disk image
- This replaces the current annotation-based hypervisor selection with a spec-standard mechanism
- Keep annotation-based config as fallback for backward compatibility

**2c. Map `mounts` to VM shares**
- `mounts[].destination` -> guest mount point
- `mounts[].source` -> host directory path
- `mounts[].type` -> `virtiofs` or `9p` (maps to VM share mechanism)
- `mounts[].options` -> mount options passed to guest

**2d. Map resources to VM limits**
- No `darwin` resource section exists, but VM creation already takes vCPU and memory params
- If a hypothetical `vm.resources` or annotation-based approach is used, map to VMM config
- CPU count -> `VZVirtualMachineConfiguration.cpuCount` / QEMU `-smp`
- Memory -> `VZVirtualMachineConfiguration.memorySize` / QEMU `-m`

### Phase 3: Multiplatform Image Strategy

**Two viable approaches**, not mutually exclusive:

#### Option A: Single `linux/arm64` manifest (Apple's approach)

- Both Linux urunc and macOS hull pull the **same** `linux/arm64` image
- The rootfs contains a Linux filesystem (the guest IS Linux in both cases)
- Platform-specific behavior is determined by the **runtime**, not the image
- urunc annotations in the image carry hypervisor hints, but the runtime decides
- **Pro**: Simplest. One image works everywhere. Matches Apple's proven model.
- **Con**: No way to carry macOS-specific optimizations (e.g., Vz-tuned kernel config)
  in the image itself

#### Option B: Image Index with platform-specific manifests

```
image index
  ├── linux/arm64   -> standard urunc image (kernel + rootfs + urunc.json)
  └── darwin/arm64  -> macOS-optimized variant (different kernel config, Vz annotations)
```

- `darwin/arm64` manifest could carry:
  - A kernel built with virtiofs built-in (not as module) for Vz
  - macOS-specific annotations: preferred hypervisor, virtiofs vs block mode
  - Potentially different init wrappers baked in
- On macOS, the OCI client auto-resolves `darwin/arm64`
- On Linux, it auto-resolves `linux/arm64`
- **Pro**: Platform-optimized images without runtime guessing
- **Con**: Two manifests to maintain. The rootfs layers are often identical (both are Linux).

#### Recommendation: Start with Option A, evolve to Option B

- Option A is already how hull works today (pulls `linux/arm64` images)
- The runtime handles platform differences (Vz vs QEMU, virtiofs vs 9pfs)
- Option B can be added later when there's a real need for platform-specific image content
- Bunny could eventually produce both manifests in one build

### Phase 4: Bundle Generation from OCI Images

Currently, hull has its own OCI client that pulls images and generates bundles internally.
To be OCI-runtime-compatible, it should also accept pre-made bundles (like `runc` does).

**4a. Accept external bundles**
- `hull create <id> --bundle <path>` — standard OCI runtime invocation
- Parse config.json from the bundle directory
- The bundle's rootfs/ already contains the extracted image layers

**4b. Keep the pull+run convenience**
- `hull run <image-ref>` — the current CLI flow, pulls and generates bundle
- This is sugar on top of the OCI runtime operations
- Internally: pull -> generate bundle -> create -> start

**4c. containerd shim (future)**
- The existing `containerd-shim-urunc-v2` has darwin stubs
- Once create/start/kill/delete work, the shim can delegate to hull
- This enables `nerdctl run --runtime urunc` on macOS

### Phase 5: Guest Init Improvements (inspired by Apple)

Apple's vminitd approach (gRPC over vsock) is more robust than shell init wrappers.

**5a. Short term: improve init wrappers**
- Make `.vz-init` / `.qemu-init` / `.block-init` more robust
- Add OCI process.env propagation, user switching, working directory
- Add signal forwarding from host to guest init

**5b. Medium term: vsock control channel — SHIPPED**
- Implemented as the urunit-agent transport: agentproto frames over vsock
  port 1024 on Vz and virtio-serial `io.urunc.agent.0` on QEMU
- `hull exec` runs commands in a running instance (tty, cwd, user,
  env), when the image ships `/urunit-agent`
- Remaining from the original sketch: mount commands, signal forwarding
  beyond SIGINT/SIGTERM, and replacing the serial console as the primary
  control mechanism (boot config still travels via kernel cmdline and
  urunit.conf; stop still via QMP/SIGTERM; the agent is a separate channel)

**5c. Long term: vminitd-style agent**
- Minimal Go or C init that runs as PID 1 in the guest
- Receives container spec over vsock, sets up mounts/env/user, execs the workload
- Reports status back to host
- This matches both Apple's vminitd and Kata Containers' kata-agent

### Phase 6: Host-Side Containment (Defense-in-Depth)

The VM boundary is the primary isolation, but we can add host-side hardening using
macOS-native primitives. This matches the defense-in-depth model — even if the VM is
compromised, the host-side constraints limit blast radius.

**6a. Sandbox (Seatbelt) profile for VMM processes**
- Write a `.sb` sandbox profile that restricts the QEMU/vz-runner process:
  - Filesystem: read-only except instance directory, image cache, QMP socket
  - Network: allow vmnet/Vz networking only
  - Mach IPC: allow only Virtualization.framework service lookups
  - No exec of other binaries, no sysctl writes
- Apply via `sandbox_init_with_parameters()` before exec, or `sandbox-exec -f` wrapper
- The sandbox is irrevocable and inherited by child processes

**6b. Jetsam memory limits**
- Set a memory limit on the VMM host process via `memorystatus_control()` or
  `posix_spawnattr_setjetsam_ext()` at spawn time
- If the VMM process exceeds the limit, jetsam kills it (macOS OOM killer)
- This prevents a runaway VM from consuming all host memory
- Map from `vm.hwConfig.memory` or `--mem` flag (host limit = guest mem + overhead)

**6c. QoS class / CPU policy**
- Set the VMM process to a specific QoS class via `taskpolicy` or `setpriority()`
- Background class (`PRIO_DARWIN_BG`) restricts to E-cores on Apple Silicon
- Useful for low-priority workloads — prevents VM from starving host processes
- Could be exposed as `--cpu-priority background|utility|default`

**6d. I/O throttling**
- Apply I/O throttle tier via `setiopolicy_np()` on the VMM process
- Three tiers: normal, throttle (reduced priority), passive (lowest)
- Limits disk I/O impact of VM disk operations on host

**6e. Guest-side cgroups (for precise limits)**
- For precise CPU/memory/IO limits, use Linux cgroups v2 inside the guest
- The guest init (`.vz-init` or future vminitd) creates a cgroup for the workload
- Host passes resource limits to guest via kernel cmdline, vsock, or config file
- This is exactly what Apple's container framework does — and it works
- Requires guest kernel with CONFIG_CGROUPS (the Nubificus kernel may need this)

---

## Priority Order

| Phase | Effort | Impact | Dependency |
|-------|--------|--------|------------|
| 1 (OCI lifecycle) | Medium | High — enables containerd integration | None |
| 2 (config.json) | Medium | High — spec compliance | Phase 1 |
| 3 (multiplatform) | Low | Medium — Option A is already working | None |
| 4 (bundle input) | Low | Medium — external tool compatibility | Phase 1 |
| 5a (init wrappers) | Low | Medium — robustness | None |
| 5b (vsock control) | — shipped (urunit-agent transport) | High — enables exec, proper I/O | Phase 1 |
| 5c (vminitd agent) | High | High — full OCI process model | Phase 5b (shipped) |
| 6a (sandbox profile) | Low | Medium — host-side hardening | None |
| 6b (jetsam limits) | Low | Medium — memory containment | None |
| 6c (QoS/CPU policy) | Low | Low-Med — CPU containment | None |
| 6d (I/O throttle) | Low | Low — disk I/O containment | None |
| 6e (guest cgroups) | Medium | High — precise resource limits | Phase 5b (shipped) |

**Recommended start**: Phase 1 (lifecycle) + Phase 2 (config.json) in parallel — they're
the foundation. Phase 6a-6d are quick wins that can be done independently at any time.
Phase 3 Option A is already working. Phase 5a improves robustness with minimal effort.
