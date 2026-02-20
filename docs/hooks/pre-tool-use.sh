#!/bin/bash
# cw PreToolUse hook — routes tool calls through the approval gateway.
# Install: see https://codewire.sh/hooks/pre-tool-use.sh

# If no gateway session is running, allow everything.
if ! cw list 2>/dev/null | grep -qE "^[0-9]+[[:space:]]+gateway[[:space:]]+running"; then
    exit 0
fi

# Read tool input from stdin.
INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // "unknown"')
INPUT_STR=$(echo "$INPUT" | jq -r '.tool_input | tostring')

# Skip non-destructive read-only tools.
case "$TOOL" in
    Read|Glob|Grep|WebFetch|WebSearch|TodoRead|TaskList|TaskGet)
        exit 0 ;;
esac

# Send request to gateway and get raw reply.
REPLY=$(cw request gateway "$TOOL: $INPUT_STR" --timeout 30 --raw 2>/dev/null)
EXIT=$?

if [ $EXIT -ne 0 ] || [ -z "$REPLY" ]; then
    # Gateway unreachable or timeout — allow by default.
    exit 0
fi

UPPER_REPLY=$(echo "$REPLY" | tr '[:lower:]' '[:upper:]')

if echo "$UPPER_REPLY" | grep -q "^DENIED"; then
    REASON=$(echo "$REPLY" | sed 's/^[Dd][Ee][Nn][Ii][Ee][Dd][: ]*//')
    printf '{"decision":"block","reason":"Gateway denied: %s"}' "$REASON"
    exit 2
fi

# APPROVED or ESCALATE — allow (human will handle ESCALATE notification separately).
exit 0
