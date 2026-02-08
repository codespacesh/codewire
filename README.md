# Codewire

Persistent process server for AI coding agents. Like tmux, but purpose-built for long-running LLM processes — with native terminal scrolling, copy/paste, and no weird key chords.

Codewire runs as a daemon inside your development environment (e.g., a Coder workspace). You launch AI agent sessions with a prompt, and they keep running even if you disconnect. Reconnect anytime to pick up where you left off.

Works with any CLI-based AI agent: Claude Code, Aider, Goose, Codex, or anything else.

## Install

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

### `cw daemon`

Start the daemon manually. Usually you don't need this — the daemon auto-starts on first CLI invocation.

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
