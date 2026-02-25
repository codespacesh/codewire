#!/usr/bin/env bash
# Multi-agent code review demo — all real cw commands.
# Runs in read-only ttyd; visitors watch the orchestration unfold.
set -e

CYAN='\033[36m'
GREEN='\033[32m'
YELLOW='\033[33m'
DIM='\033[2m'
BOLD='\033[1m'
RESET='\033[0m'

type_cmd() {
  local cmd="$1"
  printf "${CYAN}\$ ${RESET}"
  for ((i=0; i<${#cmd}; i++)); do
    printf "%s" "${cmd:$i:1}"
    sleep 0.03
  done
  echo ""
  sleep 0.3
  eval "$cmd"
  sleep 1
}

narrate() {
  echo ""
  echo -e "${DIM}─────────────────────────────────────────${RESET}"
  echo -e "${BOLD}$1${RESET}"
  echo -e "${DIM}─────────────────────────────────────────${RESET}"
  echo ""
  sleep 1.5
}

cleanup() {
  cw kill --all 2>/dev/null || true
  sleep 1
}

# Ensure clean state
cleanup

clear
echo -e "${CYAN}"
echo "  ╔═══════════════════════════════════════════╗"
echo -e "  ║   ${BOLD}CodeWire — AI Code Review Workflow${RESET}${CYAN}      ║"
echo "  ╚═══════════════════════════════════════════╝"
echo -e "${RESET}"
echo -e "  ${DIM}Multi-agent orchestration with real cw commands${RESET}"
echo ""
sleep 3

# ── Step 1: Launch three named agents ──
narrate "1. Launch three named agents"

type_cmd "cw launch planner --tag agent --tag plan -- bash -c '
echo \"Analyzing codebase...\"
sleep 4
echo \"Plan: 1) Refactor auth to JWT  2) Add token tests  3) Update API docs\"
sleep 3
echo \"Plan complete. Notifying coder.\"
sleep 30
'"

type_cmd "cw launch coder --tag agent --tag impl -- bash -c '
echo \"Waiting for plan...\"
sleep 8
echo \"Received plan. Starting implementation.\"
sleep 3
echo \"Refactoring auth module to JWT...\"
sleep 4
echo \"Adding token validation middleware...\"
sleep 3
echo \"Writing unit tests...\"
sleep 4
echo \"Implementation complete. 3 files changed, 47 tests passing.\"
sleep 20
'"

type_cmd "cw launch reviewer --tag agent --tag review -- bash -c '
echo \"Standing by for review assignment...\"
sleep 20
echo \"Reviewing changes...\"
sleep 4
echo \"Review: LGTM — clean JWT implementation, good test coverage.\"
sleep 10
'"

sleep 1

# ── Step 2: List all running agents ──
narrate "2. Show running agents"

type_cmd "cw list"
sleep 2

# ── Step 3: Send a direct message ──
narrate "3. Agent messaging — planner tells coder the plan"

type_cmd "cw msg -f planner coder 'Refactor auth: switch to JWT, add token expiry tests, update API docs'"
sleep 1.5

type_cmd "cw inbox coder"
sleep 2

# ── Step 4: Request/reply coordination ──
narrate "4. Request/reply — synchronous coordination"

echo -e "${DIM}Coder asks planner a question and blocks for the answer...${RESET}"
sleep 1

# Run request in background (it blocks waiting for reply)
cw request -f coder planner "Should tokens expire? What TTL?" &
REQUEST_PID=$!
sleep 2

# Get the request ID from planner's inbox and reply
REQ_ID=$(cw inbox planner 2>/dev/null | grep -o 'req_[a-zA-Z0-9_]*' | head -1)
if [ -n "$REQ_ID" ]; then
  type_cmd "cw reply $REQ_ID -f planner 'Yes — 24h TTL for access tokens, 7d for refresh tokens'"
else
  echo -e "${DIM}(reply sent)${RESET}"
fi

wait $REQUEST_PID 2>/dev/null || true
sleep 2

# ── Step 5: Subscribe to events ──
narrate "5. Event-driven orchestration"

echo -e "${DIM}Subscribing to real-time session events...${RESET}"
sleep 0.5

timeout 8 cw subscribe --tag agent --event session.status 2>/dev/null &
SUB_PID=$!
sleep 7
kill $SUB_PID 2>/dev/null || true
wait $SUB_PID 2>/dev/null || true
echo ""
sleep 1

# ── Step 6: Wait for all agents to complete ──
narrate "6. Wait for all agents to finish"

type_cmd "cw wait --tag agent --condition all --timeout 90"
sleep 1

# ── Step 7: Final status ──
narrate "7. Final status — all agents completed"

type_cmd "cw list"
sleep 3

# ── Done ──
echo ""
echo -e "${GREEN}"
echo "  ╔═══════════════════════════════════════════╗"
echo -e "  ║   ${BOLD}Demo complete!${RESET}${GREEN}                          ║"
echo -e "  ║                                           ║"
echo -e "  ║   ${RESET}${DIM}brew install codewiresh/codewire/codewire${RESET}${GREEN} ║"
echo -e "  ║   ${RESET}${DIM}https://codewire.sh${RESET}${GREEN}                      ║"
echo "  ╚═══════════════════════════════════════════╝"
echo -e "${RESET}"

# Keep the terminal open briefly so viewers see the final state
sleep 15

# Cleanup
cleanup
