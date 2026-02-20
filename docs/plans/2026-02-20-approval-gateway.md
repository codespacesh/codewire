# Approval Gateway Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add `cw gateway` — a command that lets workers (Claude Code sessions) send approval
requests upward to a supervisor LLM or human, blocking until a reply arrives.

**Architecture:**
- Workers call `cw request gateway "approve: <action>"` from inside their cw session.
  This blocks until the gateway replies.
- `cw gateway --exec <cmd>` subscribes to `message.request` events on a stub session named
  "gateway", pipes each request body through `--exec`, and auto-replies with the output.
- For human notification, `--notify macos` pops a macOS dialog.
- A Claude Code `PreToolUse` hook can intercept tool calls automatically without the worker
  needing to call `cw request` explicitly.

**Tech Stack:** Go 1.23+, existing cw protocol, shell/bash for the hook script

---

### Task 1: CW_SESSION_ID injection + fix anonymous SendRequest

**Goal:** Workers running inside cw sessions need to know their own session ID so
`cw request gateway` can identify itself. Also: allow CLI callers with no session ID to
send requests (fix the `fromID=0` crash in SendRequest).

**Files:**
- Modify: `internal/session/session.go` (Launch function ~line 584, SendRequest ~line 431)
- Modify: `cmd/cw/main.go` (requestCmd, msgCmd, replyCmd — auto-detect CW_SESSION_ID)

**Step 1: Write the failing test**

Add to `tests/integration_test.go`:

```go
func TestCWSessionIDEnv(t *testing.T) {
    t.Parallel()
    sm := startNode(t)
    defer sm.Stop()

    // Launch a session that echoes CW_SESSION_ID
    id := launchSession(t, sm, []string{"sh", "-c", "echo CW_SESSION_ID=$CW_SESSION_ID"})
    time.Sleep(200 * time.Millisecond)

    logs := sessionLogs(t, sm, id)
    if !strings.Contains(logs, fmt.Sprintf("CW_SESSION_ID=%d", id)) {
        t.Fatalf("expected CW_SESSION_ID=%d in output, got: %s", id, logs)
    }
}

func TestAnonymousSendRequest(t *testing.T) {
    t.Parallel()
    sm := startNode(t)
    defer sm.Stop()

    gatewayID := launchSession(t, sm, []string{"sleep", "10"})
    // fromID=0 should not crash; gateway receives the request
    requestID, replyCh, err := sm.Sessions.SendRequest(0, gatewayID, "hello from cli")
    if err != nil {
        t.Fatalf("anonymous SendRequest failed: %v", err)
    }
    // Clean up
    sm.Sessions.CleanupRequest(requestID)
    _ = replyCh
}
```

**Step 2: Run to verify failure**
```
go test ./tests/... -run TestCWSessionIDEnv -v -timeout 30s
go test ./internal/session/... -run TestAnonymousSendRequest -v -timeout 30s
```
Expected: FAIL

**Step 3: Inject CW_SESSION_ID into PTY sessions**

In `internal/session/session.go`, `Launch` function, change line ~584:
```go
// Before:
cmd.Env = buildEnv(env)

// After:
cmd.Env = buildEnv(append(env, fmt.Sprintf("CW_SESSION_ID=%d", id)))
```

**Step 4: Fix SendRequest to allow anonymous fromID=0**

In `internal/session/session.go`, replace the entire body of `SendRequest` from the
`m.mu.RLock()` line through the `return requestID, replyCh, nil` (keep signature unchanged).
The full replacement (lines ~426–477):

```go
m.mu.RLock()
fromSess, fromOK := m.sessions[fromID]
toSess, toOK := m.sessions[toID]
m.mu.RUnlock()

// fromID=0 is allowed (anonymous caller, e.g. CLI or gateway hook).
if !fromOK && fromID != 0 {
    return "", nil, fmt.Errorf("sender session %d not found", fromID)
}
if !toOK {
    return "", nil, fmt.Errorf("recipient session %d not found", toID)
}

requestID := fmt.Sprintf("req_%d_%d_%d", fromID, toID, time.Now().UnixNano())

var fromName string
if fromOK {
    fromSess.mu.Lock()
    fromName = fromSess.Meta.Name
    fromSess.mu.Unlock()
}

toSess.mu.Lock()
toName := toSess.Meta.Name
toSess.mu.Unlock()

reqData := RequestData{
    RequestID: requestID,
    From:      fromID,
    FromName:  fromName,
    To:        toID,
    ToName:    toName,
    Body:      body,
}
event := NewRequestEvent(reqData)

// Write to recipient's message log and publish.
if toSess.messageLog != nil {
    toSess.messageLog.Append(event)
}
m.Subscriptions.Publish(toID, toSess.Meta.Tags, event)
// Also publish on sender (if sender is a real session).
if fromOK && fromID != toID {
    if fromSess.messageLog != nil {
        fromSess.messageLog.Append(event)
    }
    m.Subscriptions.Publish(fromID, fromSess.Meta.Tags, event)
}

// Register reply channel.
replyCh := make(chan ReplyData, 1)
m.pendingRequestsMu.Lock()
m.pendingRequests[requestID] = replyCh
m.pendingRequestsMu.Unlock()

return requestID, replyCh, nil
```

**Step 5: Auto-detect CW_SESSION_ID in requestCmd, msgCmd, replyCmd**

In `cmd/cw/main.go`, for each of the three commands, before the `from != ""` check:
```go
// In requestCmd, msgCmd, replyCmd — add this at the start of the from-resolution block:
if from == "" {
    if envID := os.Getenv("CW_SESSION_ID"); envID != "" {
        from = envID
    }
}
```

This means code running inside a cw session automatically identifies itself.

**Step 6: Run tests to verify pass**
```
go test ./tests/... -run TestCWSessionIDEnv -v -timeout 30s
go test ./internal/session/... -run TestAnonymousSendRequest -v -timeout 30s
```

**Step 7: Commit**
```bash
git add internal/session/session.go cmd/cw/main.go tests/integration_test.go
git commit -m "feat: inject CW_SESSION_ID into sessions, allow anonymous request senders"
```

---

### Task 2: cw request --raw + cw gateway command

**Goal:** `cw request --raw` prints just the reply body (for scripting). `cw gateway` creates
a stub session, subscribes to message.request events, and auto-replies via an exec command.

**Files:**
- Modify: `internal/client/commands.go` — update `Request`, add `Gateway`
- Modify: `cmd/cw/main.go` — add `--raw` to requestCmd, add `gatewayCmd()`

**Step 1: Write the failing test**

Add to `tests/integration_test.go`:

```go
func TestGatewayAutoReply(t *testing.T) {
    t.Parallel()
    sm := startNode(t)
    defer sm.Stop()

    // Create a gateway stub session
    gatewayID := launchNamedSession(t, sm, "gateway", []string{"sleep", "30"})

    // Subscribe to message.request on the gateway session
    sub := sm.Sessions.Subscriptions.Subscribe(&gatewayID, nil,
        []session.EventType{session.EventRequest})
    defer sm.Sessions.Subscriptions.Unsubscribe(sub.ID)

    // Worker sends request to gateway (anonymous fromID=0)
    workerID := launchSession(t, sm, []string{"sleep", "5"})
    requestID, replyCh, err := sm.Sessions.SendRequest(workerID, gatewayID, "approve: git push")
    if err != nil {
        t.Fatalf("SendRequest failed: %v", err)
    }

    // Simulate gateway receiving and replying
    select {
    case evt := <-sub.Ch:
        var rd session.RequestData
        if err := json.Unmarshal(evt.Event.Data, &rd); err != nil {
            t.Fatalf("unmarshal RequestData: %v", err)
        }
        if rd.Body != "approve: git push" {
            t.Fatalf("unexpected body: %s", rd.Body)
        }
        if err := sm.Sessions.SendReply(gatewayID, rd.RequestID, "APPROVED"); err != nil {
            t.Fatalf("SendReply: %v", err)
        }
    case <-time.After(3 * time.Second):
        t.Fatal("timeout waiting for request event")
    }

    // Verify worker got the reply
    select {
    case reply := <-replyCh:
        if reply.Body != "APPROVED" {
            t.Fatalf("expected APPROVED, got %q", reply.Body)
        }
    case <-time.After(3 * time.Second):
        sm.Sessions.CleanupRequest(requestID)
        t.Fatal("timeout waiting for reply")
    }
}
```

**Step 2: Run to verify failure**
```
go test ./tests/... -run TestGatewayAutoReply -v -timeout 30s
```
Expected: FAIL (launchNamedSession not yet implemented)

**Step 3: Add helper launchNamedSession to test helpers (or inline)**

In `tests/integration_test.go`, add or confirm the helper exists:
```go
func launchNamedSession(t *testing.T, sm *testNode, name string, cmd []string) uint32 {
    t.Helper()
    id := launchSession(t, sm, cmd)
    if err := sm.Sessions.SetName(id, name); err != nil {
        t.Fatalf("SetName: %v", err)
    }
    return id
}
```

**Step 4: Add --raw to Request function**

In `internal/client/commands.go`, update `Request` signature:
```go
func Request(target *Target, fromID *uint32, toID uint32, body string, timeout uint64, rawOutput bool) error {
```
And change the print:
```go
case "MsgRequestResult":
    if rawOutput {
        fmt.Println(resp.ReplyBody)
    } else {
        fromLabel := "unknown"
        if resp.FromName != "" {
            fromLabel = resp.FromName
        } else if resp.FromID != nil {
            fromLabel = fmt.Sprintf("%d", *resp.FromID)
        }
        fmt.Printf("[reply from %s] %s\n", fromLabel, resp.ReplyBody)
    }
```

**Step 5: Add Gateway function**

Add to `internal/client/commands.go`:

```go
// Gateway starts an approval gateway. It creates a stub session named `name`
// (running "sleep infinity"), subscribes to message.request events on that
// session, evaluates each request by piping body to execCmd, and replies with
// the command's stdout. If notifyMethod is "macos", it also sends a macOS
// notification for ESCALATE replies.
func Gateway(target *Target, name, execCmd, notifyMethod string) error {
    // 1. Launch stub session.
    resp, err := requestResponse(target, &protocol.Request{
        Type:    "Launch",
        Command: []string{"sleep", "infinity"},
        Tags:    []string{"_gateway"},
        Name:    name,
    })
    if err != nil {
        return fmt.Errorf("launching gateway session: %w", err)
    }
    if resp.Type == "Error" {
        return fmt.Errorf("%s", resp.Message)
    }
    stubID := resp.ID
    fmt.Fprintf(os.Stderr, "[cw gateway] listening as %q (session %d)\n", name, stubID)

    // 2. Cleanup on exit.
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    go func() {
        select {
        case <-sigCh:
            cancel()
        case <-ctx.Done():
        }
    }()

    defer func() {
        _ = Kill(target, &stubID, nil)
        fmt.Fprintf(os.Stderr, "[cw gateway] stopped\n")
    }()

    // 3. Subscribe to message.request on the stub session.
    reader, writer, err := target.Connect()
    if err != nil {
        return err
    }
    defer reader.Close()
    defer writer.Close()

    subReq := &protocol.Request{
        Type:       "Subscribe",
        ID:         &stubID,
        EventTypes: []string{"message.request"},
    }
    if err := writer.SendRequest(subReq); err != nil {
        return fmt.Errorf("subscribing: %w", err)
    }

    // 4. Event loop.
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }

        frame, err := reader.ReadFrame()
        if err != nil {
            if ctx.Err() != nil {
                return nil
            }
            return err
        }
        if frame == nil {
            return nil
        }
        if frame.Type != protocol.FrameControl {
            continue
        }

        var resp protocol.Response
        if err := json.Unmarshal(frame.Payload, &resp); err != nil {
            continue
        }

        if resp.Type != "Event" || resp.Event == nil {
            continue
        }
        if resp.Event.EventType != "message.request" {
            continue
        }

        var reqData struct {
            RequestID string `json:"request_id"`
            From      uint32 `json:"from"`
            FromName  string `json:"from_name"`
            Body      string `json:"body"`
        }
        if err := json.Unmarshal(resp.Event.Data, &reqData); err != nil {
            continue
        }

        // Evaluate in a goroutine so one slow request doesn't block others.
        go func(requestID, body, fromName string) {
            reply := gatewayEvaluate(execCmd, body, fromName)
            upperReply := strings.ToUpper(reply)

            // Notify human on ESCALATE.
            if strings.HasPrefix(upperReply, "ESCALATE") && notifyMethod != "" {
                gatewayNotify(notifyMethod, body, fromName)
            }

            // Send the reply.
            if _, err := requestResponse(target, &protocol.Request{
                Type:      "MsgReply",
                RequestID: requestID,
                Body:      reply,
            }); err != nil {
                fmt.Fprintf(os.Stderr, "[cw gateway] reply error: %v\n", err)
            } else {
                fmt.Fprintf(os.Stderr, "[cw gateway] %s → %s\n", fromName, reply)
            }
        }(reqData.RequestID, reqData.Body, reqData.FromName)
    }
}

func gatewayEvaluate(execCmd, body, fromName string) string {
    if execCmd == "" {
        return "APPROVED"
    }
    cmd := exec.Command("sh", "-c", execCmd)
    cmd.Stdin = strings.NewReader(body)
    cmd.Env = append(os.Environ(),
        "CW_REQUEST_BODY="+body,
        "CW_REQUEST_FROM="+fromName,
    )
    out, err := cmd.Output()
    if err != nil {
        return fmt.Sprintf("DENIED: exec error: %v", err)
    }
    reply := strings.TrimSpace(string(out))
    if reply == "" {
        return "APPROVED"
    }
    return reply
}

func gatewayNotify(method, body, fromName string) {
    switch {
    case method == "macos":
        msg := fmt.Sprintf("Approval needed from %s: %s", fromName, body)
        _ = exec.Command("osascript", "-e",
            fmt.Sprintf(`display notification %q with title "cw gateway"`, msg)).Run()
    case strings.HasPrefix(method, "ntfy:"):
        url := strings.TrimPrefix(method, "ntfy:")
        _ = exec.Command("curl", "-s", "-d", body, url).Run()
    }
}
```

**Step 6: Update requestCmd to pass rawOutput, add gatewayCmd**

In `cmd/cw/main.go`:

For `requestCmd`, add flag and pass to `client.Request`:
```go
var raw bool
cmd.Flags().BoolVar(&raw, "raw", false, "Print just the reply body (for scripting)")
// ...
return client.Request(target, fromID, toID, args[1], timeout, raw)
```

Add `gatewayCmd()` and register it:
```go
func gatewayCmd() *cobra.Command {
    var name, execCmd, notify string

    cmd := &cobra.Command{
        Use:   "gateway",
        Short: "Run an approval gateway for worker sessions",
        Long: `Start an approval gateway. Workers call 'cw request gateway "<action>"'
and block until the gateway replies.

The gateway subscribes to requests on a stub session named NAME (default: gateway).
Each request body is piped to --exec; its stdout becomes the reply.

LLM supervisor:
  cw gateway --exec 'claude --dangerously-skip-permissions --print \
    "Approval policy: approve git/edit/read ops; deny rm -rf, DROP TABLE. \
     Request: $(cat). Reply: APPROVED or DENIED: <reason>"'

Human notification (macOS):
  cw gateway --notify macos

Combined (LLM first, notify on ESCALATE):
  cw gateway --exec '...' --notify macos`,
        RunE: func(cmd *cobra.Command, args []string) error {
            target, err := resolveTarget()
            if err != nil {
                return err
            }
            if target.IsLocal() {
                if err := ensureNode(); err != nil {
                    return err
                }
            }
            return client.Gateway(target, name, execCmd, notify)
        },
    }
    cmd.Flags().StringVar(&name, "name", "gateway", "Session name to register as")
    cmd.Flags().StringVar(&execCmd, "exec", "", "Shell command to evaluate requests (body on stdin)")
    cmd.Flags().StringVar(&notify, "notify", "", "Notification method: macos or ntfy:<url>")
    return cmd
}
```

Register in root command list (alongside existing commands).

**Step 7: Run tests**
```
go test ./tests/... -run TestGatewayAutoReply -v -timeout 30s
go test ./internal/... ./tests/... -timeout 120s -count=1
```

**Step 8: Build and smoke test**
```bash
make build
./cw gateway --help
```

**Step 9: Commit**
```bash
git add internal/client/commands.go cmd/cw/main.go tests/integration_test.go
git commit -m "feat: add cw gateway command + cw request --raw + CW_SESSION_ID env"
```

---

### Task 3: Claude Code PreToolUse hook + cw-gateway skill

**Goal:** A `PreToolUse` hook for Claude Code that intercepts Bash/Edit/Write tool calls and
routes them through the gateway. Install script serves both the hook and a `/cw-gateway` skill.

**Files:**
- Create: `docs/hooks/pre-tool-use.sh`
- Create: `docs/skills/cw-gateway.md`
- Modify: `codewire-demo/docs/Dockerfile`
- Modify: `codewire-demo/.gitea/workflows/ci.yaml`

**Step 1: Create the hook script**

Create `docs/hooks/pre-tool-use.sh`:

```bash
#!/bin/bash
# cw PreToolUse hook — routes tool calls through the approval gateway.
# Install: see https://codewire.sh/hooks/pre-tool-use.sh

# If no gateway session is running, allow everything.
if ! cw list 2>/dev/null | grep -qE "^[0-9]+\s+gateway\s+running"; then
    exit 0
fi

# Read tool input from stdin.
INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // "unknown"')
INPUT_STR=$(echo "$INPUT" | jq -r '.tool_input | tostring')

# Skip non-destructive read-only tools.
case "$TOOL" in
    Read|Glob|Grep|WebFetch|WebSearch|TodoRead|TaskList|TaskGet)
        exit 0 ;;
esac

# Send request to gateway and get raw reply.
REPLY=$(cw request gateway "$TOOL: $INPUT_STR" --timeout 30 --raw 2>/dev/null)
EXIT=$?

if [ $EXIT -ne 0 ] || [ -z "$REPLY" ]; then
    # Gateway unreachable or timeout — allow by default.
    exit 0
fi

UPPER_REPLY=$(echo "$REPLY" | tr '[:lower:]' '[:upper:]')

if echo "$UPPER_REPLY" | grep -q "^DENIED"; then
    REASON=$(echo "$REPLY" | sed 's/^[Dd][Ee][Nn][Ii][Ee][Dd][: ]*//')
    printf '{"decision":"block","reason":"Gateway denied: %s"}' "$REASON"
    exit 2
fi

# APPROVED or ESCALATE — allow (human will handle ESCALATE notification separately).
exit 0
```

**Step 2: Create cw-gateway skill**

Create `docs/skills/cw-gateway.md`:

```markdown
Start a Codewire approval gateway that auto-approves or auto-denies tool calls
from worker sessions based on a policy.

## Steps

1. Ensure the cw node is running: `cw node -d`

2. Start the gateway with an LLM policy:
   ```bash
   cw gateway --exec 'claude --dangerously-skip-permissions --print "You are an
   approval gateway. Policy: approve git operations, file reads/writes, builds.
   Deny: rm -rf on non-build paths, DROP TABLE, mass deletions. Escalate: anything
   unclear. Request from worker: $(cat). Reply with exactly:
   APPROVED
   DENIED: <reason>
   ESCALATE: <reason>"'
   ```

3. Tell the user the gateway is running. Workers can now call:
   `cw request gateway "approve: <action>"`

4. To add automatic Claude Code interception (all Bash/Edit/Write go through gateway):
   ```bash
   mkdir -p ~/.claude/hooks
   curl -fsSL https://codewire.sh/hooks/pre-tool-use.sh -o ~/.claude/hooks/pre-tool-use.sh
   chmod +x ~/.claude/hooks/pre-tool-use.sh
   ```
   Then add to `~/.claude/settings.json`:
   ```json
   {
     "hooks": {
       "PreToolUse": [{
         "hooks": [{"type": "command", "command": "~/.claude/hooks/pre-tool-use.sh"}]
       }]
     }
   }
   ```

## Notes

- The gateway creates a stub session named "gateway" — visible in `cw list`
- `cw kill gateway` stops the gateway and the stub session
- Workers inside cw sessions automatically have CW_SESSION_ID set, so
  `cw request gateway "..."` identifies them correctly
- Without `--exec`, the gateway auto-approves everything (useful for audit logging)
```

**Step 3: Update Dockerfile**

In `codewire-demo/docs/Dockerfile`, add after the skills COPY:
```dockerfile
RUN mkdir -p /usr/share/nginx/html/hooks
COPY hooks/pre-tool-use.sh /usr/share/nginx/html/hooks/pre-tool-use.sh
```

**Step 4: Update CI**

In `codewire-demo/.gitea/workflows/ci.yaml`, add after the skills fetch:
```yaml
          mkdir -p docs/hooks
          curl -fL $BASE/hooks/pre-tool-use.sh -o docs/hooks/pre-tool-use.sh
```

**Step 5: Commit**
```bash
git add docs/hooks/ docs/skills/cw-gateway.md
git commit -m "feat: add PreToolUse hook and cw-gateway skill"

cd /Users/noel/src/sonica/codewire-demo
git add docs/Dockerfile .gitea/workflows/ci.yaml
git commit -m "feat: serve hooks/ and cw-gateway skill from docs container"
```

---

### Task 4: Docs update

**Goal:** Document the gateway pattern in `llms-full.txt` under section 8 (Claude Code
Integration). Update `llms.txt` to mention it.

**Files:**
- Modify: `docs/llms-full.txt`
- Modify: `docs/llms.txt`

**Step 1: Expand llms-full.txt section 8**

After the existing `/cw skill` content, add a new subsection:

```markdown
### Approval gateway

Workers can send approval requests to a supervisor — either an LLM or a human.

**Start a gateway (LLM supervisor):**
```bash
cw gateway --exec 'claude --dangerously-skip-permissions --print \
  "Policy: approve git, file edits, builds. Deny: rm -rf non-build paths.
   Request: $(cat). Reply: APPROVED or DENIED: <reason>"'
```

**Worker sends an approval request:**
```bash
# From inside a cw session — blocks until reply
cw request gateway "approve: rm -rf ./dist"
# Output: APPROVED  (or DENIED: <reason>)
```

**Install Claude Code hook for automatic interception:**
```bash
mkdir -p ~/.claude/hooks
curl -fsSL https://codewire.sh/hooks/pre-tool-use.sh -o ~/.claude/hooks/pre-tool-use.sh
chmod +x ~/.claude/hooks/pre-tool-use.sh
```
Add to `~/.claude/settings.json` hooks → PreToolUse.

**Worker self-identification:** Sessions automatically have `CW_SESSION_ID` in their
environment, so `cw request` from inside a session correctly identifies the sender.

**Skill:** `curl -fsSL https://codewire.sh/skills/cw-gateway.md -o ~/.claude/commands/cw-gateway.md`
```

**Step 2: Update llms.txt Optional section**

Add a line:
```
- [Approval gateway skill](https://codewire.sh/skills/cw-gateway.md): Start a supervisor LLM that auto-approves worker tool calls
```

**Step 3: Commit**
```bash
git add docs/llms-full.txt docs/llms.txt
git commit -m "docs: document approval gateway pattern"
```

---

### Task 5: Full test suite + tag release

**Step 1: Run all tests**
```
go test ./internal/... ./tests/... -timeout 120s -count=1
```
Expected: all PASS

**Step 2: Build**
```
make build
./cw gateway --help
```

**Step 3: /cpv both repos**

Use the `cpv` skill in the `codewire` repo, then the `codewire-demo` repo.
