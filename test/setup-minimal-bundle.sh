#!/bin/bash

# Create a minimal Linux bundle for testing without Bunny
# Uses Alpine Linux + downloaded kernel

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"
BUNDLE_DIR="/tmp/test-ubuntu-bundle"

echo "=== Creating Minimal Linux Test Bundle ==="
echo ""

# Clean up old bundle
rm -rf "$BUNDLE_DIR"
mkdir -p "$BUNDLE_DIR/rootfs"

echo "Step 1: Prepare rootfs..."

# Use Alpine Linux as base (lighter than Ubuntu)
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/aarch64/alpine-minirootfs-latest-aarch64.tar.gz"

echo "Downloading Alpine rootfs..."
curl -sL "$ALPINE_URL" -o /tmp/alpine.tar.gz

echo "Extracting rootfs..."
tar -xzf /tmp/alpine.tar.gz -C "$BUNDLE_DIR/rootfs"

# Create init script
echo "Step 2: Create init script..."

cat > "$BUNDLE_DIR/rootfs/init" << 'INIT_SCRIPT'
#!/bin/sh
set -e

# Mount essential filesystems
echo "Mounting filesystems..."
mount -t proc proc /proc || true
mount -t sysfs sysfs /sys || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mount -t tmpfs tmpfs /tmp || true

# Show system info
echo ""
echo "=== Linux System Ready ==="
echo "Kernel: $(uname -r)"
echo "Arch: $(uname -m)"
echo "Hostname: $(hostname)"
echo ""

# Try to mount 9pfs if available
if grep -q 9p /proc/filesystems 2>/dev/null; then
    mkdir -p /mnt/host 2>/dev/null || true
    if mount -t 9p -o trans=virtio fs0 /mnt/host 2>/dev/null; then
        echo "✓ 9pfs mounted at /mnt/host"
        ls -la /mnt/host 2>/dev/null || echo "  (empty or inaccessible)"
    fi
fi

echo ""
echo "=== Launching shell ==="
echo ""

# Launch interactive shell
exec /bin/sh
INIT_SCRIPT

chmod +x "$BUNDLE_DIR/rootfs/init"

echo "Step 3: Get Linux kernel..."

# Try to get aarch64 kernel
# Option 1: Alpine Linux kernel
KERNEL_URL="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/aarch64/netboot/kernel-lts"

echo "Downloading kernel from Alpine..."
if ! curl -sL --max-time 10 "$KERNEL_URL" -o "$BUNDLE_DIR/rootfs/kernel" 2>/dev/null; then
    echo "⚠ Failed to download Alpine kernel, trying alternative..."

    # Fallback: Create minimal kernel symlink (won't work, but we'll catch it)
    echo "Creating placeholder kernel path..."
    mkdir -p "$BUNDLE_DIR/rootfs/.boot"
    touch "$BUNDLE_DIR/rootfs/.boot/kernel"
fi

# Check if we got a kernel
if [ -s "$BUNDLE_DIR/rootfs/kernel" ]; then
    echo "✓ Kernel downloaded ($(du -h "$BUNDLE_DIR/rootfs/kernel" | cut -f1))"
    cp "$BUNDLE_DIR/rootfs/kernel" "$BUNDLE_DIR/rootfs/.boot/kernel"
else
    echo "❌ Could not get kernel - trying from system..."

    # Try to use system kernel (if running Linux)
    if [ -f "/boot/vmlinuz-$(uname -r)" ]; then
        echo "Using system kernel..."
        cp "/boot/vmlinuz-$(uname -r)" "$BUNDLE_DIR/rootfs/.boot/kernel"
    else
        echo "❌ No kernel available"
        echo "Options:"
        echo "1. Install Bunny: brew install nubificus/tap/bunny"
        echo "2. Or provide kernel path via: --kernel /path/to/kernel"
        exit 1
    fi
fi

echo "Step 4: Create OCI bundle config..."

# Create minimal OCI config
cat > "$BUNDLE_DIR/config.json" << 'CONFIG'
{
  "ociVersion": "1.0.0",
  "process": {
    "terminal": true,
    "user": {
      "uid": 0,
      "gid": 0
    },
    "args": ["/init"],
    "env": [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      "TERM=xterm"
    ],
    "cwd": "/",
    "capabilities": {
      "bounding": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_SETFCAP", "CAP_SYS_ADMIN"],
      "effective": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_SETFCAP", "CAP_SYS_ADMIN"],
      "inheritable": [],
      "permitted": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_SETFCAP", "CAP_SYS_ADMIN"],
      "ambient": []
    }
  },
  "root": {
    "path": "rootfs",
    "readonly": false
  },
  "hostname": "test-linux",
  "mounts": [
    {
      "destination": "/proc",
      "type": "proc",
      "source": "proc"
    },
    {
      "destination": "/sys",
      "type": "sysfs",
      "source": "sysfs"
    }
  ]
}
CONFIG

echo "Step 5: Create urunc.json annotations..."

cat > "$BUNDLE_DIR/urunc.json" << 'URUNC'
{
  "annotations": {
    "com.urunc.unikernel.kernel": "/.boot/kernel",
    "com.urunc.unikernel.hypervisor": "qemu",
    "com.urunc.unikernel.unikernelType": "linux",
    "com.urunc.unikernel.cmdline": "root=/dev/ram0 init=/init console=ttyS0"
  }
}
URUNC

echo ""
echo "✓ Bundle created at: $BUNDLE_DIR"
echo ""
echo "Bundle structure:"
ls -la "$BUNDLE_DIR"

echo ""
echo "Test with:"
echo "  cd $PROJECT_DIR"
echo "  ./dist/hull_arm64 run --hypervisor qemu --mem 512 --cpus 2 --kernel $BUNDLE_DIR/rootfs/.boot/kernel --rootfs $BUNDLE_DIR/rootfs"
echo ""
echo "Or run automated test:"
echo "  bash $SCRIPT_DIR/test-qemu-bundle.sh $BUNDLE_DIR"
