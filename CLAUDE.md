# Claude Code Context

## Project Overview

Codewire is a persistent process server for AI coding agents. Single Go binary (`cw`) acts as both daemon and CLI client. Manages PTY sessions that survive disconnects — launch AI agents, detach, reconnect later.

## Tech Stack

Go 1.23+, cobra, creack/pty, nhooyr.io/websocket, nats.go, BurntSushi/toml, golang.org/x/term

## Project Structure

```
cmd/cw/main.go              # CLI entry (cobra)
internal/
  auth/auth.go              # Token generation/validation
  config/config.go          # TOML config + env overrides
  protocol/
    protocol.go             # Frame wire format [type:u8][len:u32 BE][payload]
    messages.go             # Request/Response JSON (PascalCase type discriminator)
    fleet_messages.go       # Fleet protocol types
  connection/
    connection.go           # FrameReader/FrameWriter interfaces
    unix.go                 # Unix socket transport
    websocket.go            # WebSocket transport (Text=Control, Binary=Data)
  session/session.go        # SessionManager, Broadcaster, StatusWatcher, PTY lifecycle
  node/
    node.go                 # Daemon: Unix listener, WS server, PID file, signals
    handler.go              # Client dispatch, attach/watch/logs handlers
  client/
    client.go               # Target (local/remote), Connect, requestResponse
    commands.go             # All CLI command implementations
  terminal/
    rawmode.go              # RawModeGuard (golang.org/x/term)
    size.go                 # Terminal size, SIGWINCH
    detach.go               # DetachDetector state machine (Ctrl+B d)
  statusbar/statusbar.go    # Status bar rendering
  fleet/
    fleet.go                # NATS subscriptions, heartbeat (30s)
    client.go               # Fleet CLI: discover, launch, kill, send, attach
  mcp/server.go             # MCP JSON-RPC over stdio (7 tools)
tests/
  integration_test.go       # 20 E2E tests
  fleet_test.go             # 4 fleet unit tests
```

## Development

```bash
make build          # Build ./cw binary
make test           # Unit tests (internal packages)
make lint           # go vet
make test-manual    # CLI smoke test
make install        # Build + install to /usr/local/bin

# All tests including integration
go test ./internal/... ./tests/... -timeout 120s -count=1
```

## CI/CD

- **Gitea** (`.gitea/workflows/ci.yaml`) — builds and deploys the docs website only. Triggers on push to main and version tags.
- **GitHub** (`.github/workflows/ci.yml`) — runs `go test`, `go vet`, and `go build` on ubuntu + macOS. Triggers on push to main and PRs.
- **GitHub Release** (`.github/workflows/release.yml`) — triggered by `v*` tags. Cross-compiles for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 with `CGO_ENABLED=0`. Creates GitHub release with GPG-signed checksums and updates the Homebrew formula.

### Release Process

```bash
git tag v0.X.Y
git push origin v0.X.Y
```

GitHub Actions builds binaries, creates the release, and updates `Formula/codewire.rb`.

## Key Architecture

- **Wire protocol**: `[type:u8][length:u32 BE][payload]` — type 0x00 = Control (JSON), 0x01 = Data (raw bytes)
- **JSON messages**: flat struct with `"type"` discriminator in PascalCase (e.g. `{"type":"Launch","command":["bash"]}`)
- **Socket**: `~/.codewire/codewire.sock` (Unix domain socket)
- **Session lifecycle**: 3 goroutines per session (PTY reader, input writer, process waiter)
- **Broadcaster**: fan-out to multiple attached clients, non-blocking send with drop for slow consumers
- **Fleet**: NATS for control plane (discovery, commands), WebSocket for data plane (PTY attach)
