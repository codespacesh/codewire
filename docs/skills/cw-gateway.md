Start a Codewire approval gateway that auto-approves or auto-denies tool calls
from worker sessions based on a policy.

## Steps

1. Ensure the cw node is running: `cw node -d`

2. Start the gateway with an LLM policy:
   ```bash
   cw gateway --exec 'claude --dangerously-skip-permissions --print "You are an
   approval gateway. Policy: approve git operations, file reads/writes, builds.
   Deny: rm -rf on non-build paths, DROP TABLE, mass deletions. Escalate: anything
   unclear. Request from worker: $(cat). Reply with exactly:
   APPROVED
   DENIED: <reason>
   ESCALATE: <reason>"'
   ```

3. Tell the user the gateway is running. Workers can now call:
   `cw request gateway "approve: <action>"`

4. To add automatic Claude Code interception (all Bash/Edit/Write go through gateway):
   ```bash
   mkdir -p ~/.claude/hooks
   curl -fsSL https://codewire.sh/hooks/pre-tool-use.sh -o ~/.claude/hooks/pre-tool-use.sh
   chmod +x ~/.claude/hooks/pre-tool-use.sh
   ```
   Then add to `~/.claude/settings.json`:
   ```json
   {
     "hooks": {
       "PreToolUse": [{
         "hooks": [{"type": "command", "command": "~/.claude/hooks/pre-tool-use.sh"}]
       }]
     }
   }
   ```

## Notes

- The gateway creates a stub session named "gateway" — visible in `cw list`
- `cw kill gateway` stops the gateway and the stub session
- Workers inside cw sessions automatically have CW_SESSION_ID set, so
  `cw request gateway "..."` identifies them correctly
- Without `--exec`, the gateway auto-approves everything (useful for audit logging)
