# UX Improvements Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make Codewire's CLI feel dead simple for LLMs — smart tag resolution without `--tag`, `cw list --status`, better error messages, and expanded relay docs.

**Architecture:** All changes are in two layers: cobra command definitions (`cmd/cw/main.go`) and the client logic layer (`internal/client/commands.go`). No protocol changes needed — tag routing already exists server-side. A new `ResolveSessionOrTag` helper centralizes the smart resolution logic.

**Tech Stack:** Go 1.23+, cobra, existing node/client architecture

---

## What This Changes

### CLI behaviour (code)
1. `cw wait batch-42` — positional arg tries session ID → name → tag (no `--tag` needed)
2. `cw kill batch-42` — same smart resolution
3. `cw watch batch-42` — same, falls through to multi-session watch for tags
4. `cw subscribe batch-42` — new positional arg, smart resolution
5. `cw list --status running` — filter by status
6. `cw run` error when `--` missing: helpful hint instead of "unknown shorthand flag"
7. `cw attach --help` — long description mentions Ctrl+B d
8. `cw mcp-server --help` — long description shows `claude mcp add` one-liner
9. MCP server connection error — "No node running. Start one with: cw node -d"

### Docs (markdown)
10. `docs/quickstart.md` — replace `cw launch` with `cw run` throughout
11. `docs/llms-full.txt` — replace `cw launch` with `cw run`, expand relay section

---

## Task 1: Add `ResolveSessionOrTag` to `internal/client/commands.go`

**Files:**
- Modify: `internal/client/commands.go` (after `ResolveSessionArg`, ~line 54)
- Test: `tests/integration_test.go`

### Step 1: Write the failing test

Add to `tests/integration_test.go`:

```go
func TestResolveSessionOrTag(t *testing.T) {
	dir := tempDir(t, "resolve-tag")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch two sessions with tag "batch-99"
	r1 := requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "5"}, WorkingDir: "/tmp",
		Tags: []string{"batch-99"},
	})
	if r1.Type != "Launched" {
		t.Fatalf("launch 1: %s", r1.Message)
	}
	r2 := requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "5"}, WorkingDir: "/tmp",
		Tags: []string{"batch-99"},
	})
	if r2.Type != "Launched" {
		t.Fatalf("launch 2: %s", r2.Message)
	}
	time.Sleep(200 * time.Millisecond)

	// "batch-99" is not a session name — should resolve to tag
	id, tags, err := client.ResolveSessionOrTag(target, "batch-99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != nil {
		t.Fatalf("expected no session ID, got %d", *id)
	}
	if len(tags) != 1 || tags[0] != "batch-99" {
		t.Fatalf("expected tags=[batch-99], got %v", tags)
	}

	// A numeric ID resolves as session
	id2, tags2, err2 := client.ResolveSessionOrTag(target, fmt.Sprintf("%d", *r1.ID))
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if id2 == nil || *id2 != *r1.ID {
		t.Fatalf("expected session ID %d", *r1.ID)
	}
	if len(tags2) != 0 {
		t.Fatalf("expected no tags, got %v", tags2)
	}
}
```

### Step 2: Run the test, verify it fails

```bash
go test ./tests/... -run TestResolveSessionOrTag -v
```
Expected: FAIL — `client.ResolveSessionOrTag undefined`

### Step 3: Implement `ResolveSessionOrTag`

Add after the existing `ResolveSessionArg` function in `internal/client/commands.go` (~line 54):

```go
// ResolveSessionOrTag tries to resolve arg as a session ID/name, then as a tag.
// Returns (sessionID, tags, err). Exactly one of sessionID or tags will be non-nil/non-empty.
func ResolveSessionOrTag(target *Target, arg string) (*uint32, []string, error) {
	// Try as session ID or name first.
	id, err := ResolveSessionArg(target, arg)
	if err == nil {
		return &id, nil, nil
	}

	// Only fall back to tag for "not found" errors, not connection errors.
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "not found") && !strings.Contains(lower, "no session named") {
		return nil, nil, err
	}

	// Check if any sessions have this tag.
	resp, listErr := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if listErr != nil {
		return nil, nil, err // return original error
	}
	if resp.Sessions != nil {
		for _, s := range *resp.Sessions {
			for _, t := range s.Tags {
				if t == arg {
					return nil, []string{arg}, nil
				}
			}
		}
	}

	return nil, nil, fmt.Errorf("no session or tag named %q\n\nUse 'cw list' to see active sessions", arg)
}
```

### Step 4: Run the test, verify it passes

```bash
go test ./tests/... -run TestResolveSessionOrTag -v
```
Expected: PASS

### Step 5: Commit

```bash
git add internal/client/commands.go tests/integration_test.go
git commit -m "feat: add ResolveSessionOrTag for smart session/tag positional resolution"
```

---

## Task 2: Update `waitSessionCmd` to use smart resolution

**Files:**
- Modify: `cmd/cw/main.go` (`waitSessionCmd`, ~line 641)
- Test: `tests/integration_test.go`

### Step 1: Write the failing test

Add to `tests/integration_test.go`:

```go
func TestWaitByTagPositional(t *testing.T) {
	dir := tempDir(t, "wait-tag-positional")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch two short-lived sessions tagged "wt-42"
	for i := 0; i < 2; i++ {
		r := requestResponse(t, sock, &protocol.Request{
			Type: "Launch", Command: []string{"bash", "-c", "sleep 0.2"},
			WorkingDir: "/tmp", Tags: []string{"wt-42"},
		})
		if r.Type != "Launched" {
			t.Fatalf("launch %d: %s", i, r.Message)
		}
	}

	// cw wait wt-42 should work without --tag
	done := make(chan error, 1)
	go func() {
		done <- client.WaitForSession(target, nil, []string{"wt-42"}, "all", nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tagged sessions")
	}
}
```

### Step 2: Run the test, verify it passes (it's testing the existing `WaitForSession`)

```bash
go test ./tests/... -run TestWaitByTagPositional -v
```
Expected: PASS — this validates the underlying function works; we just need the CLI to call it right.

### Step 3: Update `waitSessionCmd` in `cmd/cw/main.go`

Replace the `waitSessionCmd` function body's arg-resolution block (lines ~663-669):

```go
// Old:
var sid *uint32
if len(args) > 0 {
    resolved, err := client.ResolveSessionArg(target, args[0])
    if err != nil {
        return err
    }
    sid = &resolved
}

// New:
var sid *uint32
var resolvedTags []string
if len(args) > 0 {
    id, tagList, err := client.ResolveSessionOrTag(target, args[0])
    if err != nil {
        return err
    }
    sid = id
    resolvedTags = tagList
}
allTags := append(resolvedTags, tags...)
```

And change the `WaitForSession` call to use `allTags` instead of `tags`:
```go
return client.WaitForSession(target, sid, allTags, condition, timeoutPtr)
```

### Step 4: Build and smoke test

```bash
make build
cw run --tag smoketest -- bash -c 'sleep 0.3'
cw wait smoketest
```
Expected: waits and returns `Session N: completed (0) (exit_code=0)`

### Step 5: Commit

```bash
git add cmd/cw/main.go
git commit -m "feat: cw wait accepts tag name as positional arg (no --tag needed)"
```

---

## Task 3: Update `killCmd` to use smart resolution

**Files:**
- Modify: `cmd/cw/main.go` (`killCmd`, ~line 305)

### Step 1: Replace the arg-resolution block in `killCmd`

The current block (lines ~336-344):
```go
if len(args) == 0 {
    return fmt.Errorf("session id or name required (or use --all / --tag)")
}
resolved, err := client.ResolveSessionArg(target, args[0])
if err != nil {
    return err
}
return client.Kill(target, resolved)
```

Replace with:
```go
if len(args) == 0 {
    return fmt.Errorf("session id, name, or tag required (or use --all)")
}
id, tagList, err := client.ResolveSessionOrTag(target, args[0])
if err != nil {
    return err
}
if len(tagList) > 0 {
    return client.KillByTags(target, tagList)
}
return client.Kill(target, *id)
```

Also remove `--tag` from `killCmd` — it's now redundant. Delete the `tags []string` var and the `if len(tags) > 0` block and the `cmd.Flags().StringSliceVar(&tags, "tag", ...)` line.

### Step 2: Build and smoke test

```bash
make build
cw run --tag killtest -- bash -c 'sleep 60'
cw list | grep killtest    # should show running
cw kill killtest           # should kill by tag
cw list | grep killtest    # should show killed
```

### Step 3: Commit

```bash
git add cmd/cw/main.go
git commit -m "feat: cw kill accepts tag name as positional arg, removes --tag flag"
```

---

## Task 4: Update `watchCmd` and `subscribeCmd` for smart resolution

**Files:**
- Modify: `cmd/cw/main.go` (`watchCmd` ~line 458, `subscribeCmd` ~line 598)

### Step 1: Update `watchCmd`

Replace the current resolution block in `watchCmd`'s RunE. The current code checks `len(tags) > 0` first; change to use `ResolveSessionOrTag` for the positional arg:

```go
// Remove the --tag flag from watchCmd entirely (delete tags var, flag, and the tags check block).
// Instead, in the RunE:
if len(args) == 0 {
    return fmt.Errorf("session id, name, or tag required")
}
id, tagList, err := client.ResolveSessionOrTag(target, args[0])
if err != nil {
    return err
}
if len(tagList) > 0 {
    var timeoutPtr *uint64
    if cmd.Flags().Changed("timeout") {
        timeoutPtr = &timeout
    }
    return client.WatchMultiByTag(target, tagList[0], os.Stdout, timeoutPtr)
}
// single session
var tailPtr *int
if cmd.Flags().Changed("tail") {
    tailPtr = &tail
}
var timeoutPtr *uint64
if cmd.Flags().Changed("timeout") {
    timeoutPtr = &timeout
}
return client.WatchSession(target, *id, tailPtr, noHistory, timeoutPtr)
```

Also change `Args: cobra.MaximumNArgs(1)` to `Args: cobra.ExactArgs(1)`.

### Step 2: Update `subscribeCmd`

Add a positional `[target]` arg that resolves to session or tag. Change `subscribeCmd`:

```go
cmd := &cobra.Command{
    Use:   "subscribe [target]",
    Short: "Subscribe to session events",
    Args:  cobra.MaximumNArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        // ... existing target/ensureNode setup ...

        var sid *uint32
        var resolvedTags []string

        if len(args) > 0 {
            id, tagList, err := client.ResolveSessionOrTag(target, args[0])
            if err != nil {
                return err
            }
            sid = id
            resolvedTags = tagList
        } else if cmd.Flags().Changed("session") {
            v := uint32(sessionID)
            sid = &v
        }

        allTags := append(resolvedTags, tags...)
        return client.SubscribeEvents(target, sid, allTags, eventTypes)
    },
}
```

Remove the `--session` flag from `subscribeCmd` (it's replaced by the positional arg).

### Step 3: Build and smoke test

```bash
make build
cw run --tag watchtest -- bash -c 'for i in 1 2 3; do echo "x$i"; sleep 0.3; done'
cw watch watchtest    # should multiplex if tag, single if session
```

### Step 4: Commit

```bash
git add cmd/cw/main.go
git commit -m "feat: cw watch and cw subscribe accept tag name as positional arg"
```

---

## Task 5: Add `cw list --status` filter

**Files:**
- Modify: `cmd/cw/main.go` (`listCmd`, ~line 234)
- Modify: `internal/client/commands.go` (`List`, ~line 61)

### Step 1: Write the failing test

Add to `tests/integration_test.go`:

```go
func TestListStatusFilter(t *testing.T) {
	dir := tempDir(t, "list-status")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch one long-running and one short session
	requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "30"}, WorkingDir: "/tmp",
	})
	requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"bash", "-c", "exit 0"}, WorkingDir: "/tmp",
	})
	time.Sleep(300 * time.Millisecond)

	// Filter running — should see 1
	sessions, err := client.ListFiltered(target, "running")
	if err != nil {
		t.Fatalf("ListFiltered: %v", err)
	}
	for _, s := range sessions {
		if s.Status != "running" {
			t.Fatalf("expected only running sessions, got %s", s.Status)
		}
	}
	if len(sessions) < 1 {
		t.Fatal("expected at least 1 running session")
	}
}
```

### Step 2: Run the test, verify it fails

```bash
go test ./tests/... -run TestListStatusFilter -v
```
Expected: FAIL — `client.ListFiltered undefined`

### Step 3: Add `ListFiltered` to `internal/client/commands.go`

Change the existing `List` function signature and add filtering:

```go
// List retrieves sessions, optionally filtered by status ("all", "running", "completed", "killed").
func List(target *Target, jsonOutput bool, statusFilter string) error {
	sessions, err := ListFiltered(target, statusFilter)
	if err != nil {
		return err
	}
	if jsonOutput {
		data, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(sessions) == 0 {
		fmt.Println("No sessions")
		return nil
	}
	printSessionTable(sessions)
	return nil
}

// ListFiltered returns sessions filtered by status. statusFilter: "all", "running", "completed", "killed".
func ListFiltered(target *Target, statusFilter string) ([]protocol.SessionInfo, error) {
	resp, err := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if err != nil {
		return nil, err
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Sessions == nil {
		return nil, fmt.Errorf("unexpected response type: %s", resp.Type)
	}
	sessions := *resp.Sessions
	if statusFilter == "" || statusFilter == "all" {
		return sessions, nil
	}
	var filtered []protocol.SessionInfo
	for _, s := range sessions {
		// s.Status can be "running", "completed (N)", "killed"
		if statusFilter == "completed" {
			if strings.HasPrefix(s.Status, "completed") {
				filtered = append(filtered, s)
			}
		} else if strings.HasPrefix(s.Status, statusFilter) {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}
```

### Step 4: Update `listCmd` in `cmd/cw/main.go`

```go
func listCmd() *cobra.Command {
	var jsonOutput bool
	var statusFilter string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ... existing target/ensureNode setup ...
			return client.List(target, jsonOutput, statusFilter)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&statusFilter, "status", "all", "Filter by status: all, running, completed, killed")
	return cmd
}
```

### Step 5: Run tests

```bash
go test ./tests/... -run TestListStatusFilter -v
go test ./internal/... -timeout 60s -count=1
```
Expected: PASS

### Step 6: Smoke test

```bash
make build
cw list --status running
```

### Step 7: Commit

```bash
git add cmd/cw/main.go internal/client/commands.go tests/integration_test.go
git commit -m "feat: add cw list --status filter (running, completed, killed, all)"
```

---

## Task 6: Improve error messages and help text

**Files:**
- Modify: `cmd/cw/main.go` (`runCmd`, `attachCmd`, `mcpServerCmd`)
- Modify: `internal/mcp/server.go` (connection error)

### Step 1: Fix `--` missing error in `runCmd` (~line 175)

Replace:
```go
if dash == -1 {
    return fmt.Errorf("command required after --")
}
```

With:
```go
if dash == -1 {
    if len(args) > 0 {
        return fmt.Errorf("missing '--' before command\n\nDid you mean: cw run -- %s\n\nUsage: cw run [name] -- <command> [args...]", strings.Join(args, " "))
    }
    return fmt.Errorf("command required\n\nUsage: cw run [name] -- <command> [args...]")
}
```

### Step 2: Add detach chord to `attachCmd` Long description

```go
cmd := &cobra.Command{
    Use:   "attach [session]",
    Short: "Attach to a session's PTY (by ID or name)",
    Long: `Attach to a running session's PTY for interactive use.

Detach without killing: press Ctrl+B d
The session continues running after you detach.

Warning: Ctrl+C sends SIGINT to the session process — use Ctrl+B d to detach safely.`,
    // ...
}
```

### Step 3: Add registration one-liner to `mcpServerCmd`

```go
return &cobra.Command{
    Use:   "mcp-server",
    Short: "Run the MCP (Model Context Protocol) server",
    Long: `Run the Codewire MCP server (communicates over stdio).

To register with Claude Code:
  claude mcp add --scope user codewire -- cw mcp-server

The node must be running before MCP tools work:
  cw node -d

The MCP server does NOT auto-start a node.`,
    // ...
}
```

### Step 4: Improve MCP connection error in `internal/mcp/server.go`

Find the `net.Dial` call (~line 1035) and wrap the error:
```go
conn, err := net.Dial("unix", sockPath)
if err != nil {
    return "", fmt.Errorf("no node running — start one with: cw node -d\n(socket: %s)", sockPath)
}
```

### Step 5: Build and verify help text

```bash
make build
cw run bash -c 'echo hi'     # should show helpful error
cw attach --help              # should show detach chord
cw mcp-server --help          # should show registration one-liner
```

### Step 6: Commit

```bash
git add cmd/cw/main.go internal/mcp/server.go
git commit -m "fix: improve error messages — missing --, detach chord in help, MCP node hint"
```

---

## Task 7: Update docs — `cw run` everywhere + expanded relay section

**Files:**
- Modify: `docs/quickstart.md`
- Modify: `docs/llms-full.txt`

### Step 1: Update `docs/quickstart.md`

Replace every occurrence of `cw launch` with `cw run`. The file has these instances:
- "First Session" section: `cw launch -- bash -c ...`
- "Core Commands" section: `cw launch -- <command>`, `cw launch --name`, `cw launch --tag`
- "Naming and Tags" section: two `cw launch` examples
- "Fan-out with tags" section: `cw launch --tag run-42`

Use find-and-replace: `cw launch` → `cw run`

### Step 2: Update `docs/llms-full.txt`

Replace every `cw launch` with `cw run` (section 3 CLI Commands, section 6 patterns).

Also expand the Relay mode section (section 7). Replace the current sparse section with:

```markdown
## 7. Relay Mode

Relay mode enables remote access and fleet management. Two tiers:
- **Standalone** (default): local Unix socket only, zero config
- **Relay** (opt-in): WireGuard tunneling, node discovery, shared KV store

### How it works

Nodes establish userspace WireGuard tunnels to a relay server. No root required,
works behind NAT. Each node generates a WireGuard key pair on first run and
registers via device auth.

```
[Your machine]               [Relay server]              [Remote machine]
  cw node  ←── WG tunnel ──→  cw relay  ←── WG tunnel ──→  cw node
  (behind NAT)                (public IP)                  (behind NAT)
```

### Connect a node to a relay

```bash
cw setup https://relay.codewire.sh
# Opens browser for GitHub OAuth (or use --invite token for headless)
```

After setup, the node auto-connects to the relay on start. Other nodes on the
same relay become accessible via `--server`.

```bash
# Headless / CI
cw setup https://relay.codewire.sh --invite <token>

# With admin token
cw setup https://relay.codewire.sh --token <admin-token>
```

### Access remote nodes

```bash
cw --server mynode list
cw --server mynode run -- make build
cw --server mynode logs 5
```

Save named connections to avoid repeating --server:
```bash
cw server add mynode https://relay.codewire.sh
cw server list
cw --server mynode list
```

### Node discovery

```bash
cw nodes    # list all nodes registered with your relay
```

Output: NAME, TUNNEL URL, STATUS (online/offline)

### Running your own relay

```bash
cw relay \
  --base-url https://relay.example.com \
  --auth-mode github \
  --allowed-users alice,bob \
  --github-client-id <id> \
  --github-client-secret <secret>
```

Key flags:
| Flag | Default | Description |
|---|---|---|
| `--base-url` | (required) | Public URL of the relay |
| `--auth-mode` | `none` | `github`, `token`, or `none` |
| `--auth-token` | — | Admin token (for `--auth-mode=token` or CI fallback) |
| `--wg-port` | `41820` | WireGuard UDP port |
| `--listen` | `:8080` | HTTP listen address |
| `--data-dir` | `~/.codewire/relay` | SQLite + WireGuard keys |
| `--allowed-users` | — | GitHub usernames allowed (github mode) |

Auth modes:
- `none` — open relay, any client can register
- `token` — clients must present the admin token
- `github` — clients authenticate via GitHub OAuth; optionally restrict to `--allowed-users`

### Invites (device onboarding)

Create an invite code to onboard a new node without sharing your admin token:
```bash
cw invite               # single-use, expires in 1h
cw invite --uses 5 --ttl 24h
cw invite --qr          # print QR code for mobile
```

Share the resulting URL or run:
```bash
cw setup https://relay.example.com --invite <token>
```

### Revoke a node

```bash
cw revoke <node-name>
```

### Shared KV store

```bash
cw kv set mykey myvalue
cw kv set mykey myvalue --ttl 5m
cw kv get mykey
cw kv list --prefix my
cw kv delete mykey
```

Via MCP: `codewire_kv_set/get/list/delete` (requires relay configured).

The KV store is useful for cross-node coordination — e.g., a supervisor on one
node signalling workers on another.
```

### Step 3: Verify docs look right

```bash
head -30 docs/quickstart.md   # confirm no cw launch
grep "cw launch" docs/llms-full.txt   # should return nothing
grep "Relay mode" docs/llms-full.txt  # should find the section
```

### Step 4: Commit

```bash
git add docs/quickstart.md docs/llms-full.txt
git commit -m "docs: replace cw launch with cw run, expand relay section in llms-full.txt"
```

---

## Task 8: Run full test suite and tag

### Step 1: Run all tests

```bash
go test ./internal/... ./tests/... -timeout 120s -count=1
```
Expected: all PASS

### Step 2: Build final binary

```bash
make build
./cw --version
```

### Step 3: Run /cpv to commit, tag, push

Use the `cpv` skill to create a version tag and push both `codewire` and `codewire-demo`.

---

## Summary of files changed

| File | Change |
|---|---|
| `internal/client/commands.go` | Add `ResolveSessionOrTag`, add `ListFiltered`, update `List` signature |
| `cmd/cw/main.go` | Update `waitSessionCmd`, `killCmd`, `watchCmd`, `subscribeCmd`, `listCmd`, `runCmd`, `attachCmd`, `mcpServerCmd` |
| `internal/mcp/server.go` | Improve connection error message |
| `tests/integration_test.go` | Add `TestResolveSessionOrTag`, `TestWaitByTagPositional`, `TestListStatusFilter` |
| `docs/quickstart.md` | Replace `cw launch` → `cw run` |
| `docs/llms-full.txt` | Replace `cw launch` → `cw run`, expand relay section |
