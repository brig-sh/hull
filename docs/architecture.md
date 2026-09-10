# Architecture

This page explains how the pieces fit together. It is background, not a
procedure. For the flags themselves, read [cli.md](cli.md).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/architecture-on-dark.svg">
    <img alt="hull pulls an OCI image and prepares a rootfs in its store, then starts one of three runner processes: vz-runner on Virtualization.framework, hvi on Hypervisor.framework, or qemu-system-aarch64 using the HVF accelerator. Each boots an arm64 Linux kernel and an init wrapper that hands off to the workload. The optional network gateway serves all three over a Unix socket." src="../assets/architecture-on-light.svg" width="900">
  </picture>
</p>

## The CLI starts no virtual machine

`hull` is a Go program that prepares state and then runs another process. It
never calls a hypervisor framework itself. One runner process exists per
instance, and it is the process that owns the guest's memory and vCPUs.

This split explains most of hull's behavior:

- The runners need code-signing entitlements. `hull` does not. See
  [signing.md](signing.md).
- `hull` finds a runner next to its own executable, so the three files travel
  together.
- If a runner dies, the instance dies with it. `hull ps` reports what is left.

## What a run does, in order

1. Resolve the image reference and apply the pull policy.
2. Pull the image into the store's image cache, if it is not already there.
3. Read the OCI configuration and hull's annotations from the image.
4. Decide the backend, from `--hypervisor` or the
   `com.urunc.unikernel.hypervisor` annotation.
5. Decide the kernel. If the image carries one, use it. If it does not, use the
   generic boot assets from the store.
6. Prepare the root filesystem in the mode the backend and `--rootfs-type`
   select.
7. Build the kernel command line, including the init wrapper to run.
8. Start the runner process, and connect the console.

Steps 1 to 7 are the same work whichever backend runs. Step 8 is where the
three diverge. [images.md](images.md) covers steps 1 to 5 in detail, and
[storage.md](storage.md) covers step 6.

## The three backends are not one thing

The old picture of "three backends over one framework" is misleading, so the
diagram above keeps them apart:

- **`vz`** runs `vz-runner`, a Swift program that calls
  **Virtualization.framework**. That is Apple's high-level VM abstraction. It
  supplies the device model, the NAT network and the save and restore calls
  that checkpointing uses.
- **`hvi`** runs `hvi`, a Rust program that calls **Hypervisor.framework**
  directly. It owns guest RAM and the vCPUs, and it implements its own virtio
  device model. That is what lets brig give a workload the same device model on
  macOS and on Linux.
- **`qemu`** runs `qemu-system-aarch64` with `-accel hvf`, which reaches
  Hypervisor.framework through QEMU's accelerator. The device model is QEMU's.

Virtualization.framework and Hypervisor.framework are different interfaces at
different levels, and they need different entitlements. A capability one
backend has is not evidence another has it.
[backends.md](backends.md) is the comparison.

## Init wrappers

hull passes `init=` on the kernel command line, pointing at a small shell
script. The script prepares the guest, then hands off to `urunit` or to the
image's entrypoint. Which script runs depends on the backend and the rootfs
mode:

| Wrapper | Used for | What it does |
|---|---|---|
| `/vz-init` in the generic initrd | `vz` with a plain OCI image | mounts the read-only virtiofs export as the lower layer of a tmpfs overlay, restores the OCI metadata, then `switch_root` into the entrypoint |
| `.vz-init` | `vz` with virtiofs | overlayfs with a tmpfs upper layer, `pivot_root`, devtmpfs, devpts, `resolv.conf`, then `exec urunit` |
| `.qemu-init` | `qemu` with 9pfs | devtmpfs, devpts, a tmpfs on `/tmp`, `resolv.conf`, then `exec urunit` |
| `.block-init` | any backend with an ext4 block rootfs | devtmpfs, devpts, a tmpfs on `/tmp`, `resolv.conf`, then `exec urunit` |

The wrapper is also where guest DNS comes from on the backend-native network
paths. It copies the kernel's DHCP answer from `/proc/net/pnp` into
`/etc/resolv.conf`. [networking.md](networking.md) explains when that applies
and when the gateway serves DNS instead.

## The store is the boundary

Everything durable lives under one directory, and `--store-dir` selects it.
Two stores share nothing. The boot assets live inside the store rather than
beside it, so a `hull assets pull` in one shell cannot change what a run
against a different store boots. That was a real defect once, and the layout is
the fix.

The store is a case-sensitive APFS volume because Linux package trees contain
names that differ only by case. A single directory can hold both
`xt_CONNMARK.h` and `xt_connmark.h`. Unpacking that onto a case-insensitive
filesystem silently collapses the two, and the guest boots and then fails in a
way that is hard to trace. hull refuses a case-insensitive store instead.

[storage.md](storage.md) has the layout and the lifetimes.

## The network gateway is a separate process

Backend-native networking differs per backend, and on `hvi` it does not carry
TCP at all. The user-mode gateway exists so that one network model works
everywhere.

`hull network-gateway` runs a single process holding a gvisor netstack, a layer
2 switch, DHCP, DNS and the host port forwards. Guests reach it over a Unix
socket, which needs no entitlement and no root. It is also the one place every
packet a guest sends outward already passes, which is why the egress policy
lives there.

`hull compose` starts one gateway per project. A standalone `hull run` joins one
with `--gateway-sock`. See [networking.md](networking.md) and
[network-egress.md](network-egress.md).

## Where the code lives

| Path | What is in it |
|---|---|
| `cmd/hull` | the CLI: commands, run path, exec, compose, gateway supervision |
| `pkg/ociclient` | pulling and resolving OCI images |
| `pkg/store` | the store volume, image cache, instance state |
| `internal/bootassets` | fetching and verifying the generic kernel and initrd |
| `internal/compose` | Compose file loading and the unsupported-key warnings |
| `internal/netgw` | the gateway's netstack, DNS and egress filter |
| `internal/telemetry` | the telemetry client |
| `vz-runner` | the Swift runner for the `vz` backend |
| `hvi-vmm` | the `hvi` VMM, a git submodule pinned to a commit |

hull consumes [urunc](https://github.com/urunc-dev/urunc) as a Go dependency
for the darwin hypervisor backends and the generic container initrd. The
pinned version is a branch tip rather than a tagged release; see
[build.md](build.md#the-urunc-dependency).
