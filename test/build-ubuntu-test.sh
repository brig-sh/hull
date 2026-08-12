#!/bin/bash

# Build Ubuntu QEMU test image with Bunny

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"
TEST_DIR="$SCRIPT_DIR"

echo "=== Building Ubuntu QEMU Test Image ==="
echo ""

# Check if Bunny is available
if ! command -v bunny &> /dev/null; then
    echo "❌ Bunny not found. Install with:"
    echo "   brew install nubificus/tap/bunny"
    exit 1
fi

echo "Step 1: Building image with Bunny..."
cd "$TEST_DIR"

bunny build \
    -f Dockerfile.ubuntu-qemu \
    -t localhost/ubuntu-qemu:aarch64 \
    --platform linux/arm64 \
    2>&1 | tee build.log

if [ $? -ne 0 ]; then
    echo "❌ Bunny build failed"
    exit 1
fi

echo ""
echo "✓ Image built successfully"
echo ""

# The image should now be available as an OCI image
# Get the image digest
IMAGE_DIGEST=$(bunny inspect localhost/ubuntu-qemu:aarch64 2>/dev/null | grep -i digest | head -1 | awk '{print $NF}' || echo "unknown")

echo "Image digest: $IMAGE_DIGEST"
echo ""
echo "Next steps:"
echo "1. Test with QEMU backend:"
echo "   cd $PROJECT_DIR"
echo "   ./dist/urunc-macos_arm64 run --hypervisor qemu --mem 512 --cpus 2 localhost/ubuntu-qemu:aarch64"
echo ""
echo "2. Or run automated test:"
echo "   bash $TEST_DIR/test-ubuntu-qemu.sh"
