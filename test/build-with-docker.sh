#!/bin/bash

# Build the Ubuntu test image using docker build with Bunny syntax

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"

echo "=== Building Ubuntu Test Image with Docker + Bunny ==="
echo ""

# Enable BuildKit (required for #syntax directive)
export DOCKER_BUILDKIT=1

echo "Building image with docker build..."
echo "This uses Bunny as the BuildKit frontend (via #syntax directive)"
echo ""

cd "$SCRIPT_DIR"

docker build \
    -f Dockerfile.ubuntu-qemu \
    -t localhost/ubuntu-qemu:aarch64 \
    --platform linux/arm64 \
    . 2>&1 | tee build.log

if [ $? -eq 0 ]; then
    echo ""
    echo "✓ Image built successfully!"
    echo "  Name: localhost/ubuntu-qemu:aarch64"
    echo ""
    echo "Next steps:"
    echo "1. Extract bundle from image"
    echo "   bash $SCRIPT_DIR/extract-bundle.sh"
    echo ""
    echo "2. Or run directly:"
    echo "   cd $PROJECT_DIR"
    echo "   ./dist/urunc-macos_arm64 run --hypervisor qemu --mem 512 --cpus 2 localhost/ubuntu-qemu:aarch64"
else
    echo ""
    echo "❌ Build failed"
    echo "Check build.log for details"
    exit 1
fi
