---
name: codewire-dev
description: Development workflow for codewire - persistent process server for AI coding agents
install: curl -sSL https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/codewire-dev.md > ~/.claude/skills/codewire-dev.md
---

# Codewire Development Skill

Use this skill when working on the codewire codebase - implementing features, fixing bugs, or adding tests.

## Project Structure

```
codewire/
├── src/
│   ├── main.rs       # CLI definition (clap)
│   ├── client.rs     # Client commands (launch, attach, list, etc.)
│   ├── daemon.rs     # Daemon server (session management)
│   ├── protocol.rs   # Wire protocol (frames, requests, responses)
│   ├── session.rs    # Session state machine
│   └── terminal.rs   # PTY and raw mode handling
├── tests/
│   ├── integration.rs     # End-to-end tests
│   └── manual_test.sh     # CLI integration test
└── Formula/
    └── codewire.rb   # Homebrew formula
```

## Development Workflow

### 1. Before Making Changes

**Always run tests first:**
```bash
cargo test
```

**Verify current state:**
```bash
# Check for running sessions
cw list

# Stop daemon if needed
cw stop
```

### 2. Making Changes

**Key principles:**
- CLI changes go in `main.rs`
- Client logic goes in `client.rs`
- Protocol changes require updating `protocol.rs` Request/Response enums
- Always add integration tests for new features
- Unix-only (no Windows support for Unix domain sockets)

**Testing approach:**
```bash
# Build and test
cargo build --release
cargo test

# Run manual CLI test
make test-manual
# or
./tests/manual_test.sh ./target/release/cw
```

### 3. Adding New Commands

**Example: Adding a new command**

1. **Update CLI** (`src/main.rs`):
```rust
Commands::NewCommand { arg1, arg2 } => {
    ensure_daemon(&dir).await?;
    client::new_command(&dir, arg1, arg2).await
}
```

2. **Implement client** (`src/client.rs`):
```rust
pub async fn new_command(data_dir: &Path, arg1: Type1, arg2: Type2) -> Result<()> {
    let resp = request_response(data_dir, &Request::NewCommand { arg1, arg2 }).await?;
    // Handle response
}
```

3. **Update protocol** (`src/protocol.rs`):
```rust
// Add to Request enum
NewCommand { arg1: Type1, arg2: Type2 },

// Add to Response enum
NewCommandResult { data: String },
```

4. **Add daemon handler** (`src/daemon.rs`):
```rust
Request::NewCommand { arg1, arg2 } => {
    // Implementation
    send_response(&mut writer, &Response::NewCommandResult { data }).await?;
}
```

5. **Add integration test** (`tests/integration.rs`):
```rust
#[tokio::test]
async fn test_new_command() {
    let dir = temp_dir("new-cmd"); // Keep name short (<12 chars)
    let sock = start_test_daemon(&dir).await;

    let resp = request_response(&sock, &Request::NewCommand { ... }).await;
    // Assertions
}
```

### 4. Test Naming Convention

**CRITICAL:** Unix domain socket paths have length limits (~108 bytes on macOS).

```rust
// ❌ BAD - path too long
let dir = temp_dir("auto-attach-skip-completed");

// ✅ GOOD - short name
let dir = temp_dir("auto-skip-done");
```

**Rule:** Test directory names must be ≤12 characters.

### 5. Common Testing Patterns

```rust
// Launch a session
let resp = request_response(
    &sock,
    &Request::Launch {
        command: vec!["bash".into(), "-c".into(), "sleep 10".into()],
        working_dir: "/tmp".to_string(),
    },
).await;

// Attach to session (keep connection alive)
let stream = UnixStream::connect(&sock).await.unwrap();
let (mut reader, mut writer) = stream.into_split();
send_request(&mut writer, &Request::Attach { id }).await.unwrap();
let _ = read_frame(&mut reader).await.unwrap();

// Detach
send_request(&mut writer, &Request::Detach).await.unwrap();
let _ = read_frame(&mut reader).await.unwrap();

// Clean up
request_response(&sock, &Request::Kill { id }).await;
```

### 6. Manual Testing

```bash
# Launch sessions
cw launch -- sleep 100
cw launch -- bash -c "echo hello; sleep 100"

# List sessions
cw list

# Attach to session
cw attach 1       # Explicit ID
cw attach         # Auto-attach (oldest unattached)

# View logs
cw logs 1
cw logs 1 --follow

# Send input without attaching
cw send 1 "ls -la\n"

# Watch session output
cw watch 1

# Kill sessions
cw kill 1
cw kill --all
```

### 7. Release Process

**Update version:**
1. `Cargo.toml` - version field
2. `Formula/codewire.rb` - version and sha256

**Build and test:**
```bash
make test-all
cargo build --release
```

**Tag and push:**
```bash
git tag v0.2.0
git push origin v0.2.0
```

GitHub Actions will build binaries for macOS, Linux, and Windows.

## Architecture Notes

### Session State Machine
```
launch → running → (attached/detached) → completed/killed
```

### Wire Protocol
```
Frame = [type: u8][length: u32 BE][payload]
  type: 0x00 = Control (JSON), 0x01 = Data (raw bytes)
```

### Multi-Attach Support
Multiple clients can attach to the same session simultaneously. Output is broadcast to all attached clients.

### Persistence
- Sessions persisted to `~/.codewire/sessions.json`
- Output buffered in `~/.codewire/sessions/{id}/output`
- Event-driven persistence (debounced 500ms)

## Common Pitfalls

1. **Socket path length** - Keep test names short
2. **Timing in tests** - Add `tokio::time::sleep()` after state changes
3. **Daemon lifecycle** - Stop daemon between manual test runs
4. **Windows** - Project is Unix-only (no Windows support)
5. **PTY encoding** - Always use UTF-8, test with non-ASCII chars

## Quick Commands

```bash
# Full test suite
make test-all

# Build only
make build

# Install locally
make install

# Clean everything
make clean

# Format code
cargo fmt

# Check lints
cargo clippy --all-targets --all-features
```

## Integration with Coder CLI

Codewire pairs perfectly with [Coder](https://coder.com) for remote development environments. Use `coder` CLI to manage workspaces and `cw` to manage persistent sessions within them.

### Basic Workflow

```bash
# SSH into Coder workspace
coder ssh myworkspace

# Inside workspace: launch long-running processes with cw
cw launch -- cargo build --release
cw launch -- npm run dev
cw launch -- python train_model.py

# List all running sessions
cw list

# Detach from SSH (sessions keep running)
exit

# Later: SSH back in
coder ssh myworkspace

# Attach to ongoing build
cw attach 1

# Or auto-attach to oldest session
cw attach
```

### Remote Session Monitoring

```bash
# From local machine: watch remote session without SSH
coder ssh myworkspace -- cw watch 1

# Follow logs from remote session
coder ssh myworkspace -- cw logs 1 --follow

# Check status of all remote sessions
coder ssh myworkspace -- cw list --json | jq
```

### Development Patterns

**Pattern 1: Background Build Server**
```bash
# On remote: launch build server
cw launch -- cargo watch -x 'build --release'

# Detach and work locally
exit

# Later: check build status
coder ssh myworkspace -- cw logs 1 --tail 50
```

**Pattern 2: Multiple Workstreams**
```bash
# Launch parallel tasks on remote
coder ssh myworkspace << 'EOF'
cw launch -- cargo test --all-features
cw launch -- cargo clippy --all-targets
cw launch -- cargo doc --no-deps
cw list
EOF

# Monitor all from local
watch -n 5 'coder ssh myworkspace -- cw list'
```

**Pattern 3: Long-Running Training**
```bash
# Launch ML training job
coder ssh myworkspace -- cw launch -- python train.py --epochs 100

# Check progress remotely
coder ssh myworkspace -- cw watch 1 --timeout 10

# Send commands to running job (if interactive)
echo "save checkpoint" | coder ssh myworkspace -- cw send 1 --stdin
```

**Pattern 4: Jupyter in Background**
```bash
# Launch Jupyter on remote
coder ssh myworkspace -- cw launch -- jupyter lab --no-browser --port 8888

# Forward port through Coder
coder port-forward myworkspace --tcp 8888:8888

# Access at localhost:8888
# Session persists even if you disconnect
```

### Coder + CW Commands Cheatsheet

```bash
# Quick status check
alias cw-remote='coder ssh myworkspace -- cw'
cw-remote list
cw-remote status 1

# Launch on remote from local
coder ssh myworkspace -- cw launch -- htop

# Attach interactively to remote session
coder ssh myworkspace -t -- cw attach 1

# Kill all remote sessions (cleanup)
coder ssh myworkspace -- cw kill --all

# Get remote session logs as JSON
coder ssh myworkspace -- cw list --json > remote-sessions.json
```

### Coder Workspace Lifecycle

```bash
# Create workspace with codewire pre-installed
cat > workspace.yaml << 'EOF'
name: dev
template: docker
startup_script: |
  # Install codewire
  curl -sSL https://github.com/sonica/codewire/releases/latest/download/cw-$(uname -s)-$(uname -m) -o /usr/local/bin/cw
  chmod +x /usr/local/bin/cw

  # Auto-launch services
  cw launch -- redis-server
  cw launch -- postgres
EOF

coder create --from workspace.yaml

# Stop workspace (sessions persist in ~/.codewire)
coder stop myworkspace

# Start workspace (sessions auto-resume on next cw command)
coder start myworkspace
```

### Why Coder + Codewire?

| Feature | Coder Alone | Coder + Codewire |
|---------|-------------|------------------|
| SSH disconnect | Terminates processes | Processes persist |
| Multiple tasks | Need tmux/screen | Native multi-session |
| Monitor remotely | Requires extra setup | Built-in logs/watch |
| Session management | Manual process tracking | Automatic with IDs |
| Reattach | Complex tmux commands | `cw attach` |

### Testing with Coder

```bash
# Run integration tests on remote workspace
coder ssh buildbox -- "cd ~/codewire && make test-all"

# Test on multiple architectures
for ws in linux-amd64 linux-arm64 macos-m1; do
  echo "Testing on $ws..."
  coder ssh $ws -- "cd ~/codewire && cargo test" &
done
wait
```

### Pro Tips

1. **Add to shell rc:**
```bash
# In remote ~/.bashrc or ~/.zshrc
alias cwl='cw list'
alias cwa='cw attach'
alias cww='cw watch'

# Auto-show sessions on login
cw list 2>/dev/null || true
```

2. **Use cw for CI/CD in Coder:**
```bash
# In Coder workspace CI script
cw launch -- cargo build --release
cw launch -- cargo test --all-features
cw launch -- cargo clippy

# Wait for all to complete
while cw list | grep -q "running"; do sleep 5; done

# Check results
cw logs 1
cw logs 2
cw logs 3
```

3. **Supervisor pattern:**
```bash
# Launch supervisor that monitors other sessions
cw launch -- bash -c 'while true; do cw list; sleep 10; done'
```

## When to Use This Skill

- ✅ Implementing new commands
- ✅ Adding features to existing commands
- ✅ Fixing bugs in session management
- ✅ Adding integration tests
- ✅ Updating protocol
- ✅ Working with PTY/terminal handling
- ❌ General Rust questions (use base knowledge)
- ❌ Non-codewire projects

## Remember

1. **Test first** - Run existing tests before changing code
2. **Add tests** - Every feature needs an integration test
3. **Short names** - Test directory names ≤12 characters
4. **Unix only** - No Windows support needed
5. **Manual test** - Run `make test-manual` before committing

---

**Shortcode install:**
```bash
# Install this skill
curl -sSL https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/codewire-dev.md > ~/.claude/skills/codewire-dev.md

# Or copy directly to skills directory
cp .claude/skills/codewire-dev.md ~/.claude/skills/
```
