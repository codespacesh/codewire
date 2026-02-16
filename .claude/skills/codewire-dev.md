---
name: codewire-dev
description: Development workflow for codewire - persistent process server for AI coding agents (Go)
---

# Codewire Development Skill

Use this skill when working on the codewire codebase — implementing features, fixing bugs, or adding tests.

## Project Structure

```
codewire/
├── cmd/cw/main.go              # CLI entry (cobra)
├── internal/
│   ├── auth/auth.go            # Token generation/validation
│   ├── config/config.go        # TOML config + env overrides
│   ├── protocol/
│   │   ├── protocol.go         # Frame wire format
│   │   ├── messages.go         # Request/Response JSON types
│   │   └── fleet_messages.go   # Fleet protocol types
│   ├── connection/
│   │   ├── connection.go       # FrameReader/FrameWriter interfaces
│   │   ├── unix.go             # Unix socket transport
│   │   └── websocket.go        # WebSocket transport
│   ├── session/session.go      # SessionManager, PTY lifecycle
│   ├── node/
│   │   ├── node.go             # Daemon: listeners, PID file, signals
│   │   └── handler.go          # Client dispatch, attach/watch/logs
│   ├── client/
│   │   ├── client.go           # Target, Connect, requestResponse
│   │   └── commands.go         # All CLI command implementations
│   ├── terminal/
│   │   ├── rawmode.go          # RawModeGuard (golang.org/x/term)
│   │   ├── size.go             # Terminal size, SIGWINCH
│   │   └── detach.go           # DetachDetector state machine
│   ├── statusbar/statusbar.go  # Status bar rendering
│   ├── fleet/
│   │   ├── fleet.go            # NATS subscriptions, heartbeat
│   │   └── client.go           # Fleet CLI commands
│   └── mcp/server.go           # MCP JSON-RPC over stdio
├── tests/
│   ├── integration_test.go     # E2E tests (20 tests)
│   └── fleet_test.go           # Fleet unit tests (4 tests)
├── go.mod
├── Makefile
└── Dockerfile
```

## Development Workflow

### Build and test

```bash
make build                                        # Build binary
make test                                         # Unit tests
go test ./internal/... ./tests/... -timeout 120s  # All tests
make lint                                         # go vet
make test-manual                                  # Manual CLI smoke test
```

### Before making changes

```bash
# Run tests to confirm green baseline
make test

# Check for running sessions
cw list

# Stop daemon if needed
cw stop
```

### Adding a new command

1. **Update CLI** (`cmd/cw/main.go`):
```go
newCmd := &cobra.Command{
    Use:   "newcmd <id>",
    Short: "Description",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        target := resolveTarget(serverFlag, tokenFlag, dir)
        return client.NewCommand(target, args[0])
    },
}
rootCmd.AddCommand(newCmd)
```

2. **Update protocol** (`internal/protocol/messages.go`):
```go
// Add Type constant
// Request.Type = "NewCommand"

// Add fields with `json:"field_name,omitempty"` tags
// Response.Type = "NewCommandResult"
```

3. **Implement client** (`internal/client/commands.go`):
```go
func NewCommand(target *Target, id string) error {
    resp, err := requestResponse(target, &protocol.Request{
        Type: "NewCommand",
        ID:   parseID(id),
    })
    // Handle response
}
```

4. **Add node handler** (`internal/node/handler.go`):
```go
case "NewCommand":
    // Implementation
    writer.SendResponse(&protocol.Response{Type: "NewCommandResult", ...})
```

5. **Add integration test** (`tests/integration_test.go`):
```go
func TestNewCommand(t *testing.T) {
    dir := tempDir(t)
    target := startTestNode(t, dir)

    resp := requestResponse(t, target, &protocol.Request{
        Type: "NewCommand",
        // ...
    })
    // Assertions
}
```

### Test naming

Unix socket paths have ~108 byte limits on macOS. The test helper uses `t.TempDir()` which handles this, but keep test names reasonable.

### Wire protocol

```
Frame = [type: u8][length: u32 BE][payload]
  type: 0x00 = Control (JSON), 0x01 = Data (raw bytes)
```

JSON messages use PascalCase `type` discriminator:
```json
{"type":"Launch","command":["bash"],"working_dir":"/tmp"}
{"type":"SessionList","sessions":[...]}
```

### Key files for common changes

| Change | Files |
|--------|-------|
| New CLI command | `cmd/cw/main.go` + `internal/client/commands.go` |
| Protocol change | `internal/protocol/messages.go` + `internal/node/handler.go` |
| Session lifecycle | `internal/session/session.go` |
| Terminal handling | `internal/terminal/` |
| Fleet/NATS | `internal/fleet/` + `internal/protocol/fleet_messages.go` |
| MCP tools | `internal/mcp/server.go` |

## Quick reference

```bash
make build          # Build ./cw binary
make test           # Unit tests
make test-manual    # CLI smoke test
make lint           # go vet
make clean          # Remove binary
make install        # Build + install to /usr/local/bin
```
