#!/usr/bin/env bash
set -e

# Manual integration test for auto-attach feature
# This tests the actual CLI behavior end-to-end

BIN="${1:-./target/release/cw}"

echo "=== Testing Auto-Attach Feature ==="
echo "Using binary: $BIN"
echo

# Clean slate
$BIN stop 2>/dev/null || true
sleep 1

echo "1. Launch 3 sessions with different timestamps..."
$BIN launch -- bash -c "echo Session-1; sleep 100" > /dev/null
sleep 0.2
$BIN launch -- bash -c "echo Session-2; sleep 100" > /dev/null
sleep 0.2
$BIN launch -- bash -c "echo Session-3; sleep 100" > /dev/null
sleep 1

echo "2. List sessions:"
$BIN list
echo

echo "3. Verify help text shows optional ID:"
if ! $BIN attach --help | grep -q "omit to auto-select"; then
    echo "ERROR: Help text doesn't mention auto-select"
    exit 1
fi
echo "✓ Help text correct"
echo

echo "4. Test error with no sessions after cleanup:"
$BIN kill --all > /dev/null
sleep 1

if $BIN attach 2>&1 | grep -q "No unattached running sessions available"; then
    echo "✓ Error message correct when no sessions available"
else
    echo "ERROR: Expected 'No unattached running sessions available' message"
    exit 1
fi
echo

# Stop daemon
$BIN stop 2>/dev/null || true

echo "=== All manual tests passed! ==="
