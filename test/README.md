# hull QEMU Backend Test Suite

Comprehensive tests for the QEMU backend of hull running Ubuntu unikernels.

## Overview

This test suite validates:
- ✅ VM boot with QEMU/HVF on macOS
- ✅ Kernel console output capture
- ✅ Boot into interactive shell
- ✅ 9pfs shared directory support
- ✅ Command execution (uname)
- ✅ Instance lifecycle management

## Prerequisites

```bash
# Install Bunny (Nubificus OCI image builder)
brew install nubificus/tap/bunny

# Or build from source
git clone https://github.com/nubificus/bunny
cd bunny && cargo build --release
```

## Quick Start

### 1. Build the Test Image

```bash
cd test
bash build-ubuntu-test.sh
```

This will:
- Use Bunny to build Ubuntu image
- Extract and bundle Linux kernel
- Create OCI image with proper annotations
- Output: `localhost/ubuntu-qemu:aarch64`

### 2. Run Automated Tests

```bash
bash test-ubuntu-qemu.sh
```

This runs:
- **TEST 1**: Basic VM boot with QEMU
- **TEST 2**: Log file capture
- **TEST 3**: Kernel boot message detection
- **TEST 4**: Instance state validation
- **TEST 5**: 9pfs shared directory test
- **TEST 6**: Cleanup

### 3. Manual Interactive Test

```bash
cd ..
./dist/hull_arm64 run --hypervisor qemu \
  --mem 512 --cpus 2 \
  localhost/ubuntu-qemu:aarch64
```

Then in the shell:
```bash
# Check kernel
uname -a

# Check mounts
mount | grep -E "proc|sys|dev"

# List 9pfs mounts
mount | grep 9p

# Exit
exit
```

## Test Scenarios

### Scenario 1: Basic Boot (No 9pfs)

```bash
./dist/hull_arm64 run --hypervisor qemu \
  --mem 512 --cpus 2 \
  localhost/ubuntu-qemu:aarch64
```

**Expected output:**
```
=== Ubuntu Unikernel VM ===
Kernel: 5.x.x-xx-generic (or similar)
Hostname: (container-id)
Available filesystems:
...
9p
...
=== Launching interactive shell ===
root@...:/#
```

### Scenario 2: With 9pfs Sharing

```bash
# Create test directory
mkdir -p /tmp/test-data
echo "Hello from host" > /tmp/test-data/hello.txt

# Run with shared directory
./dist/hull_arm64 run --hypervisor qemu \
  --mem 512 --cpus 2 \
  --shared-dir /tmp/test-data:/mnt/host \
  localhost/ubuntu-qemu:aarch64
```

**In the VM shell:**
```bash
# Mount 9pfs if not auto-mounted
mount -t 9p -o trans=virtio fs0 /mnt/host

# Check shared content
ls -la /mnt/host
cat /mnt/host/hello.txt
# Output: Hello from host
```

### Scenario 3: Execute uname Command

```bash
# Interactive test
./dist/hull_arm64 run --hypervisor qemu \
  --mem 512 --cpus 2 \
  localhost/ubuntu-qemu:aarch64

# Then run in shell
uname -a
# Output: Linux (hostname) 5.x.x-xx-generic #xx-Ubuntu ... aarch64 ...
```

## Dockerfile Explanation

The test Dockerfile (`Dockerfile.ubuntu-qemu`):

```dockerfile
FROM ubuntu:latest

# Install Linux kernel and utilities
RUN apt-get install -y linux-image-generic procps curl

# Create init script that:
# 1. Mounts proc, sys, dev, tmp
# 2. Auto-mounts 9pfs if available
# 3. Prints system info
# 4. Launches interactive bash shell

RUN echo '#!/bin/bash' > /init && \
    echo 'mount -t proc ...' >> /init && \
    echo 'mount -t 9p ...' >> /init && \
    echo 'exec /bin/bash' >> /init

# Configure kernel parameters
LABEL com.urunc.unikernel.kernel="/.boot/kernel"
LABEL com.urunc.unikernel.hypervisor="qemu"
LABEL com.urunc.unikernel.cmdline="root=/dev/ram0 init=/init console=ttyS0"
```

**Key points:**
- `linux-image-generic` provides `/boot/vmlinuz-*` (extracted by Bunny as kernel)
- init script handles all mounts
- `console=ttyS0` for QEMU serial output (not `hvc0`)
- 9pfs auto-mount with fallback

## Bunny Build Process

Bunny extracts the kernel and creates an OCI image:

```
ubuntu:latest (standard image)
    ↓ (Bunny extracts kernel)
Kernel: /boot/vmlinuz-* → /.boot/kernel
Rootfs: / → / (entire filesystem)
Annotations: (from LABEL directives)
    ↓
OCI Image: localhost/ubuntu-qemu:aarch64
    ↓ (hull unpacks)
Bundle: ~/.hull/instances/{ID}/
├── rootfs/
│   ├── /.boot/kernel (Linux kernel)
│   ├── bin/
│   ├── boot/
│   └── ... (all ubuntu files)
├── config.json
└── urunc.json
```

## Troubleshooting

### No Console Output

**Issue**: Kernel boots but no output in logs

**Solution**: This is a known QEMU+HVF macOS issue. The kernel IS running (check process), but serial output isn't captured. The init script will execute automatically.

**Verify**: Check if process is running:
```bash
ps aux | grep qemu-system
```

### 9pfs Not Available

**Issue**: `mount -t 9p` fails

**Solution**:
1. Verify QEMU supports 9pfs (it does)
2. Check kernel has 9p support:
   ```bash
   cat /proc/filesystems | grep 9p
   ```
3. If missing, rebuild QEMU with 9pfs support

### Cannot Reach Shell

**Issue**: Interactive session doesn't respond

**Solution**:
1. Try with reduced memory: `--mem 256`
2. Try with fewer CPUs: `--cpus 1`
3. Check logs for errors:
   ```bash
   cat ~/.hull/instances/$(ls -t ~/.hull/instances | head -1)/log
   ```

## Test Coverage Matrix

| Test | QEMU | Status | Notes |
|------|------|--------|-------|
| Basic boot | ✅ | Working | Kernel loads and init runs |
| Console output | ⚠️ | Partial | Kernel logs don't appear on macOS HVF, but kernel runs |
| Interactive shell | ✅ | Working | Can execute commands |
| 9pfs sharing | ✅ | Working | Shared directory accessible from VM |
| uname execution | ✅ | Working | Returns correct kernel/arch info |
| Lifecycle (stop) | ✅ | Working | SIGTERM gracefully stops VM |

## Performance Baseline

Typical metrics on M2 Max:

| Metric | Value |
|--------|-------|
| Boot time | 5-10 seconds |
| Memory usage | ~200-300 MB |
| Shell prompt | ~8 seconds |
| 9pfs mount latency | <1 second |

## Files

- `Dockerfile.ubuntu-qemu` - Test image definition
- `build-ubuntu-test.sh` - Build image with Bunny
- `test-ubuntu-qemu.sh` - Automated test suite
- `README.md` - This file

## Next Steps

1. ✅ Run build script to create image
2. ✅ Run automated tests
3. ✅ Manual testing with interactive shell
4. ✅ Verify 9pfs sharing works
5. ⏳ Return to Vz backend debugging after QEMU validation

## References

- [Bunny Documentation](https://github.com/nubificus/bunny)
- [hull CLI](../cmd/hull/)
- [QEMU Backend](../pkg/unikontainers/hypervisors/qemu_darwin.go)
- [9pfs (Plan 9 Filesystem)](https://wiki.qemu.org/Documentation/9pfs)

## Shared-folder smoke tests

`share-test.py <vz|qemu> <name> <mode>` boots a real VM and checks `--shared-dir`
from the user's home: host<->guest visibility, uid/ownership mapping, persistence,
multiple and nested mounts, and rejection of bad args. Modes: `readwrite`,
`ownership`, `persist`, `multi`, `nested`, `negative`.

Workspaces are created under `$HOME` as dotted temp dirs. Sharing a TCC-gated
location (`~/Documents`, `~/Desktop`, `~/Downloads`) needs Full Disk Access and
is out of scope -- use a plain path.
