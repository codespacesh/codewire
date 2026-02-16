---
name: codewire
description: Use codewire to launch, monitor, and coordinate persistent AI agent sessions
---

# Codewire

Persistent process server for AI coding agents. Launch sessions that survive disconnects, monitor progress, send input, and coordinate multiple agents. Tags, subscriptions, and wait provide structured LLM orchestration primitives.

## Commands

### Launch a session

```bash
cw launch -- <command> [args...]
cw launch --dir /path/to/project -- claude -p "fix the auth bug"
cw launch --tag worker --tag build -- claude -p "fix tests"
```

### List sessions

```bash
cw list                # Human-readable table
cw list --json         # Machine-readable
```

### Attach to a session

```bash
cw attach <id>         # Full terminal I/O
# Detach: Ctrl+B then d
```

### View output without attaching

```bash
cw logs <id>              # Full output
cw logs <id> --follow     # Stream new output
cw logs <id> --tail 100   # Last 100 lines
```

### Monitor in real-time

```bash
cw watch <id>                      # Watch with recent history
cw watch <id> --tail 50            # Start from last 50 lines
cw watch <id> --no-history         # Only new output
cw watch <id> --timeout 60         # Auto-exit after 60 seconds
```

### Send input to a session

```bash
cw send <id> "your message"                  # Send text + newline
cw send <id> "text" --no-newline             # No trailing newline
echo "command" | cw send <id> --stdin        # From stdin
cw send <id> --file commands.txt             # From file
```

### Check session status

```bash
cw status <id>              # Human-readable (exit code, duration, tags, output stats)
cw status <id> --json       # JSON output
```

### Kill sessions

```bash
cw kill <id>
cw kill --all
cw kill --tag worker         # Kill all sessions tagged "worker"
```

### Subscribe to events

```bash
cw subscribe --tag worker                           # Events from worker sessions
cw subscribe --event session.status                  # Status change events
cw subscribe --session 3                             # All events from session 3
```

### Wait for completion

```bash
cw wait 3                                            # Wait for session 3
cw wait --tag worker --condition all                 # Wait for ALL workers
cw wait --tag worker --condition any --timeout 60    # ANY worker, 60s timeout
```

### Remote nodes

```bash
cw nodes                                             # List all nodes
cw list dev-1                                        # Sessions on remote node
cw attach dev-1:3                                    # Remote session
cw launch dev-1 --tag worker -- claude -p "fix bug"  # Launch on remote node
```

### Shared KV store

```bash
cw kv set build_status done
cw kv get build_status
cw kv list task:                                     # List by prefix
cw kv set --ttl 60s lock "node-a"                    # Auto-expiring
cw kv delete build_status
```

## LLM Orchestration Patterns

### Supervisor: orchestrate workers

```bash
# Launch tagged workers
cw launch --tag worker -- claude -p "implement feature X"
cw launch --tag worker -- claude -p "write tests for X"

# Wait for all to complete
cw wait --tag worker --condition all --timeout 300

# Check results
cw list --json
```

### Parallel agents

```bash
cw launch --tag backend -- claude -p "optimize queries"
cw launch --tag frontend -- claude -p "optimize bundle"

# Subscribe to status events
cw subscribe --tag backend --event session.status
```

### Cross-session communication

```bash
# Session 1 is running. Send it output from session 2:
cw send 1 "Session 2 finished with exit code 0"
```

## When to use codewire

- Launch long-running AI agent tasks that should survive disconnects
- Run multiple AI agents in parallel on different parts of a codebase
- Monitor what an agent is doing without interrupting it
- Coordinate between agents by sending messages between sessions
- Wait for agent completion instead of polling
- Tag and filter sessions for structured orchestration
- Run background builds, tests, or other long processes
