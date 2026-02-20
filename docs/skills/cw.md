Launch a background Codewire session running Claude Code on the given task.

Task: $ARGUMENTS

## Steps

1. Derive a short session name from the task: 3–4 words, kebab-case (e.g. "build-auth-module")

2. Run the session in the background:
   ```bash
   cw run --name <slug> -- claude --dangerously-skip-permissions --print "<task>"
   ```

3. Confirm launch — show the exact command used and the session name.

4. Tell the user they can track it:
   - `cw watch <name>` — stream output live
   - `cw logs <name>` — view buffered output
   - `cw wait <name>` — block until complete
   - `cw list` — see all running sessions

## Notes

- `--dangerously-skip-permissions` is needed because the session is non-interactive —
  Claude cannot respond to permission prompts from inside a detached PTY.
- If the task involves file writes, git operations, or shell commands, mention this to
  the user before launching so they can decide whether to proceed.
- If the node isn't running, start it first: `cw node -d`
