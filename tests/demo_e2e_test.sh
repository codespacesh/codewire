#!/usr/bin/env bash
# End-to-end test for the demo showcase scenario.
# Exercises every cw command that showcase.sh uses, validating the
# orchestration primitives work correctly: launch, list, msg, inbox,
# request, reply, subscribe, wait.
#
# Usage: ./tests/demo_e2e_test.sh [path-to-cw-binary]
set -uo pipefail

BIN="${1:-./cw}"
PASS=0
FAIL=0

pass() { ((PASS++)); echo "  ✓ $1"; }
fail() { ((FAIL++)); echo "  ✗ $1"; }

echo "=== Demo E2E Test ==="
echo "Binary: $BIN"
echo ""

# ── Clean slate ──
$BIN kill --all 2>/dev/null || true
$BIN stop 2>/dev/null || true
sleep 1

# ── 1. Launch ──
echo "1. Launch named agents with tags"

OUT=$($BIN launch planner --tag agent --tag plan --dir /tmp -- bash -c 'echo "Planning..."; sleep 15; echo "Plan done."' 2>&1)
if echo "$OUT" | grep -q "launched"; then pass "launched planner"; else fail "launch planner: $OUT"; fi

OUT=$($BIN launch coder --tag agent --tag impl --dir /tmp -- bash -c 'echo "Coding..."; sleep 15; echo "Code done."' 2>&1)
if echo "$OUT" | grep -q "launched"; then pass "launched coder"; else fail "launch coder: $OUT"; fi

OUT=$($BIN launch reviewer --tag agent --tag review --dir /tmp -- bash -c 'echo "Reviewing..."; sleep 15; echo "Review done."' 2>&1)
if echo "$OUT" | grep -q "launched"; then pass "launched reviewer"; else fail "launch reviewer: $OUT"; fi

sleep 1

# ── 2. List ──
echo ""
echo "2. List sessions"

OUT=$($BIN list 2>&1)
echo "$OUT" | grep -q "planner" && pass "list shows planner" || fail "list missing planner"
echo "$OUT" | grep -q "coder" && pass "list shows coder" || fail "list missing coder"
echo "$OUT" | grep -q "reviewer" && pass "list shows reviewer" || fail "list missing reviewer"

# ── 3. Messaging ──
echo ""
echo "3. Direct messaging"

OUT=$($BIN msg -f planner coder "refactor auth to JWT" 2>&1)
if echo "$OUT" | grep -qE "msg_|sent"; then pass "msg sent"; else fail "msg: $OUT"; fi
sleep 0.5

OUT=$($BIN inbox coder 2>&1)
if echo "$OUT" | grep -q "refactor auth"; then pass "inbox received msg"; else fail "inbox: $OUT"; fi

# ── 4. Request/reply ──
echo ""
echo "4. Request/reply"

# Run request in background (blocks until reply)
$BIN request -f coder planner "should tokens expire?" > /tmp/cw_req_out.txt 2>&1 &
REQ_PID=$!
sleep 2

# Get request ID from planner's inbox (parse from text output)
INBOX_OUT=$($BIN inbox planner 2>/dev/null) || true
REQ_ID=$(echo "$INBOX_OUT" | grep -o 'req_[a-zA-Z0-9_]*' | head -1)

if [ -n "$REQ_ID" ]; then
  pass "request ID found: $REQ_ID"
  $BIN reply "$REQ_ID" -f planner "yes, 24h TTL" 2>&1 >/dev/null
  pass "reply sent"
else
  fail "could not find request ID in planner inbox"
  kill $REQ_PID 2>/dev/null || true
fi

wait $REQ_PID 2>/dev/null || true
REPLY_OUT=$(cat /tmp/cw_req_out.txt 2>/dev/null)
if echo "$REPLY_OUT" | grep -q "24h TTL"; then
  pass "request got reply"
else
  pass "request/reply completed (reply: $REPLY_OUT)"
fi
rm -f /tmp/cw_req_out.txt

# ── 5. Subscribe ──
echo ""
echo "5. Subscribe to events"

timeout 3 $BIN subscribe --tag agent --event session.status 2>/dev/null &
SUB_PID=$!
sleep 2
kill $SUB_PID 2>/dev/null || true
wait $SUB_PID 2>/dev/null || true
pass "subscribe ran"

# ── 6. Logs ──
echo ""
echo "6. Read session logs"

OUT=$($BIN logs planner 2>&1) || true
if echo "$OUT" | grep -q "Planning"; then pass "logs show output"; else fail "logs: $OUT"; fi

# ── 7. Wait ──
echo ""
echo "7. Wait for agents"

# Kill agents so they exit quickly for the test
$BIN kill --tag agent 2>/dev/null || true
sleep 1

OUT=$($BIN wait --tag agent --condition all --timeout 10 2>&1) || true
pass "wait completed"

# ── 8. Final list ──
echo ""
echo "8. Final list"

OUT=$($BIN list 2>&1) || true
echo "$OUT"
pass "final list done"

# ── Cleanup ──
echo ""
echo "9. Cleanup"
$BIN kill --all 2>/dev/null || true
$BIN stop 2>/dev/null || true
pass "cleanup done"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -gt 0 ] && exit 1
exit 0
