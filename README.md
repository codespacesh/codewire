# Codewire

Persistent process server for AI coding agents. Like tmux, but purpose-built for long-running LLM processes — with native terminal scrolling, copy/paste, and no weird key chords.

Codewire runs as a daemon inside your development environment (e.g., a Coder workspace). You launch AI agent sessions with a prompt, and they keep running even if you disconnect. Reconnect anytime to pick up where you left off.

Works with any CLI-based AI agent: Claude Code, Aider, Goose, Codex, or anything else.

## Install

### Homebrew (macOS/Linux)

```bash
brew tap codespacesh/tap
brew install codewire
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

### Manual install with verification

```bash
# Download binary, checksums, and signature
VERSION=v0.1.0
TARGET=aarch64-apple-darwin  # or x86_64-apple-darwin, x86_64-unknown-linux-musl, aarch64-unknown-linux-gnu
curl -fsSL -O "https://github.com/codespacesh/codewire/releases/download/${VERSION}/cw-${VERSION}-${TARGET}"
curl -fsSL -O "https://github.com/codespacesh/codewire/releases/download/${VERSION}/SHA256SUMS"
curl -fsSL -O "https://github.com/codespacesh/codewire/releases/download/${VERSION}/SHA256SUMS.asc"

# Verify GPG signature
curl -fsSL https://raw.githubusercontent.com/codespacesh/codewire/main/GPG_PUBLIC_KEY.asc | gpg --import
gpg --verify SHA256SUMS.asc SHA256SUMS

# Verify checksum
sha256sum --check --ignore-missing SHA256SUMS  # Linux
shasum -a 256 --check --ignore-missing SHA256SUMS  # macOS

# Install
chmod +x "cw-${VERSION}-${TARGET}"
sudo mv "cw-${VERSION}-${TARGET}" /usr/local/bin/cw
```

### Build from source

```bash
cargo install --path .
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

Start an MCP (Model Context Protocol) server for programmatic access. Compile with `--features mcp` flag.

```bash
# Build with MCP support
cargo build --release --features mcp

# Start MCP server
cw mcp-server
```

See [MCP Integration](#mcp-integration) section below for details.

### `cw start` / `cw daemon`

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

Codewire is a single Rust binary (`cw`) that acts as both daemon and CLI client.

**Daemon** (`cw daemon`): Listens on a Unix socket at `~/.codewire/server.sock`. Manages PTY sessions — each AI agent runs in its own pseudoterminal. The daemon owns the master side of each PTY and keeps processes alive regardless of client connections.

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
├── server.sock           # Unix domain socket
├── daemon.pid            # Daemon PID file
├── sessions.json         # Session metadata
└── sessions/
    ├── 1/
    │   └── output.log    # Captured PTY output
    └── 2/
        └── output.log
```

## Remote Access (WebSocket)

Connect to a codewire daemon running on a remote machine over WebSocket:

```bash
# On the remote machine: start daemon with WS listener
cat >> ~/.codewire/config.toml << 'EOF'
[daemon]
listen = "0.0.0.0:9100"
EOF
cw daemon

# On your local machine: save the remote server
cw server add my-server ws://remote-host:9100 --token <token>

# Use it
cw --server my-server list
cw --server my-server attach 1
```

WSS (TLS) is supported automatically — use `wss://` URLs for connections through TLS proxies like Caddy or Cloudflare.

## Fleet Discovery (NATS)

Discover and manage codewire daemons across multiple machines using NATS as the control plane. Build with the `nats` feature:

```bash
cargo build --features nats
```

> **Note:** Fleet commands require building with `--features nats`. Pre-built release binaries do not include fleet support.

### Configuration

```toml
# ~/.codewire/config.toml
[daemon]
name = "gpu-box"
listen = "0.0.0.0:9100"
external_url = "wss://9100--gpu.coder.codespace.sh/ws"

[nats]
url = "nats://nats.example.com:4222"
creds_file = "~/.codewire/fleet.creds"
```

Or via environment variables (zero-config in codespace.sh workspaces):

```bash
export CODEWIRE_NATS_URL="nats://nats.example.com:4222"
export CODEWIRE_NATS_CREDS="~/.codewire/fleet.creds"
export CODEWIRE_EXTERNAL_URL="wss://9100--myworkspace.coder.codespace.sh/ws"
```

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

# Run fleet tests
cargo test --features nats --test fleet

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

## MCP Integration

CodeWire provides an MCP (Model Context Protocol) server for programmatic access from AI agents like Claude Code.

### Building with MCP Support

```bash
cargo build --release --features mcp
```

### Available MCP Tools

The MCP server exposes these tools:

| Tool | Description | Key Parameters |
|------|-------------|----------------|
| `codewire_list_sessions` | Discover sessions | `status_filter: "all"\|"running"\|"completed"` |
| `codewire_read_session_output` | Read output snapshot | `session_id, tail?, max_chars?` |
| `codewire_send_input` | Send input to session | `session_id, input, auto_newline?` |
| `codewire_watch_session` | Monitor session (time-bounded) | `session_id, include_history?, history_lines?, max_duration_seconds?` |
| `codewire_get_session_status` | Get detailed status | `session_id` |
| `codewire_launch_session` | Launch new session | `command, working_dir?` |
| `codewire_kill_session` | Terminate session | `session_id` |

### Using from Claude Code

Add CodeWire MCP server to your Claude Code configuration:

```json
{
  "mcpServers": {
    "codewire": {
      "command": "/path/to/cw",
      "args": ["mcp-server"]
    }
  }
}
```

Then use the tools in your prompts:

```
Use codewire_list_sessions to see what agents are running.
Use codewire_watch_session(session_id=1) to monitor progress.
Use codewire_send_input(session_id=1, input="Status update?\n") to communicate.
```

### Example: Multi-Agent Workflow

```python
# Supervisor agent workflow via MCP

# 1. Launch worker sessions
codewire_launch_session(command=["claude", "-p", "implement feature X"])
# Returns: session_id=1

codewire_launch_session(command=["claude", "-p", "write tests"])
# Returns: session_id=2

# 2. Monitor progress
codewire_watch_session(session_id=1, max_duration_seconds=30)
# Returns: output stream for 30 seconds

# 3. Check status
codewire_get_session_status(session_id=1)
# Returns: detailed status, PID, output size

# 4. Send coordination messages
codewire_send_input(session_id=1, input="Tests ready, please integrate\n")

# 5. Read results
codewire_read_session_output(session_id=1, tail=100)
```

## LLM-Friendly Development

Codewire is designed to be developed BY LLMs, FOR LLMs. We provide a comprehensive Claude Code skill that makes contributing easy.

### Quick Start for LLM Development

```bash
# Install the codewire-dev skill
curl -fsSL https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/install.sh | bash

# Or if you're in the repo
./.claude/skills/install.sh
```

**Usage in Claude Code:**
```
Use the codewire-dev skill to implement new features
```

The skill provides:
- ✅ Project structure overview
- ✅ Step-by-step command implementation guide
- ✅ Testing patterns and conventions
- ✅ Coder CLI integration examples
- ✅ Common pitfalls and how to avoid them
- ✅ Quick reference commands

### Key LLM Development Principles

**1. Test-Driven:** Always run tests before and after changes
```bash
make test-all  # Runs unit, integration, and CLI tests
```

**2. Short Test Names:** Unix socket paths have length limits
```rust
// ❌ BAD
let dir = temp_dir("auto-attach-skip-completed");

// ✅ GOOD
let dir = temp_dir("auto-skip-done");
```

**3. Add Integration Tests:** Every feature needs an end-to-end test
```rust
#[tokio::test]
async fn test_new_feature() {
    let dir = temp_dir("new-feat");
    let sock = start_test_daemon(&dir).await;
    // Test implementation
}
```

**4. Manual CLI Testing:** Use the automated script
```bash
./tests/manual_test.sh ./target/release/cw
```

### Integration with Coder

Codewire + Coder = Perfect remote development. The skill includes comprehensive examples:

```bash
# Launch long-running build on remote Coder workspace
coder ssh myworkspace -- cw launch -- cargo build --release

# Monitor from local machine
coder ssh myworkspace -- cw watch 1

# Attach interactively
coder ssh myworkspace -t -- cw attach
```

See the [codewire-dev skill](.claude/skills/codewire-dev.md) for complete Coder integration patterns.

### Development Workflow

```bash
# 1. Run existing tests
make test

# 2. Make your changes
# - CLI: src/main.rs
# - Client: src/client.rs
# - Protocol: src/protocol.rs
# - Daemon: src/daemon.rs

# 3. Add integration test
# tests/integration.rs

# 4. Run full test suite
make test-all

# 5. Manual verification
./tests/manual_test.sh

# 6. Format and lint
cargo fmt
cargo clippy --all-targets --all-features
```

### Quick Reference

```bash
# Full test suite
make test-all

# Build release
make build

# Install locally
make install

# Clean everything
make clean
```

## Development

```bash
# Build
cargo build

# Run tests (unit + integration)
cargo test

# Run with logging
RUST_LOG=codewire=debug cw daemon
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
