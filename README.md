# Codewire

Persistent process server for AI coding agents. Like tmux, but purpose-built for long-running LLM processes — with native terminal scrolling, copy/paste, and no weird key chords.

Codewire runs as a background node inside your development environment (e.g., a Coder workspace). You launch AI agent sessions with a prompt, and they keep running even if you disconnect. Reconnect anytime to pick up where you left off.

Works with any CLI-based AI agent: Claude Code, Aider, Goose, Codex, or anything else.

## Install

### Homebrew (macOS/Linux)

```bash
brew install codespacesh/codewire/codewire
```

### Install Script

```bash
curl -fsSL https://raw.githubusercontent.com/codespacesh/codewire/main/install.sh | bash
```

This downloads the latest binary, verifies its SHA256 checksum (and GPG signature if `gpg` is installed), and installs `cw` to `/usr/local/bin`.

Options:

```bash
# Install a specific version
curl -fsSL .../install.sh | bash -s -- --version v0.1.0

# Install to a custom prefix
curl -fsSL .../install.sh | bash -s -- --prefix ~/.local
```

### Build from source

```bash
go build -o cw ./cmd/cw
sudo mv cw /usr/local/bin/cw
```

Or use `make`:

```bash
make install
```

## Quick Start

```bash
# Launch a Claude Code session (daemon auto-starts)
cw launch -- claude -p "fix the auth bug in login.ts"

# Use a different AI agent
cw launch -- aider --message "fix the auth bug"
cw launch -- goose run

# Specify a working directory
cw launch --dir /home/coder/project -- claude -p "add tests"

# List running sessions
cw list

# Attach to a session
cw attach 1

# Detach: Ctrl+B then d

# View what the agent did while you were away
cw logs 1
```

## Commands

### `cw launch [--dir <dir>] [--tag <tag>...] -- <command> [args...]`

Start a new session running the given command in a persistent PTY. Everything after `--` is the command and its arguments. Tags enable filtering and coordination.

```bash
cw launch -- claude -p "refactor the database layer"
cw launch --dir /home/coder/project -- claude -p "add unit tests for auth"
cw launch --tag worker --tag build -- claude -p "fix tests"
cw launch -- bash -c "npm test && npm run lint"
```

Options:
- `--dir`, `-d` — Working directory (defaults to current dir)
- `--tag`, `-t` — Tag the session (repeatable)

### `cw list`

Show all sessions with their status, age, and prompt.

```bash
cw list
cw list --json   # machine-readable output
```

### `cw attach <id>`

Take over your terminal and connect to a running session. You get full terminal I/O — native scrolling, native copy/paste, everything your terminal emulator supports.

Detach with **Ctrl+B d** (press Ctrl+B, release, then press d). The session keeps running.

### `cw logs <id>`

View captured output from a session without attaching.

```bash
cw logs 1              # full output
cw logs 1 --follow     # tail -f style, streams new output
cw logs 1 --tail 100   # last 100 lines
```

Works on completed sessions too — review what the agent did after it finished.

### `cw kill <id>`

Terminate a session. Supports tag-based filtering.

```bash
cw kill 3
cw kill --all
cw kill --tag worker          # Kill all sessions tagged "worker"
```

### `cw send <id> [input]`

Send input to a session without attaching. Useful for multi-agent coordination.

```bash
cw send 1 "Status update?"                    # Send text with newline
cw send 1 "test" --no-newline                 # No newline
echo "command" | cw send 1 --stdin            # From stdin
cw send 1 --file commands.txt                 # From file
```

### `cw watch <id>`

Monitor a session in real-time without attaching. Perfect for observing another agent's progress.

```bash
cw watch 1                      # Watch with recent history
cw watch 1 --tail 50            # Start from last 50 lines
cw watch 1 --no-history         # Only new output
cw watch 1 --timeout 60         # Auto-exit after 60 seconds
```

### `cw status <id>`

Get detailed session status including PID, output size, and recent output.

```bash
cw status 1                     # Human-readable format
cw status 1 --json              # JSON output
```

### `cw subscribe [node] [--tag <tag>] [--event <type>]`

Subscribe to real-time session events. Events stream until you disconnect.

```bash
cw subscribe --tag worker                           # Events from sessions tagged "worker"
cw subscribe --event session.status                  # Only status change events
cw subscribe dev-1 --tag build                       # Events from remote node
```

### `cw wait [node:]<id> [--tag <tag>] [--condition all|any] [--timeout <seconds>]`

Block until sessions complete.

```bash
cw wait 3                                            # Wait for session 3 to complete
cw wait --tag worker --condition all                 # Wait for ALL workers to complete
cw wait --tag worker --condition any --timeout 60    # Wait for ANY worker, 60s timeout
```

### `cw nodes`

List all nodes registered with the relay.

```bash
cw nodes
```

### `cw setup [relay-url]`

Authorize this node with a relay using the device authorization flow.

```bash
cw setup https://relay.codespace.sh
```

### `cw relay`

Run a relay server. The relay provides WireGuard tunneling, node discovery, and shared KV storage.

```bash
cw relay --base-url https://relay.example.com --data-dir /data/relay
```

### `cw kv`

Shared key-value store (requires relay connection).

```bash
cw kv set build_status done                          # Set key
cw kv get build_status                               # Get key
cw kv list task:                                     # List by prefix
cw kv set --ttl 60s lock "node-a"                    # Auto-expiring key
cw kv set --ns myproject key value                   # Namespaced
cw kv delete build_status                            # Delete key
```

### `cw mcp-server`

Start an MCP (Model Context Protocol) server for programmatic access.

```bash
cw mcp-server
```

See [MCP Integration](#mcp-integration) section below for details.

### `cw start` / `cw node`

Start the daemon manually. Usually you don't need this — the daemon auto-starts on first CLI invocation.

```bash
cw start
```

### `cw stop`

Stop the running daemon gracefully.

```bash
cw stop
```

### `cw server`

Manage saved remote server connections.

```bash
cw server add my-gpu ws://gpu-host:9100 --token <token>   # Save a server
cw server remove my-gpu                                    # Remove it
cw server list                                             # List saved servers
```

Saved servers can be referenced by name with `--server`:

```bash
cw --server my-gpu list
cw --server my-gpu attach 1
```

## How It Works

Codewire is a single Go binary (`cw`) that acts as both daemon and CLI client.

**Node** (`cw node`): Listens on a Unix socket at `~/.codewire/codewire.sock`. Manages PTY sessions — each AI agent runs in its own pseudoterminal. The node owns the master side of each PTY and keeps processes alive regardless of client connections.

**Client** (`cw launch`, `attach`, etc.): Connects to the daemon's Unix socket. When you attach, the client puts your terminal in raw mode and bridges your stdin/stdout directly to the PTY. Your terminal emulator handles all rendering — that's why scrolling and copy/paste work natively.

**Logs**: All PTY output is teed to `~/.codewire/sessions/<id>/output.log` so you can review what happened while disconnected.

### Using Different AI Agents

Pass the exact command you want to run after `--`. No magic — what you type is what runs:

```bash
cw launch -- claude -p "fix the bug"              # Claude Code
cw launch -- aider --message "fix the bug"         # Aider
cw launch -- goose run                             # Goose
cw launch -- codex "refactor auth"                 # Codex
```

### Wire Protocol

Communication between client and daemon uses a frame-based binary protocol over the Unix socket:

- Frame format: `[type: u8][length: u32 BE][payload]`
- Type `0x00`: Control messages (JSON) — launch, list, attach, detach, kill, resize
- Type `0x01`: Data messages (raw bytes) — PTY I/O

### Data Directory

```
~/.codewire/
├── codewire.sock         # Unix domain socket
├── codewire.pid          # Node PID file
├── token                 # Auth token (for direct WS fallback)
├── wg_private_key        # WireGuard private key (0600)
├── config.toml           # Configuration (optional)
├── servers.toml          # Saved remote servers (optional)
├── sessions.json         # Session metadata
└── sessions/
    ├── 1/
    │   ├── output.log    # Captured PTY output
    │   └── events.jsonl  # Metadata event log
    └── 2/
        ├── output.log
        └── events.jsonl
```

### Configuration

All settings via `~/.codewire/config.toml` or environment variables:

```toml
[node]
name = "my-node"                          # CODEWIRE_NODE_NAME
listen = "0.0.0.0:9100"                   # CODEWIRE_LISTEN — direct WebSocket (optional)
external_url = "wss://host/ws"            # CODEWIRE_EXTERNAL_URL
relay_url = "https://relay.codespace.sh"  # CODEWIRE_RELAY_URL — opt-in remote access
```

When no config file exists, codewire runs in standalone mode (Unix socket only, no relay).

## Remote Access (WireGuard Relay)

Codewire uses WireGuard tunneling for remote access. Nodes establish userspace WireGuard tunnels to a relay server — no root required, works behind NAT.

### Quick Setup

```bash
# Authorize your node with a relay
cw setup https://relay.codespace.sh

# That's it. Your node is now accessible remotely.
```

The setup flow generates a WireGuard key pair, authorizes via a device code (browser-based), and starts the tunnel.

### Remote Commands

All commands accept an optional node prefix for remote access:

```bash
# Local (no prefix)
cw list                                    # Local sessions
cw attach 3                                # Local session

# Remote (node prefix)
cw nodes                                   # List all nodes from relay
cw list dev-1                              # Sessions on dev-1
cw attach dev-1:3                          # Session 3 on dev-1
cw launch dev-1 -- claude -p "fix bug"     # Launch on dev-1
cw kill dev-1:3                            # Kill on dev-1
```

### Direct WebSocket (alternative)

You can also connect directly via WebSocket without a relay:

```bash
cw server add my-server wss://remote-host:9100 --token <token>
cw --server my-server list
cw --server my-server attach 1
```

### Architecture

```
                          INTERNET
                             |
                    +--------+--------+
                    |   cw relay      |
                    | HTTPS :443      |  <- Clients connect here
                    | WG UDP :41820   |  <- Nodes tunnel here
                    | /api/v1/nodes   |  <- Node discovery
                    +--------+--------+
                        |         |
           WG tunnel    |         |   WG tunnel
           (UDP)        |         |   (UDP)
                        |         |
                +-------+--+  +---+-------+
                | cw node  |  | cw node   |
                | "dev-1"  |  | "gpu-box" |
                +----------+  +-----------+
```

- **Relay** = WireGuard hub + HTTP API (node discovery, shared KV, device auth)
- **Nodes** = userspace WireGuard clients, serve HTTP/WS on tunnel listener
- **Clients** = connect via relay, same wire protocol as local Unix socket

### Local Development (Docker Compose)

The repo includes a Docker Compose stack:

- **Relay** — WireGuard + HTTP API on `localhost:8080`
- **Caddy** — TLS reverse proxy on `localhost:9443` with wildcard subdomain support
- **Codewire** — containerized node (`docker-test`) on `localhost:9100`

```bash
# Start the stack
docker compose up -d --build

# List nodes
cw nodes

# Tear down
docker compose down
```

## LLM Orchestration

CodeWire is designed for LLM-driven multi-agent workflows. Tags, subscriptions, and wait provide structured coordination primitives.

### Tags

Label sessions at launch for filtering and coordination:

```bash
cw launch --tag worker --tag build -- claude -p "fix tests"
cw launch --tag worker --tag lint -- claude -p "fix lint issues"

# List sessions by tag (via MCP or API)
# Kill all workers
cw kill --tag worker
```

### Subscribe to Events

Stream structured events from sessions:

```bash
# Watch all status changes for "worker" sessions
cw subscribe --tag worker --event session.status

# Subscribe to all events from session 3
cw subscribe --session 3
```

Event types: `session.created`, `session.status`, `session.output_summary`, `session.input`, `session.attached`, `session.detached`

### Wait for Completion

Block until sessions finish — replaces polling:

```bash
# Wait for session 3 to complete
cw wait 3

# Wait for ALL workers to complete
cw wait --tag worker --condition all

# Wait for ANY worker to complete, 60s timeout
cw wait --tag worker --condition any --timeout 60
```

### Multi-Agent Patterns

**Supervisor pattern** — one orchestrator coordinates workers:

```bash
# Launch tagged workers
cw launch --tag worker -- claude -p "implement feature X"
cw launch --tag worker -- claude -p "write tests for X"

# Wait for all workers to finish
cw wait --tag worker --condition all --timeout 300

# Check results
cw list
```

**Agent swarms** — parallel agents with cross-session communication:

```bash
cw launch --tag backend -- claude -p "optimize queries"
cw launch --tag frontend -- claude -p "optimize bundle"

# Send coordination message
cw send 1 "Backend ready for integration"

# Monitor in real-time
cw watch 2 --tail 100
```

**Multiple attachments** — multiple clients can attach to the same session:

```bash
# Terminal 1
cw attach 1

# Terminal 2 (both see output)
cw attach 1
```

## Claude Code Integration

### Skill (recommended)

Install the codewire skill so Claude Code knows how to use `cw` for session management:

```bash
curl -fsSL https://raw.githubusercontent.com/codespacesh/codewire/main/.claude/skills/install.sh | bash
```

This installs two skills to `~/.claude/skills/`:
- **codewire** — teaches Claude Code to launch, monitor, and coordinate sessions
- **codewire-dev** — development workflow for contributing to the codewire codebase

### MCP Server (optional)

For programmatic tool access, add CodeWire as an MCP server:

```bash
# User-level (available in all projects)
claude mcp add --scope user codewire -- cw mcp-server

# Or project-level
claude mcp add codewire -- cw mcp-server
```

This exposes 14 tools:

| Tool | Description |
|------|-------------|
| `codewire_launch_session` | Launch new session (with tags) |
| `codewire_list_sessions` | List sessions with enriched metadata |
| `codewire_read_session_output` | Read output snapshot |
| `codewire_send_input` | Send input to a session |
| `codewire_watch_session` | Monitor session (time-bounded) |
| `codewire_get_session_status` | Get detailed status (exit code, duration, etc.) |
| `codewire_kill_session` | Terminate session (by ID or tags) |
| `codewire_subscribe` | Subscribe to session events |
| `codewire_wait_for` | Block until sessions complete |
| `codewire_list_nodes` | List nodes from relay |
| `codewire_kv_set` | Set key-value (shared KV store) |
| `codewire_kv_get` | Get value by key |
| `codewire_kv_list` | List keys by prefix |
| `codewire_kv_delete` | Delete key |

## Contributing

```bash
# Build
make build

# Run unit tests
make test

# Run all tests (unit + integration)
go test ./internal/... ./tests/... -timeout 120s

# Lint
make lint

# Manual CLI test
make test-manual

# Run with debug logging
cw node  # slog outputs to stderr by default
```

## Security

Release binaries are signed with GPG. The public key is committed to this repository at [`GPG_PUBLIC_KEY.asc`](GPG_PUBLIC_KEY.asc).

To verify a release:

```bash
# Import the public key
curl -fsSL https://raw.githubusercontent.com/codespacesh/codewire/main/GPG_PUBLIC_KEY.asc | gpg --import

# Verify checksums signature
gpg --verify SHA256SUMS.asc SHA256SUMS

# Verify binary checksum
sha256sum --check --ignore-missing SHA256SUMS
```

## License

MIT
