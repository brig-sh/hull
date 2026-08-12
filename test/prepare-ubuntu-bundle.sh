#!/bin/bash

# Prepare an Ubuntu Linux bundle for urunc-macos QEMU backend
# This manually extracts the kernel and creates an initrd (since Bunny is not available)

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"
BUNDLE_DIR="${1:-/tmp/ubuntu-bundle}"
DOCKER_IMAGE="${2:-ubuntu-test-qemu}"

echo "=== Preparing Ubuntu Bundle for QEMU ==="
echo "Bundle dir: $BUNDLE_DIR"
echo "Docker image: $DOCKER_IMAGE"
echo ""

# Step 1: Check if Docker image already exists, otherwise build
echo "Step 1: Checking Docker image..."
cd "$SCRIPT_DIR"

if ! docker inspect "$DOCKER_IMAGE:latest" > /dev/null 2>&1; then
    echo "Building Docker image..."
    docker build --platform linux/arm64 \
        -f Dockerfile.test-linux \
        -t "$DOCKER_IMAGE:latest" \
        . > /dev/null 2>&1 || {
        echo "❌ Failed to build Docker image"
        exit 1
    }
    echo "✓ Docker image built: $DOCKER_IMAGE:latest"
else
    echo "✓ Docker image already exists: $DOCKER_IMAGE:latest"
fi
echo ""

# Step 2: Create a temporary container and extract files
echo "Step 2: Extracting kernel and rootfs from image..."
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Create container from image
CONTAINER_ID=$(docker create "$DOCKER_IMAGE:latest" /bin/sh)
trap "docker rm -f $CONTAINER_ID > /dev/null 2>&1; rm -rf $TEMP_DIR" EXIT

echo "Created temp container: $CONTAINER_ID"

# Export the entire filesystem
docker export "$CONTAINER_ID" | tar -x -C "$TEMP_DIR" 2>/dev/null || {
    echo "❌ Failed to export container"
    exit 1
}

echo "✓ Exported filesystem"

# Step 3: Prepare bundle directory
echo "Step 3: Setting up bundle structure..."
rm -rf "$BUNDLE_DIR"
mkdir -p "$BUNDLE_DIR/rootfs"

# Copy the entire rootfs
cp -r "$TEMP_DIR"/* "$BUNDLE_DIR/rootfs/" 2>/dev/null || true

# Ensure kernel exists at .boot/kernel
mkdir -p "$BUNDLE_DIR/rootfs/.boot"

if [ -f "$BUNDLE_DIR/rootfs/boot/vmlinuz" ]; then
    cp "$BUNDLE_DIR/rootfs/boot/vmlinuz" "$BUNDLE_DIR/rootfs/.boot/kernel"
    echo "✓ Kernel found and copied: $(du -h $BUNDLE_DIR/rootfs/.boot/kernel | cut -f1)"
elif [ -f "$BUNDLE_DIR/rootfs/boot/vmlinuz-$(ls -t $BUNDLE_DIR/rootfs/boot/vmlinuz-* 2>/dev/null | head -1 | xargs -I {} basename {})" ]; then
    cp "$(ls -t $BUNDLE_DIR/rootfs/boot/vmlinuz-* 2>/dev/null | head -1)" "$BUNDLE_DIR/rootfs/.boot/kernel"
    echo "✓ Kernel found and copied: $(du -h $BUNDLE_DIR/rootfs/.boot/kernel | cut -f1)"
else
    echo "❌ Could not find kernel in rootfs"
    exit 1
fi

# Step 4: Create initrd from rootfs
echo "Step 4: Creating initrd..."
cd "$BUNDLE_DIR/rootfs"

# Create cpio archive of the entire rootfs (this becomes the initrd)
# We need to exclude the kernel itself to avoid duplication
find . -type f -not -path './.boot/kernel' -not -path '*/vmlinuz*' -print0 | \
    cpio -0 -o -H newc 2>/dev/null | \
    gzip > "$BUNDLE_DIR/rootfs/.boot/initrd.gz"

INITRD_SIZE=$(du -h "$BUNDLE_DIR/rootfs/.boot/initrd.gz" | cut -f1)
echo "✓ Initrd created: $INITRD_SIZE"

# Step 5: Create OCI config
echo "Step 5: Creating OCI bundle config..."

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
    "cwd": "/"
  },
  "root": {
    "path": "rootfs",
    "readonly": false
  },
  "hostname": "ubuntu-vm",
  "annotations": {
    "com.urunc.unikernel.kernel": "/.boot/kernel",
    "com.urunc.unikernel.initrd": "/.boot/initrd.gz",
    "com.urunc.unikernel.hypervisor": "qemu",
    "com.urunc.unikernel.unikernelType": "linux",
    "com.urunc.unikernel.cmdline": "root=/dev/ram0 init=/init console=ttyS0 rw"
  }
}
CONFIG

echo "✓ OCI config created"

echo ""
echo "=== Bundle Ready ==="
echo "Location: $BUNDLE_DIR"
echo "Kernel: $(du -h $BUNDLE_DIR/rootfs/.boot/kernel | cut -f1)"
echo "Initrd: $INITRD_SIZE"
echo "Rootfs: $(du -sh $BUNDLE_DIR/rootfs | cut -f1)"
echo ""

echo "To test with urunc-macos:"
echo "  cd $PROJECT_DIR"
echo "  ./dist/urunc-macos_arm64 run --detach --hypervisor qemu --mem 512 --cpus 2 --bundle $BUNDLE_DIR"
echo ""
echo "Or to run interactively:"
echo "  ./dist/urunc-macos_arm64 run --hypervisor qemu --mem 512 --cpus 2 --bundle $BUNDLE_DIR"
echo ""
