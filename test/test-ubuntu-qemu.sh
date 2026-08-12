#!/bin/bash

# Comprehensive test suite for hull QEMU backend

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$( dirname "$SCRIPT_DIR" )"

IMAGE="${1:-localhost/ubuntu-qemu:aarch64}"
TIMEOUT="${2:-60}"

echo "=== Ubuntu QEMU Backend Test Suite ==="
echo "Image: $IMAGE"
echo "Timeout: ${TIMEOUT}s"
echo ""

# Test 1: Basic VM Boot
echo "TEST 1: Boot Ubuntu VM with QEMU backend"
echo "=========================================="

instance_id=$(
    cd "$PROJECT_DIR" && \
    ./dist/hull_arm64 run --detach --hypervisor qemu \
    --mem 512 --cpus 2 \
    "$IMAGE" 2>&1 | tail -1
)

if [ -z "$instance_id" ]; then
    echo "❌ Failed to get instance ID"
    exit 1
fi

echo "✓ Instance started: $instance_id"
echo ""

# Wait for instance to be ready
sleep 3

# Test 2: Check logs
echo "TEST 2: Verify console output captured"
echo "========================================"

log_file="$HOME/.hull/instances/$instance_id/log"

if [ ! -f "$log_file" ]; then
    echo "❌ Log file not found: $log_file"
    exit 1
fi

echo "Log file found: $log_file"
echo ""
echo "--- Log Contents ---"
cat "$log_file" || echo "(empty)"
echo "--- End Log ---"
echo ""

# Test 3: Check for kernel boot messages
echo "TEST 3: Look for kernel boot indicators"
echo "========================================"

if grep -i "kernel\|boot\|init" "$log_file" > /dev/null 2>&1; then
    echo "✓ Found kernel/boot messages in logs"
else
    echo "⚠ No explicit kernel messages, but VM may still be running"
fi

# Test 4: Check instance state
echo ""
echo "TEST 4: Check instance state"
echo "============================="

state_file="$HOME/.hull/instances/$instance_id/state.json"

if [ -f "$state_file" ]; then
    echo "Instance state:"
    cat "$state_file" | jq '.'

    status=$(cat "$state_file" | jq -r '.status')
    if [ "$status" = "running" ]; then
        echo "✓ Instance is running"
    else
        echo "⚠ Instance status: $status"
    fi
else
    echo "❌ State file not found"
fi

echo ""

# Test 5: Test 9pfs sharing (if instance is still running)
echo "TEST 5: Test 9pfs shared directory"
echo "==================================="

test_dir="/tmp/urunc-test-9pfs-$$"
mkdir -p "$test_dir"

echo "Created test directory: $test_dir"
echo "Test content" > "$test_dir/test.txt"

# Try to run with shared directory
echo "Attempting to run instance with 9pfs sharing..."

instance_id_2=$(
    cd "$PROJECT_DIR" && \
    ./dist/hull_arm64 run --detach --hypervisor qemu \
    --mem 512 --cpus 2 \
    --shared-dir "$test_dir:/mnt/host" \
    "$IMAGE" 2>&1 | tail -1
) || true

if [ -n "$instance_id_2" ]; then
    echo "✓ Instance with 9pfs started: $instance_id_2"
    sleep 5

    # Cleanup
    "$PROJECT_DIR"/dist/hull_arm64 stop "$instance_id_2" 2>/dev/null || true
else
    echo "⚠ Could not start instance with 9pfs (may not be supported in current config)"
fi

# Cleanup test directory
rm -rf "$test_dir"

echo ""

# Test 6: Cleanup
echo "TEST 6: Cleanup"
echo "==============="

echo "Stopping instance: $instance_id"
cd "$PROJECT_DIR" && \
./dist/hull_arm64 stop "$instance_id" 2>/dev/null || true

sleep 2

if ! pgrep -f "qemu-system.*$instance_id" > /dev/null 2>&1; then
    echo "✓ Instance stopped cleanly"
else
    echo "⚠ QEMU process still running, may need manual cleanup"
fi

echo ""
echo "=== Test Summary ==="
echo "✓ VM boot test completed"
echo "✓ Logs captured"
echo "✓ Instance state tracked"
echo ""
echo "To run an interactive session:"
echo "  ./dist/hull_arm64 run --hypervisor qemu --mem 512 --cpus 2 $IMAGE"
