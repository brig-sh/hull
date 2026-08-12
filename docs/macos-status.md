# urunc macOS Port — Status & Benchmarks

## Overview

macOS port of urunc, an OCI-compliant unikernel container runtime. Two VMM backends
are implemented, both targeting Apple Silicon (darwin/arm64):

| Backend | VMM | Acceleration | Rootfs Modes | Networking |
|---------|-----|-------------|--------------|------------|
| **Vz** | Virtualization.framework (`vz-runner`) | Native | virtiofs+overlayfs, ext4 block | VZNATNetworkDeviceAttachment |
| **QEMU+HVF** | `qemu-system-aarch64` | HVF | 9pfs, ext4 block | vmnet-shared |

## Binaries

| Binary | Source | Language | Notes |
|--------|--------|----------|-------|
| `urunc-macos` | `cmd/urunc-macos/` | Go | CLI: pull, run, ps, stop, rm, logs, inspect, images |
| `vz-runner` | `cmd/vz-runner/` | Swift | Virtualization.framework helper, requires code-signing |

Both target `darwin/arm64`. After building, both must be signed:

```bash
codesign --force --sign - --entitlements Entitlements.plist urunc-macos
codesign --force --sign - --entitlements Entitlements.plist vz-runner
```

Required entitlements: `com.apple.security.virtualization`, `com.apple.vm.networking`.
Ad-hoc signing works with `amfi_get_out_of_my_way=1` boot arg (SIP disabled).

## Container Images (Bunny)

Images are built with the [Bunny](https://github.com/nubificus/bunny) BuildKit frontend:

```dockerfile
#syntax=harbor.nbfc.io/nubificus/bunny:latest

FROM --platform=linux/arm64 ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    net-tools iproute2 iputils-ping curl \
    fdisk mount util-linux procps \
    && rm -rf /var/lib/apt/lists/*
```

Bunny automatically injects:
- **Kernel** at `/.boot/kernel` (from `harbor.nbfc.io/nubificus/bunny/linux-kernel-qemu`)
- **urunit** init process at `/urunit`
- **urunc.json** with base64-encoded annotations

Pre-built image: `harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64`

### Building & Pushing

```bash
DOCKER_BUILDKIT=1 docker build --platform linux/arm64 \
    --provenance=false --sbom=false \
    -t harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64 \
    -f Dockerfile.ubuntu .

# Push (crane handles Harbor better than docker push for large images)
docker save harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64 -o /tmp/image.tar
crane push /tmp/image.tar harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64
```

## Running

```bash
# Vz backend (recommended — faster networking, native Apple framework)
./urunc-macos run --hypervisor vz --mem 2048 --cpus 2 --net shared \
    harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64

# QEMU+HVF backend
./urunc-macos run --hypervisor qemu --mem 2048 --cpus 2 --net shared \
    harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64

# Block device rootfs (ext4 disk image instead of virtiofs/9pfs)
./urunc-macos run --hypervisor vz --rootfs-type block --mem 2048 --cpus 2 --net shared \
    harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64

# Detached mode
./urunc-macos run --detach --hypervisor vz --mem 2048 --cpus 2 --net shared \
    harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64

# Management
./urunc-macos ps
./urunc-macos logs <instance-id>
./urunc-macos stop <instance-id>
./urunc-macos rm <instance-id>
```

## Architecture

### Boot Flow (Vz — virtiofs)

1. `urunc-macos` pulls OCI image, extracts rootfs to `~/.urunc-macos/images/<digest>/rootfs/`
2. APFS clonefile (`cp -c -a`) creates per-instance copy (copy-on-write, nearly instant)
3. Generates `.vz-init` wrapper in rootfs (overlay setup, devtmpfs, devpts, DNS)
4. Generates `urunit.conf` (env vars, UID/GID, working directory)
5. Launches `vz-runner --kernel <path> --cmdline "..." --share <rootfs> rootfs`
6. Guest kernel boots, mounts virtiofs as root, `.vz-init` sets up overlayfs + pivot_root
7. `urunit` runs the container entrypoint

### Boot Flow (QEMU — 9pfs)

1. Same image pull and clone as Vz
2. Generates `.qemu-init` wrapper (devtmpfs, devpts, DNS — no overlay needed)
3. Launches `qemu-system-aarch64 -accel hvf -M virt -cpu host ...`
4. Rootfs shared via 9pfs (`-fsdev local,security_model=none`)
5. Guest kernel boots with `root=rootfs rootfstype=9p`, `.qemu-init` runs, then urunit

### Boot Flow (ext4 Block Device)

1. Same image pull
2. Creates ext4 disk image from rootfs dir via `mke2fs -t ext4 -d <dir>`
3. Generates `.block-init` wrapper
4. Disk attached as virtio-blk (`/dev/vda`)
5. Guest boots with `root=/dev/vda rw`

### Init Wrappers

| Wrapper | Backend | Key Steps |
|---------|---------|-----------|
| `.vz-init` | Vz virtiofs | tmpfs overlay → pivot_root → devtmpfs → devpts → /tmp → resolv.conf → exec urunit |
| `.qemu-init` | QEMU 9pfs | devtmpfs → devpts → /tmp → resolv.conf → exec urunit |
| `.block-init` | ext4 block | devtmpfs → devpts → /tmp → resolv.conf → exec urunit |

DNS is populated from `/proc/net/pnp` (kernel DHCP response).

### Kernel Requirements

- **VZLinuxBootLoader requires ARM64 Image format** (uncompressed, `ARMd` magic at offset 0x38)
- PE32+ vmlinuz (EFI stub) does NOT work with Vz — fails with `VZErrorDomain Code=1`
- The Bunny kernel (`harbor.nbfc.io/nubificus/bunny/linux-kernel-qemu`) works with both backends
- Virtiofs is built-in to the Nubificus kernel (not a module)

### Networking

| Backend | Method | Subnet | Gateway |
|---------|--------|--------|---------|
| Vz | `VZNATNetworkDeviceAttachment` | 192.168.64.x | 192.168.64.1 |
| QEMU | vmnet-shared | 192.168.65.x | 192.168.65.1 |

Kernel `ip=dhcp` autoconfigures the interface. QEMU's vmnet-shared requires the binary
to be signed with `com.apple.vm.networking`.

## Benchmarks (2026-06-11)

### Test Configuration

- **Host**: macOS 26.3.1 (Tahoe), Apple Silicon (Mac14,13)
- **Guest**: Ubuntu 24.04, Kernel 6.18.0urunc (ARM64)
- **Resources**: 2 vCPUs, 2048 MB RAM
- **Image**: bunny-built Ubuntu with fio, iperf3
- **Tools**: fio 3.x (storage), iperf3 (network), ping (latency)

### Storage — tmpfs (CPU/memory bandwidth)

| Test | Vz | QEMU+HVF | Winner |
|------|-----|----------|--------|
| Sequential Write 1M | 21,376 MB/s | 21,374 MB/s | Tie |
| Sequential Read 1M | 28 MB/s | 28 MB/s | Tie |
| Random Read 4K | 1,728 MB/s | 1,751 MB/s | Tie |
| Random Write 4K | 5,929 MB/s | 6,103 MB/s | Tie |

Both use HVF for CPU virtualization; tmpfs is RAM-backed, so storage perf is identical.

### Storage — ext4 on virtio-blk (file-backed disk on host APFS)

| Test | Vz + ext4 | QEMU+HVF + ext4 | Ratio |
|------|-----------|------------------|-------|
| Sequential Write 1M | 15,419 MB/s | 10,631 MB/s | **Vz 1.45x** |
| Sequential Read 1M | 0.9 MB/s* | 7.3 MB/s* | *parsing bug* |
| Random Read 4K | 9.3 MB/s | 23.2 MB/s | **QEMU 2.5x** |
| Random Write 4K | 4,394 MB/s | 3,118 MB/s | **Vz 1.41x** |

\* Sequential Read anomaly is a fio minimal output parsing issue, not real throughput.
High write speeds are from page cache write-back (both backends).

### Network

| Test | Vz | QEMU+HVF | Ratio |
|------|-----|----------|-------|
| Ping avg | 0.54 ms | 0.36 ms | **QEMU 1.5x** |
| TCP TX (guest→host) | 25,117 Mbps | 3,107 Mbps | **Vz 8.1x** |
| TCP RX (host→guest) | 34,825 Mbps | 9,255 Mbps | **Vz 3.8x** |
| UDP 1Gbps target | 1,000 Mbps 0% loss | 1,000 Mbps 0% loss | Tie |

### Boot Time

| Metric | Vz | QEMU+HVF |
|--------|-----|----------|
| Kernel to init | ~60 ms | ~73 ms |
| DHCP completion | ~70 ms | ~164 ms |

### Summary

1. **Storage (tmpfs)**: Identical — both use HVF, tmpfs is RAM-backed
2. **Storage (ext4 block)**: Vz ~45% faster writes, QEMU faster random reads
3. **Network throughput**: Vz dramatically faster — **8x TX, 3.8x RX** (native kernel-level vs userspace QEMU)
4. **Network latency**: QEMU slightly lower ping (simpler NAT path)
5. **Boot time**: Comparable, Vz marginally faster
6. **Verdict**: Vz is the clear winner for network-intensive and write-intensive workloads

## Known Issues & Fixes

### QEMU hangs in foreground mode (fixed 2026-06-18)

**Symptom**: QEMU boots when run directly from terminal, but hangs with no output when
spawned by `urunc-macos`.

**Root cause**: `SysProcAttr{Setpgid: true}` puts QEMU in a background process group.
QEMU's `-serial stdio` calls `tcsetattr()` to set raw mode, which triggers `SIGTTOU`
for background process groups, silently blocking QEMU.

**Fix**: Use `SysProcAttr{Foreground: true, Ctty: int(os.Stdin.Fd())}` in foreground mode
to make QEMU the foreground process group. Keep `Setpgid: true` only for detached mode.

**Note**: This issue didn't reproduce in environments without a controlling terminal (e.g.,
CI, Claude Code's Bash tool), since POSIX process group restrictions don't apply there.

### Stale image cache

**Symptom**: After pushing a new version of an image, `urunc-macos run` uses the old cached
rootfs. Bunny-injected layers (`/.boot/kernel`, `/urunit`, `/urunc.json`) may be missing
if the cache was populated by an earlier, incomplete pull.

**Fix**: Clear and re-pull:
```bash
rm -rf ~/.urunc-macos/images/sha256:<digest>
./urunc-macos pull <image-ref>
```

### macOS virtiofs mode-000 write bug

**Symptom**: `dpkg` and similar tools fail when creating temporary files with mode 000.

**Root cause**: macOS doesn't allow even the file owner to write to mode-000 files.
`vz-runner` runs as UID 501, so the virtiofs server can't write to those files on the host.

**Fix**: `.vz-init` sets up an overlayfs with a tmpfs upper layer on top of virtiofs.
All writes go to the tmpfs upper layer, bypassing the macOS virtiofs limitation.

### Shell: "can't access tty; job control turned off"

This is expected — the guest shell runs on a serial console, not a real TTY.

## System Requirements

- macOS 26+ (Tahoe) on Apple Silicon
- SIP disabled
- Boot arg: `amfi_get_out_of_my_way=1`
- QEMU: `brew install qemu` (must be re-signed with entitlements for vmnet)
- ext4 block mode: `brew install e2fsprogs`
- Docker Desktop (for building images with Bunny)
