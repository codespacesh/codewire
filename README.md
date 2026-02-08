# Codewire

Persistent process server for Claude Code sessions. Like tmux, but purpose-built for LLM processes — with native terminal scrolling, copy/paste, and no weird key chords to remember.

Codewire runs as a daemon inside your development environment (e.g., a Coder workspace). You launch Claude Code sessions with a prompt, and they keep running even if you disconnect. Reconnect anytime to pick up where you left off.

## Install

```bash
cargo install --path .
```

## Quick Start

```bash
# Launch a Claude Code session (daemon auto-starts)
codewire launch "fix the auth bug in login.ts"

# List running sessions
codewire list

# Attach to a session
codewire attach 1

# Detach: Ctrl+B then d

# View what Claude did while you were away
codewire logs 1
```

## Commands

### `codewire launch <prompt>`

Start a new Claude Code session with the given prompt.

```bash
codewire launch "refactor the database layer"
codewire launch "add unit tests for auth" --dir /home/coder/project
```

### `codewire list`

Show all sessions with their status, age, and prompt.

```bash
codewire list
codewire list --json   # machine-readable output
```

### `codewire attach <id>`

Take over your terminal and connect to a running session. You get full terminal I/O — native scrolling, native copy/paste, everything your terminal emulator supports.

Detach with **Ctrl+B d** (press Ctrl+B, release, then press d). The session keeps running.

### `codewire logs <id>`

View captured output from a session without attaching.

```bash
codewire logs 1              # full output
codewire logs 1 --follow     # tail -f style, streams new output
codewire logs 1 --tail 100   # last 100 lines
```

Works on completed sessions too — review what Claude did after it finished.

### `codewire kill <id>`

Terminate a session.

```bash
codewire kill 3
codewire kill --all
```

### `codewire daemon`

Start the daemon manually. Usually you don't need this — the daemon auto-starts on first CLI invocation.

## How It Works

Codewire is a single binary that acts as both daemon and CLI client.

**Daemon** (`codewire daemon`): Listens on a Unix socket at `~/.codewire/server.sock`. Manages PTY sessions — each Claude Code process runs in its own pseudoterminal. The daemon owns the master side of each PTY and keeps processes alive regardless of client connections.

**Client** (`codewire launch`, `attach`, etc.): Connects to the daemon's Unix socket. When you attach, the client puts your terminal in raw mode and bridges your stdin/stdout directly to the PTY. Your terminal emulator handles all rendering — that's why scrolling and copy/paste work natively.

**Logs**: All PTY output is teed to `~/.codewire/sessions/<id>/output.log` so you can review what happened while disconnected.

### Wire Protocol

Communication between client and daemon uses a simple frame-based binary protocol over the Unix socket:

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
RUST_LOG=codewire=debug codewire daemon
```

## License

MIT
