# hull — Native macOS CLI for Unikernels

`hull` is a purpose-built command-line interface for running unikernels on macOS with Apple Silicon (M1/M2/M3/M4). It provides a familiar nerdctl/podman-like UX without requiring containerd or Linux VMs.

## Features

- **OCI Image Support**: Pull and run any OCI image containing unikernel binaries
- **QEMU+HVF Acceleration**: Native Apple Silicon performance via Hypervisor Framework
- **Instance Management**: List, stop, remove running unikernel instances
- **Log Streaming**: View and follow instance output in real-time
- **No Daemon**: Lightweight, standalone binary with local state storage

## Installation

### Build from Source

```bash
make urunc_macos
# Binary placed at dist/hull_arm64

# Make it available in PATH
cp dist/hull_arm64 /usr/local/bin/hull
```

### With Homebrew (planned)

```bash
brew install nubificus/tap/hull
```

## Quick Start

### Pull a Unikernel Image

```bash
hull pull ghcr.io/nubificus/unikernels/alpine:arm64
```

### List Cached Images

```bash
hull images
```

### Run a Unikernel (Foreground)

```bash
hull run ghcr.io/nubificus/unikernels/alpine:arm64
```

### Run Detached

```bash
# Run in background and get instance ID
ID=$(hull run --detach --mem 256 --cpus 2 ghcr.io/nubificus/unikernels/nginx:arm64)

# Check status
hull ps

# View logs
hull logs $ID

# Follow logs in real-time
hull logs --follow $ID

# Stop instance
hull stop $ID

# Remove instance
hull rm $ID
```

## Command Reference

### Global Flags

- `--debug` — Enable debug logging
- `--store-dir DIR` — Directory for images and instance state (default: `~/.hull`)

### Commands

#### `pull <image-ref>`

Pull an OCI image and store it locally.

```bash
hull pull ghcr.io/nubificus/unikernels/nginx:arm64
```

**Note**: Images are always pulled with `platform=linux/arm64` for Apple Silicon compatibility.

#### `run [flags] <image-ref>`

Create and run a unikernel instance.

**Flags:**
- `--detach, -d` — Run in background (prints instance ID to stdout)
- `--name ID` — Set custom instance name (default: random 8-char hex)
- `--mem MB` — Memory size in MB (default: 512)
- `--cpus N` — Number of vCPUs (default: 1)
- `--net MODE` — Network mode: `none` (default, no network) or `shared` (vmnet NAT)

**Examples:**

```bash
# Foreground execution
hull run --mem 1024 --cpus 4 ghcr.io/nubificus/unikernels/nginx:arm64

# Detached with custom name
hull run --detach --name my-app --net shared my-image:arm64

# With all options
hull run --detach --name server --mem 2048 --cpus 8 --net shared nginx:arm64
```

#### `ps`

List all instances (running and stopped).

```bash
hull ps
```

Output columns:
- `ID` — Instance identifier
- `STATUS` — Current status (`running`, `stopped`, or `error`)
- `PID` — QEMU process ID (empty if stopped)
- `CREATED` — Creation timestamp

#### `stop [flags] <instance-id>`

Gracefully stop a running instance.

**Flags:**
- `--timeout N` — Seconds to wait before force-kill (default: 10)

**How it works:**
1. Attempts graceful shutdown via QEMU QMP (System Powerdown)
2. Waits up to `--timeout` seconds
3. Falls back to SIGKILL if timeout expires

```bash
hull stop --timeout 15 my-app
```

#### `rm [flags] <instance-id>`

Remove a stopped instance (deletes all state).

**Flags:**
- `--force, -f` — Force stop running instance before removal

```bash
hull rm my-app           # Error if running
hull rm --force my-app   # Stops and removes
```

#### `logs [flags] <instance-id>`

View instance log output (QEMU stdout/stderr).

**Flags:**
- `--follow, -f` — Stream new log lines (like `tail -f`)
- `--tail N` — Show last N lines (default: -1 for all)

```bash
hull logs my-app           # Print all logs
hull logs --tail 50 my-app # Last 50 lines
hull logs --follow my-app  # Stream logs
```

#### `inspect <instance-id>`

Print full instance state as JSON (includes QEMU command line, PID, status, etc.).

```bash
hull inspect my-app | jq .
```

#### `images`

List all cached images.

```bash
hull images
```

Output columns:
- `REPOSITORY` — Image repository/host
- `TAG` — Image tag
- `DIGEST` — Short image digest (first 19 chars)
- `SIZE` — Total image size
- `CREATED` — Pull timestamp (e.g., "2h ago")

## State Storage

Instance and image state is stored in `~/.hull/`:

```
~/.hull/
├── images/
│   └── <digest>/
│       ├── rootfs/           ← extracted OCI layers
│       ├── image.json        ← metadata
│       └── oci-config.json   ← OCI ConfigFile
└── instances/
    └── <instance-id>/
        ├── bundle/
        │   ├── config.json   ← OCI runtime spec
        │   └── rootfs -> ../../../images/<digest>/rootfs
        ├── state.json        ← instance state (PID, status, etc.)
        ├── log               ← QEMU stdout/stderr
        └── qmp.sock          ← QMP control socket
```

## Networking

### Default: `--net none`

Unikernel has no network access. Useful for testing, single-binary services.

### `--net shared`

Unikernel gets network via QEMU's vmnet-shared backend (NAT mode):
- Automatic DHCP from macOS
- Outbound connectivity works immediately
- Inbound requires port forwarding (not yet implemented in hull)

**Requirements:**
- Requires `com.apple.vm.networking` entitlement OR root privilege
- For development: run with `sudo` or disable System Integrity Protection

Example:

```bash
sudo hull run --detach --net shared --name web nginx:arm64
```

## Performance Notes

- **Boot time**: ~1–2 seconds for typical unikernel (QEMU startup + HVF overhead)
- **Memory**: Overhead varies; recommend 512 MB minimum
- **Network**: vmnet-shared has ~1 ms latency, suitable for development/testing

For production workloads, consider Apple's Virtualization.framework (Phase 2 future work).

## Building Unikernel Images

Unikernel images for hull must be:

1. **Platform**: `linux/arm64` (native Apple Silicon)
   - Use Unikraft, MirageOS, OSv, or compatible framework
   - aarch64/arm64 target required

2. **Packaging**: OCI Image Format
   - Include unikernel binary + any rootfs contents
   - Store unikernel binary at `/unikernel` or set via OCI annotations

3. **Annotations** (optional, in image config):
   - `com.urunc.unikernel.unikernelType` — Type (e.g., "linux", "rumprun", "mirage")
   - `com.urunc.unikernel.hypervisor` — "qemu-hvf" (default)
   - `com.urunc.unikernel.cmdline` — Kernel command line

Example Unikraft image build:

```bash
kraft build --plat qemu/arm64 -o container ghcr.io/myrepo/my-app:arm64
```

## Troubleshooting

### Instance stuck in "running" status after crash

```bash
# Check if QEMU process still exists
hull ps

# If PID shows but process is dead, force remove
hull rm --force <instance-id>
```

### Network connectivity not working

Check entitlements:

```bash
# Is the binary notarized/signed with vmnet entitlements?
codesign -d --entitlements :- /usr/local/bin/hull

# For development, try with sudo (if SIP is enabled)
sudo hull run --net shared my-app:arm64
```

### Image pull fails with "platform not found"

The image doesn't have a `linux/arm64` variant. Check available platforms:

```bash
# Using skopeo or crane
crane config ghcr.io/myrepo/my-app:latest | jq .
```

## Contributing

Contributions welcome! Key areas for expansion:

- [ ] Port forwarding for `--net shared` mode
- [ ] Virtualization.framework backend (Phase 2)
- [ ] GPU compute via vAccel + Metal
- [ ] Checkpoint/restore support
- [ ] Integration tests with real unikernel images

## License

Apache License 2.0 — See [LICENSE](../../LICENSE)

## References

- [CLAUDE.md](../../CLAUDE.md) — Full macOS porting plan
- [urunc](../urunc/) — Main urunc CLI (Linux/containerd integration)
- [Unikraft Docs](https://docs.unikraft.org/) — Building unikernels
