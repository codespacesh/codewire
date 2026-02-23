Launch a background Codewire session running Claude Code on the given task.

Task: $ARGUMENTS

## Steps

1. Derive a short session name from the task: 3-4 words, kebab-case (e.g. "build-auth-module")

2. If launching multiple related sessions (cohort), pick a shared tag:
   ```bash
   cw run --name researcher-1 research-batch -- claude --dangerously-skip-permissions --print "<task1>"
   cw run --name researcher-2 research-batch -- claude --dangerously-skip-permissions --print "<task2>"
   ```
   Or use the positional tag syntax: `cw run <name> <tag> -- <command>`

3. For a single session:
   ```bash
   cw run --name <slug> -- claude --dangerously-skip-permissions --print "<task>"
   ```

4. Confirm launch -- show the exact command used and the session name.

5. Tell the user they can track it:
   - `cw watch <name>` -- stream output live
   - `cw logs <name>` -- view buffered output
   - `cw wait <name>` -- block until complete
   - `cw wait --tag <tag>` -- wait for entire cohort, prints captured results
   - `cw list` -- see all running sessions

## Cohort Pattern

For fan-out/fan-in work (multiple researchers, parallel tasks):

```bash
# Launch cohort
cw run researcher-1 my-cohort -- claude --dangerously-skip-permissions --print "Research topic A"
cw run researcher-2 my-cohort -- claude --dangerously-skip-permissions --print "Research topic B"

# Wait for all results (auto-captured output is printed)
cw wait --tag my-cohort

# Mid-run coordination via KV store
cw kv set --ns my-cohort status "phase-1-complete"
cw kv get --ns my-cohort status
cw kv list --ns my-cohort
```

## Environment Variables

Sessions automatically receive these env vars:
- `CW_SESSION_ID` -- numeric session ID
- `CW_SESSION_NAME` -- session name (if set)
- `CW_COHORT_TAG` -- first tag (if set), useful for siblings to discover each other

## Notes

- `--dangerously-skip-permissions` is needed because the session is non-interactive --
  Claude cannot respond to permission prompts from inside a detached PTY.
- If the task involves file writes, git operations, or shell commands, mention this to
  the user before launching so they can decide whether to proceed.
- If the node isn't running, start it first: `cw node -d`
- `cw wait --tag <tag>` automatically prints captured output (last 200 lines, ANSI-stripped) for each session.
- `cw kv` provides a local in-memory KV store for coordination between siblings. Supports TTL (`--ttl 5m`) and namespaces (`--ns batch`).
