#!/bin/bash

# Prepare Ubuntu bundle with minimal initrd approach
# The initrd is kept small by only including essential utilities
# The full rootfs is provided separately and mounted after boot

set -e

BUNDLE_DIR="${1:-/tmp/ubuntu-minimal-bundle}"

echo "=== Preparing Ubuntu with Minimal Initrd ==="
echo "Bundle: $BUNDLE_DIR"
echo ""

rm -rf "$BUNDLE_DIR"
mkdir -p "$BUNDLE_DIR/rootfs/.boot"

# Step 1: Extract full Ubuntu rootfs
echo "Step 1: Extracting Ubuntu rootfs..."
TEMP_ROOT=$(mktemp -d)
trap "rm -rf $TEMP_ROOT" EXIT

CONTAINER=$(docker create ubuntu-test-qemu:latest /bin/sh)
trap "docker rm -f $CONTAINER > /dev/null 2>&1; rm -rf $TEMP_ROOT" EXIT

docker export $CONTAINER | tar -xC "$TEMP_ROOT" 2>&1 | tail -1

# Copy kernel
mkdir -p "$BUNDLE_DIR/rootfs/.boot"
cp "$TEMP_ROOT/boot/vmlinuz" "$BUNDLE_DIR/rootfs/.boot/kernel" || \
cp "$TEMP_ROOT/boot/vmlinuz-"* "$BUNDLE_DIR/rootfs/.boot/kernel"

echo "✓ Extracted (kernel: $(du -h $BUNDLE_DIR/rootfs/.boot/kernel | cut -f1))"

# Step 2: Create minimal initrd directory structure
echo "Step 2: Creating minimal initrd..."
INITRD_DIR=$(mktemp -d)
trap "rm -rf $INITRD_DIR; ${trap_cleanup:-true}" EXIT

mkdir -p "$INITRD_DIR"/{bin,sbin,etc,lib,lib64,dev,proc,sys,mnt,root}

# Copy essential binaries
for bin in bash sh mount unmount mkdir mknod cat ls; do
    if [ -f "$TEMP_ROOT/bin/$bin" ]; then
        cp "$TEMP_ROOT/bin/$bin" "$INITRD_DIR/bin/"
    elif [ -f "$TEMP_ROOT/usr/bin/$bin" ]; then
        cp "$TEMP_ROOT/usr/bin/$bin" "$INITRD_DIR/bin/"
    fi
done

for bin in mount umount modprobe; do
    if [ -f "$TEMP_ROOT/sbin/$bin" ]; then
        cp "$TEMP_ROOT/sbin/$bin" "$INITRD_DIR/sbin/"
    elif [ -f "$TEMP_ROOT/usr/sbin/$bin" ]; then
        cp "$TEMP_ROOT/usr/sbin/$bin" "$INITRD_DIR/sbin/"
    fi
done

# Copy libraries (this is tricky without ldd, so we'll use a minimal set)
for lib in /lib /lib64 /usr/lib /usr/lib64; do
    if [ -d "$TEMP_ROOT$lib" ]; then
        # Copy a minimal set of libraries
        cp -r "$TEMP_ROOT$lib"/ld-* "$INITRD_DIR/lib/" 2>/dev/null || true
        cp -r "$TEMP_ROOT$lib"/libc.* "$INITRD_DIR/lib/" 2>/dev/null || true
        cp -r "$TEMP_ROOT$lib"/libm.* "$INITRD_DIR/lib/" 2>/dev/null || true
    fi
done

# Create init script for minimal initrd
cat > "$INITRD_DIR/init" << 'INIT_SCRIPT'
#!/bin/sh
set -e
echo "=== Minimal Initrd ==="
# Mount minimal filesystems
mount -t proc none /proc 2>/dev/null || true
mount -t sysfs none /sys 2>/dev/null || true

# Create device nodes
[ -c /dev/null ] || mknod /dev/null c 1 3
[ -c /dev/zero ] || mknod /dev/zero c 1 5
[ -c /dev/console ] || mknod /dev/console c 5 1
[ -c /dev/tty ] || mknod /dev/tty c 5 0

echo "Init: kernel $(cat /proc/version)"
echo "Init: Mounting 9pfs as root..."

# Mount 9pfs as root - this contains the real Ubuntu filesystem
mkdir -p /rootfs
if mount -t 9p -o trans=virtio,version=9p2000.L,posixacl,cache=loose fs0 /rootfs 2>/dev/null; then
    echo "Init: 9pfs mounted successfully"
else
    echo "Init: 9pfs mount failed!"
    sh
    exit 1
fi

# Switch to real root and execute init from there
cd /rootfs
mount --move /proc /rootfs/proc 2>/dev/null || mount --bind /proc /rootfs/proc 2>/dev/null || true
mount --move /sys /rootfs/sys 2>/dev/null || mount --bind /sys /rootfs/sys 2>/dev/null || true

# Execute the real init from the 9pfs mount
if [ -x /rootfs/init ]; then
    exec switch_root /rootfs /init
elif [ -x /rootfs/sbin/init ]; then
    exec switch_root /rootfs /sbin/init
else
    exec /bin/sh
fi
INIT_SCRIPT

chmod +x "$INITRD_DIR/init"

# Create cpio archive
echo "Creating initrd archive..."
cd "$INITRD_DIR"
find . -print0 | cpio -0 -o -H newc 2>/dev/null | gzip > "$BUNDLE_DIR/rootfs/.boot/initrd.gz"

INITRD_SIZE=$(du -h "$BUNDLE_DIR/rootfs/.boot/initrd.gz" | cut -f1)
echo "✓ Minimal initrd: $INITRD_SIZE"

# Step 3: Copy full rootfs to bundle
echo "Step 3: Copying full rootfs to bundle..."
# Copy everything except what's already in .boot
rsync -a --exclude='.boot' "$TEMP_ROOT/" "$BUNDLE_DIR/rootfs/" 2>/dev/null || \
cp -r "$TEMP_ROOT"/* "$BUNDLE_DIR/rootfs/" 2>/dev/null

echo "✓ Full rootfs ready"

# Step 4: Create init script in rootfs
cat > "$BUNDLE_DIR/rootfs/init" << 'ROOT_INIT'
#!/bin/sh
# Real init script in the actual rootfs
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

echo "=== Ubuntu Booted ===" > /dev/console
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true

echo "Kernel: $(uname -r)" > /dev/console
echo "Machine: $(uname -m)" > /dev/console

# Check for 9pfs (shared files)
if grep -q 9p /proc/filesystems 2>/dev/null; then
    if [ ! -d /mnt/host ]; then
        mkdir -p /mnt/host
        mount -t 9p -o trans=virtio fs1 /mnt/host 2>/dev/null || true
    fi
fi

echo "Launching bash..." > /dev/console
exec /bin/bash -i
ROOT_INIT

chmod +x "$BUNDLE_DIR/rootfs/init"

# Step 5: Create OCI config
echo "Step 4: Creating OCI config..."
cat > "$BUNDLE_DIR/config.json" << 'CONFIG'
{
  "ociVersion": "1.0.0",
  "process": {
    "terminal": true,
    "user": {"uid": 0, "gid": 0},
    "args": ["/init"]
  },
  "root": {"path": "rootfs", "readonly": false},
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
echo "Usage:"
echo "  ./dist/hull_arm64 run --hypervisor qemu --mem 512 --cpus 2 $BUNDLE_DIR"
echo ""
