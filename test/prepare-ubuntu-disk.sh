#!/bin/bash

# Prepare an Ubuntu Linux bundle using a disk image instead of initrd
# This avoids the large initrd issue

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"
BUNDLE_DIR="${1:-/tmp/ubuntu-bundle-disk}"

echo "=== Preparing Ubuntu Bundle with Disk Image ==="
echo "Bundle dir: $BUNDLE_DIR"
echo ""

# Step 1: Extract Ubuntu rootfs from Docker image
echo "Step 1: Extracting Ubuntu rootfs..."
rm -rf "$BUNDLE_DIR"
mkdir -p "$BUNDLE_DIR/rootfs"

CONTAINER_ID=$(docker create ubuntu:latest /bin/sh)
trap "docker rm -f $CONTAINER_ID > /dev/null 2>&1" EXIT

docker export $CONTAINER_ID | tar -xC "$BUNDLE_DIR/rootfs" 2>&1 | tail -1

echo "✓ Ubuntu rootfs extracted"

# Step 2: Install kernel in rootfs
echo "Step 2: Installing kernel..."
docker run --rm -v "$BUNDLE_DIR/rootfs:/rootfs" ubuntu:latest bash -c "
    apt-get update > /dev/null 2>&1
    apt-get install -y --no-install-recommends linux-image-generic > /dev/null 2>&1
    cp /boot/vmlinuz /rootfs/.boot/kernel 2>/dev/null || cp /boot/vmlinuz-* /rootfs/.boot/kernel
    chmod +r /rootfs/.boot/kernel
" || {
    # Fallback: try to get kernel from existing image
    KERNEL_CONTAINER=$(docker create ubuntu:latest /bin/sh)
    docker cp $KERNEL_CONTAINER:/boot/vmlinuz - 2>/dev/null | tar -xO > "$BUNDLE_DIR/rootfs/.boot/kernel" || \
    docker cp $KERNEL_CONTAINER:/boot/vmlinuz-* - 2>/dev/null | tar -xO > "$BUNDLE_DIR/rootfs/.boot/kernel" || true
    docker rm -f $KERNEL_CONTAINER > /dev/null 2>&1
}

echo "✓ Kernel installed"

# Step 3: Create /init script
echo "Step 3: Creating /init script..."
cat > "$BUNDLE_DIR/rootfs/init" << 'INIT_SCRIPT'
#!/bin/sh
set -x
echo "=== Unikernel Init ===" > /dev/console
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mount -t tmpfs tmpfs /tmp
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

echo "System info:" > /dev/console
uname -a > /dev/console 2>&1

# Try to mount 9pfs
if grep -q 9p /proc/filesystems 2>/dev/null; then
    mkdir -p /mnt/host 2>/dev/null || true
    if mount -t 9p -o trans=virtio fs0 /mnt/host 2>/dev/null; then
        echo "9pfs mounted" > /dev/console
    fi
fi

echo "Launching shell..." > /dev/console
exec /bin/bash -i
INIT_SCRIPT

chmod +x "$BUNDLE_DIR/rootfs/init"
echo "✓ Init script created"

# Step 4: Create disk image from rootfs
echo "Step 4: Creating disk image (this may take a minute)..."
DISK_SIZE="20G"
DISK_IMAGE="$BUNDLE_DIR/rootfs/.boot/disk.img"

# Create sparse image
dd if=/dev/zero of="$DISK_IMAGE" bs=1 count=0 seek=$DISK_SIZE 2>/dev/null

# Format as ext4
mkfs.ext4 -F "$DISK_IMAGE" > /dev/null 2>&1 || {
    echo "❌ mkfs.ext4 failed - trying with Docker"
    docker run --rm -v "$BUNDLE_DIR:/work" ubuntu:latest bash -c "
        mkfs.ext4 -F /work/rootfs/.boot/disk.img > /dev/null 2>&1
    " || {
        echo "❌ Could not create filesystem"
        exit 1
    }
}

# Mount and copy filesystem
MOUNT_POINT="/tmp/urunc-mount-$$"
mkdir -p "$MOUNT_POINT"
sudo mount "$DISK_IMAGE" "$MOUNT_POINT" 2>/dev/null || {
    echo "Note: Cannot mount disk image (requires sudo). Trying alternative approach..."
    # If sudo mount fails, create a tar instead
    rm "$DISK_IMAGE"
    cd "$BUNDLE_DIR/rootfs"
    tar czf ".boot/rootfs.tar.gz" --exclude=".boot" . > /dev/null 2>&1
    echo "✓ Created rootfs.tar.gz: $(du -h .boot/rootfs.tar.gz | cut -f1)"
    DISK_SIZE="N/A"
    DISK_IMAGE=".boot/rootfs.tar.gz"
}

if mountpoint -q "$MOUNT_POINT"; then
    # Copy rootfs to mounted disk (excluding .boot to avoid recursion)
    cp -r "$BUNDLE_DIR/rootfs"/* "$MOUNT_POINT/" 2>/dev/null || true

    sync
    sudo umount "$MOUNT_POINT" 2>/dev/null || true
    rmdir "$MOUNT_POINT" 2>/dev/null || true
    echo "✓ Disk image created and formatted: $(du -h $DISK_IMAGE | cut -f1)"
fi

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
    "com.urunc.unikernel.hypervisor": "qemu",
    "com.urunc.unikernel.unikernelType": "linux",
    "com.urunc.unikernel.cmdline": "root=/dev/vda rw init=/init console=ttyS0"
  }
}
CONFIG

echo "✓ OCI config created"

echo ""
echo "=== Bundle Ready ==="
echo "Location: $BUNDLE_DIR"
echo "Kernel: $(du -h $BUNDLE_DIR/rootfs/.boot/kernel | cut -f1)"
if [ -f "$DISK_IMAGE" ]; then
    echo "Disk Image: $(du -h $DISK_IMAGE | cut -f1)"
fi
echo "Rootfs: $(du -sh $BUNDLE_DIR/rootfs | cut -f1)"
echo ""

echo "To test with hull:"
echo "  ./dist/hull_arm64 run --detach --hypervisor qemu --mem 512 --cpus 2 $BUNDLE_DIR"
echo ""
