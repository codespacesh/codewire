---
name: codewire
description: Use codewire to launch, monitor, and coordinate persistent AI agent sessions
---

# Codewire

Persistent process server for AI coding agents. Launch sessions that survive disconnects, monitor progress, send input, and coordinate multiple agents.

## Commands

### Launch a session

```bash
cw launch -- <command> [args...]
cw launch --dir /path/to/project -- claude -p "fix the auth bug"
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
cw status <id>              # Human-readable
cw status <id> --json       # JSON output
```

### Kill sessions

```bash
cw kill <id>
cw kill --all
```

## Multi-Agent Patterns

### Supervisor: orchestrate workers

```bash
# Launch workers
cw launch -- claude -p "implement feature X"    # Session 1
cw launch -- claude -p "write tests for X"      # Session 2

# Monitor worker 1
cw watch 1 --timeout 30

# Check if done
cw status 1

# Send coordination message
cw send 1 "Tests are ready, please integrate"

# Read final output
cw logs 1 --tail 100
```

### Parallel agents

```bash
# Launch parallel tasks
cw launch -- claude -p "optimize backend queries"
cw launch -- claude -p "optimize frontend bundle"

# Check all statuses
cw list --json
```

### Cross-session communication

```bash
# Session 1 is running. Send it output from session 2:
cw send 1 "Session 2 finished with exit code 0"
```

## Fleet (multi-node)

Send commands to remote nodes via NATS:

```bash
cw fleet list                                          # Discover all nodes
cw fleet launch --on gpu-box -- claude -p "train"      # Launch on specific node
cw fleet send gpu-box:1 "Status update?"               # Send to remote session
cw fleet attach gpu-box:1                               # Attach to remote session
cw fleet kill gpu-box:1                                 # Kill remote session
```

## When to use codewire

- Launch long-running AI agent tasks that should survive disconnects
- Run multiple AI agents in parallel on different parts of a codebase
- Monitor what an agent is doing without interrupting it
- Coordinate between agents by sending messages between sessions
- Run background builds, tests, or other long processes
