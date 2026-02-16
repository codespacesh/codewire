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

### `cw launch [--dir <dir>] -- <command> [args...]`

Start a new session running the given command in a persistent PTY. Everything after `--` is the command and its arguments.

```bash
cw launch -- claude -p "refactor the database layer"
cw launch --dir /home/coder/project -- claude -p "add unit tests for auth"
cw launch -- aider --message "fix the login bug"
cw launch -- bash -c "npm test && npm run lint"
```

Options:
- `--dir`, `-d` — Working directory (defaults to current dir)

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

Terminate a session.

```bash
cw kill 3
cw kill --all
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
├── token                 # Auth token
├── config.toml           # Configuration (optional)
├── servers.toml          # Saved remote servers (optional)
├── sessions.json         # Session metadata
└── sessions/
    ├── 1/
    │   └── output.log    # Captured PTY output
    └── 2/
        └── output.log
```

### Configuration

All settings via `~/.codewire/config.toml` or environment variables:

```toml
[node]
name = "my-node"                    # CODEWIRE_NODE_NAME — node name for fleet
listen = "0.0.0.0:9100"             # CODEWIRE_LISTEN — WebSocket listener address
external_url = "wss://host/ws"      # CODEWIRE_EXTERNAL_URL — advertised URL for fleet attach

[nats]
url = "nats://nats.example.com:4222"   # CODEWIRE_NATS_URL
token = "secret"                        # CODEWIRE_NATS_TOKEN
creds_file = "~/.codewire/fleet.creds"  # CODEWIRE_NATS_CREDS
```

When no config file exists, codewire runs with defaults (Unix socket only, no WS, no fleet).

## Remote Access (WebSocket)

Enable WebSocket access by setting `listen` in [Configuration](#configuration), then start the daemon:

```bash
# On your local machine: save the remote server
cw server add my-server ws://remote-host:9100 --token <token>

# Use it
cw --server my-server list
cw --server my-server attach 1
```

WSS (TLS) is supported automatically — use `wss://` URLs for connections through TLS proxies like Caddy or Cloudflare.

## Fleet Discovery (NATS)

Discover and manage codewire daemons across multiple machines using NATS as the control plane. Fleet support is built into the binary — no feature flags needed.

See [Configuration](#configuration) for all config options. Fleet requires at minimum `[nats] url`.

### Fleet Commands

```bash
# Discover all nodes
cw fleet list
cw fleet list --json

# Launch a session on a specific node
cw fleet launch --on gpu-box -- claude -p "train the model"

# Send input to a remote session
cw fleet send gpu-box:1 "Status update?"

# Kill a remote session
cw fleet kill gpu-box:1

# Attach to a remote session (discovers URL via NATS, connects via WSS)
cw fleet attach gpu-box:1
```

### Architecture

- **NATS** = control plane (discovery, commands — JSON messages)
- **WSS** = data plane (PTY attach/streaming — binary frames)
- NATS never carries binary PTY data
- Nodes heartbeat every 30 seconds on `cw.fleet.heartbeat`

#### NATS Subjects

| Subject | Direction | Purpose |
|---------|-----------|---------|
| `cw.fleet.discover` | Broadcast | Discovery — all nodes reply with `NodeInfo` |
| `cw.fleet.heartbeat` | Publish | Nodes publish `NodeInfo` every 30s |
| `cw.<node>.list` | Request-reply | List sessions on a node |
| `cw.<node>.launch` | Request-reply | Launch session on a node |
| `cw.<node>.kill` | Request-reply | Kill session on a node |
| `cw.<node>.status` | Request-reply | Get session status on a node |
| `cw.<node>.send` | Request-reply | Send input to session on a node |

All messages are JSON-encoded `FleetRequest`/`FleetResponse` (see `internal/protocol/fleet_messages.go`). Binary PTY data never travels over NATS.

### Communication Model

Fleet > Node > Session

- **Fleet**: All nodes connected via NATS
- **Node**: A `cw` process on one machine
- **Session**: A PTY process (Claude, shell, etc.)

#### Session-to-session (same node)

```bash
cw send <id> "hello"       # Inject input into a session's stdin
cw watch <id>              # Stream a session's stdout
```

#### Session-to-session (across nodes, via NATS)

```bash
cw fleet send <node>:<id> "hello"    # Inject input via NATS
cw fleet attach <node>:<id>          # Stream output via WSS
```

#### Node-to-node

```bash
cw fleet list                            # Discover all nodes
cw fleet launch --on <node> -- <cmd>     # Launch remotely
cw fleet kill <node>:<id>                # Kill remotely
```

### Local Development (Docker Compose)

The repo includes a Docker Compose stack for local fleet development and testing:

- **NATS** — message broker on `localhost:4222` (monitor: `localhost:8222`)
- **Caddy** — TLS reverse proxy on `localhost:9443`
- **Codewire** — containerized node (`docker-test`) on `localhost:9100`

```bash
# Copy env file (optionally set ANTHROPIC_API_KEY for Claude e2e tests)
cp .env.example .env

# Start the stack
docker compose up -d --build

# Discover the containerized node
CODEWIRE_NATS_URL=nats://127.0.0.1:4222 cw fleet list

# Launch a session on the container
CODEWIRE_NATS_URL=nats://127.0.0.1:4222 cw fleet launch --on docker-test -- echo hello

# Tear down
docker compose down
```

## Multi-Agent Patterns

CodeWire supports full cross-session communication, enabling multi-agent collaboration:

### Multiple Attachments

Multiple clients can attach to the same session simultaneously. Perfect for pair programming or monitoring.

```bash
# Terminal 1: Attach to session
cw attach 1

# Terminal 2: Also attach to same session (both see output)
cw attach 1
```

### Supervisor Pattern

One orchestrator LLM coordinates multiple worker sessions:

```bash
# Launch worker agents
cw launch -- claude -p "implement feature X"  # Session 1
cw launch -- claude -p "write tests"          # Session 2

# From supervisor agent (via MCP or CLI):
cw status 1                                   # Check progress
cw watch 1 --timeout 30                       # Monitor for 30s
cw send 1 "Status update?\n"                  # Request update
```

### Agent Swarms

Multiple agents working in parallel on different tasks:

```bash
# Launch parallel agents
cw launch -- claude -p "optimize backend"     # Session 1
cw launch -- claude -p "optimize frontend"    # Session 2
cw launch -- claude -p "coordinate both"      # Session 3 (coordinator)

# Coordinator uses MCP or CLI to:
# - Monitor progress: cw watch 1
# - Send updates: cw send 1 "Frontend ready for integration"
# - Check completion: cw status 1
```

### Debugging & Monitoring

Watch another agent from a separate terminal:

```bash
# Launch agent
cw launch -- claude -p "fix auth bug"

# From another terminal, monitor progress
cw watch 1 --tail 100

# Send test input
cw send 1 "/help"

# Check detailed status
cw status 1
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

This exposes 7 tools:

| Tool | Description |
|------|-------------|
| `codewire_launch_session` | Launch new session |
| `codewire_list_sessions` | List sessions (filter by status) |
| `codewire_read_session_output` | Read output snapshot |
| `codewire_send_input` | Send input to a session |
| `codewire_watch_session` | Monitor session (time-bounded) |
| `codewire_get_session_status` | Get detailed status |
| `codewire_kill_session` | Terminate session |

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
