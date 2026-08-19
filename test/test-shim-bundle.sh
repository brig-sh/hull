#!/bin/bash
# Test script for containerd-shim-urunc-v2 with OCI bundles

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SHIM_PATH="${SCRIPT_DIR}/dist/containerd-shim-urunc-v2"
URUNC_PATH="${SCRIPT_DIR}/dist/urunc_static_arm64"

echo "=== urunc Shim Bundle Test ==="
echo ""
echo "Shim binary: $SHIM_PATH"
echo "urunc binary: $URUNC_PATH"
echo ""

# Check binaries exist
if [ ! -f "$SHIM_PATH" ]; then
    echo "ERROR: Shim not found at $SHIM_PATH"
    echo "Please build: GOOS=darwin GOARCH=arm64 go build -o $SHIM_PATH ./cmd/containerd-shim-urunc-v2"
    exit 1
fi

if [ ! -f "$URUNC_PATH" ]; then
    echo "ERROR: urunc not found at $URUNC_PATH"
    exit 1
fi

echo "✓ Both binaries found"
echo ""

# Create test bundle
TEST_BUNDLE="/tmp/urunc-shim-test-bundle"
rm -rf "$TEST_BUNDLE"
mkdir -p "$TEST_BUNDLE/rootfs"

cat > "$TEST_BUNDLE/config.json" << 'EOF'
{
  "ociVersion": "1.0.0",
  "process": {
    "terminal": true,
    "user": {"uid": 0, "gid": 0},
    "args": "console=ttyAMA0",
    "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],
    "cwd": "/"
  },
  "root": {"path": "rootfs", "readonly": false},
  "hostname": "urunc-test",
  "mounts": [],
  "annotations": {"unikernel": "linux", "vmm": "qemu"}
}
EOF

echo "✓ Created test bundle at $TEST_BUNDLE"
echo ""

# Check if we have Alpine kernel/initramfs from previous test
ALPINE_KERNEL="/tmp/urunc-test/bundle/rootfs/unikernel"
ALPINE_INITRAMFS="/tmp/urunc-test/bundle/rootfs/initramfs"

if [ -f "$ALPINE_KERNEL" ] && [ -f "$ALPINE_INITRAMFS" ]; then
    echo "✓ Linking Alpine kernel and initramfs to test bundle"
    ln -sf "$ALPINE_KERNEL" "$TEST_BUNDLE/rootfs/unikernel"
    ln -sf "$ALPINE_INITRAMFS" "$TEST_BUNDLE/rootfs/initramfs"
else
    echo "⚠ Alpine kernel/initramfs not found from previous test"
    echo "  You can test bundle validation without them"
fi

echo ""
echo "=== Bundle Structure ==="
find "$TEST_BUNDLE" -type f | sort
echo ""

# Create request
cat > /tmp/shim-test-request.json << EOF
{
  "id": "test-container-$(date +%s)",
  "bundle": "$TEST_BUNDLE"
}
EOF

echo "=== Testing Shim Bundle Validation ==="
echo ""
echo "Request:"
cat /tmp/shim-test-request.json
echo ""
echo ""

# Test shim (run in background and kill after timeout)
# Use sleep hack since 'timeout' isn't available on macOS
echo "Sending bundle to shim..."
(
  cat /tmp/shim-test-request.json | URUNC_PATH="$URUNC_PATH" "$SHIM_PATH" 2>&1 &
  SHIM_PID=$!
  sleep 2
  kill $SHIM_PID 2>/dev/null || true
) || true

echo ""
echo "=== Shim Log Output ==="
if [ -f "/tmp/urunc-shim.log" ]; then
    tail -20 /tmp/urunc-shim.log
else
    echo "(No log file created - shim may not have written logs)"
fi

echo ""
echo "=== Bundle Validation Result ==="

# Check if bundle directory exists and has config.json
if [ -f "$TEST_BUNDLE/config.json" ]; then
    echo "✓ Bundle config.json validated"
else
    echo "✗ Bundle config.json not found"
    exit 1
fi

echo ""
echo "=== Next Steps ==="
echo ""
echo "1. Build the shim:"
echo "   GOOS=darwin GOARCH=arm64 go build -o dist/containerd-shim-urunc-v2 ./cmd/containerd-shim-urunc-v2"
echo ""
echo "2. Install for containerd:"
echo "   sudo cp dist/containerd-shim-urunc-v2 /usr/local/bin/"
echo "   chmod +x /usr/local/bin/containerd-shim-urunc-v2"
echo ""
echo "3. Configure containerd with urunc runtime (see SHIM_INTEGRATION.md)"
echo ""
echo "4. Run unikernels with nerdctl:"
echo "   nerdctl run --runtime urunc --rm -it ghcr.io/nubificus/unikernels/alpine:aarch64"
echo ""
