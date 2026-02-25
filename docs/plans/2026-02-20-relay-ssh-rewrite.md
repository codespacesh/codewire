# Relay SSH Rewrite Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the `coder/wgtunnel` WireGuard overlay with persistent WebSocket node agents and an SSH gateway, enabling Termius-style SSH access to nodes via a hosted relay.

**Architecture:** Nodes connect outward to the relay via persistent authenticated WebSocket (`/node/connect`). The relay maintains an in-memory hub of connected nodes. Users SSH into `relay.codespace.sh:2222` with username=nodename and password=node-token. The relay finds the node in the hub, sends an `SSHRequest` JSON message, and the node opens a back-connection WebSocket (`/node/back/{id}`) to bridge raw PTY bytes. No WireGuard, no wildcard DNS, no root required.

**Tech Stack:** `golang.org/x/crypto/ssh` (new; already indirect dep), `nhooyr.io/websocket` (existing), `creack/pty` (existing for PTY in node agent), `modernc.org/sqlite` (existing store)

---

## Task 1: Update store — NodeRecord and new NodeGetByToken method

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go`

**Step 1: Write the failing tests**

In `internal/store/sqlite_test.go` (create if absent), add:
```go
func TestNodeToken(t *testing.T) {
    st, _ := NewSQLiteStore(t.TempDir())
    defer st.Close()

    err := st.NodeRegister(ctx, NodeRecord{
        Name:         "mynode",
        Token:        "secrettoken",
        AuthorizedAt: time.Now(),
        LastSeenAt:   time.Now(),
    })
    require.NoError(t, err)

    got, err := st.NodeGetByToken(ctx, "secrettoken")
    require.NoError(t, err)
    require.Equal(t, "mynode", got.Name)

    got2, err := st.NodeGetByToken(ctx, "wrongtoken")
    require.NoError(t, err)
    require.Nil(t, got2)
}
```

**Step 2: Run test to verify it fails**

```
go test ./internal/store/... -run TestNodeToken -v
```
Expected: compile error — `NodeGetByToken` not defined, `NodeRecord.Token` doesn't exist.

**Step 3: Update `NodeRecord` in `internal/store/store.go`**

Replace:
```go
type NodeRecord struct {
    Name         string    `json:"name"`
    PublicKey    string    `json:"public_key"`
    TunnelURL    string    `json:"tunnel_url"`
    GitHubID     *int64    `json:"github_id,omitempty"`
    AuthorizedAt time.Time `json:"authorized_at"`
    LastSeenAt   time.Time `json:"last_seen_at"`
}
```
With:
```go
type NodeRecord struct {
    Name         string    `json:"name"`
    Token        string    `json:"token"`          // random auth token (replaces WireGuard public key)
    GitHubID     *int64    `json:"github_id,omitempty"`
    AuthorizedAt time.Time `json:"authorized_at"`
    LastSeenAt   time.Time `json:"last_seen_at"`
}
```

Add to `Store` interface:
```go
NodeGetByToken(ctx context.Context, token string) (*NodeRecord, error)
```

**Step 4: Update SQLite schema in `internal/store/sqlite.go`**

In `NewSQLiteStore`, update the nodes table DDL:
```sql
CREATE TABLE IF NOT EXISTS nodes (
    name          TEXT PRIMARY KEY,
    token         TEXT NOT NULL UNIQUE,
    github_id     INTEGER,
    authorized_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL
);
```
(Remove `public_key` and `tunnel_url` columns.)

Update `NodeRegister` to use `token` instead of `public_key`/`tunnel_url`.
Update `NodeList`, `NodeGet` to scan `token` instead.

Implement `NodeGetByToken`:
```go
func (s *SQLiteStore) NodeGetByToken(ctx context.Context, token string) (*NodeRecord, error) {
    row := s.db.QueryRowContext(ctx,
        `SELECT name, token, github_id, authorized_at, last_seen_at FROM nodes WHERE token = ?`, token)
    var n NodeRecord
    var authorizedAt, lastSeenAt string
    var githubID *int64
    err := row.Scan(&n.Name, &n.Token, &githubID, &authorizedAt, &lastSeenAt)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    n.GitHubID = githubID
    n.AuthorizedAt, _ = time.Parse(time.RFC3339, authorizedAt)
    n.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeenAt)
    return &n, nil
}
```

**Step 5: Run test to verify it passes**

```
go test ./internal/store/... -run TestNodeToken -v
```
Expected: PASS

**Step 6: Run all store tests**

```
go test ./internal/store/... -v
```
Expected: all pass (or update any that broke due to schema change)

**Step 7: Commit**

```bash
git add internal/store/
git commit -m "feat(store): replace WireGuard public_key with node token in NodeRecord"
```

---

## Task 2: Add RelayToken to config

**Files:**
- Modify: `internal/config/config.go`

**Step 1: Add field**

Add to `Config`:
```go
RelayToken *string `toml:"relay_token,omitempty"` // node auth token for relay
```

Add env var override in `LoadConfig` (after the existing relay_url override):
```go
if cfg.RelayToken == nil {
    if t := os.Getenv("CODEWIRE_RELAY_TOKEN"); t != "" {
        cfg.RelayToken = &t
    }
}
```

**Step 2: Build to verify no compile errors**

```
make build
```
Expected: builds cleanly

**Step 3: Commit**

```bash
git add internal/config/config.go
git commit -m "feat(config): add relay_token field for node agent auth"
```

---

## Task 3: Create node hub (`internal/relay/hub.go`)

**Files:**
- Create: `internal/relay/hub.go`

**Step 1: Write the failing test**

Create `internal/relay/hub_test.go`:
```go
package relay_test

import (
    "context"
    "testing"
    "time"

    "github.com/codewiresh/codewire/internal/relay"
)

func TestHubRegisterUnregister(t *testing.T) {
    h := relay.NewNodeHub()
    h.Register("n1", nil)           // nil sender for test
    if !h.Has("n1") { t.Fatal("expected n1 registered") }
    h.Unregister("n1")
    if h.Has("n1") { t.Fatal("expected n1 unregistered") }
}

func TestHubSend(t *testing.T) {
    h := relay.NewNodeHub()
    ch := make(chan relay.HubMessage, 1)
    h.Register("n1", ch)
    err := h.Send("n1", relay.HubMessage{Type: "test"})
    if err != nil { t.Fatal(err) }
    select {
    case msg := <-ch:
        if msg.Type != "test" { t.Fatalf("wrong type: %s", msg.Type) }
    case <-time.After(time.Second):
        t.Fatal("timeout")
    }
}

func TestHubSendUnknown(t *testing.T) {
    h := relay.NewNodeHub()
    err := h.Send("missing", relay.HubMessage{Type: "x"})
    if err == nil { t.Fatal("expected error for unknown node") }
}
```

**Step 2: Run test to verify it fails**

```
go test ./internal/relay/... -run TestHub -v
```
Expected: package not found

**Step 3: Implement `internal/relay/hub.go`**

```go
package relay

import (
    "fmt"
    "sync"
)

// HubMessage is a control message sent to a connected node agent.
type HubMessage struct {
    Type      string `json:"type"`
    SessionID string `json:"session_id,omitempty"`
    Cols      int    `json:"cols,omitempty"`
    Rows      int    `json:"rows,omitempty"`
}

// NodeHub tracks connected node agents (in-memory).
type NodeHub struct {
    mu    sync.RWMutex
    nodes map[string]chan<- HubMessage
}

func NewNodeHub() *NodeHub {
    return &NodeHub{nodes: make(map[string]chan<- HubMessage)}
}

func (h *NodeHub) Register(name string, ch chan<- HubMessage) {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.nodes[name] = ch
}

func (h *NodeHub) Unregister(name string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    delete(h.nodes, name)
}

func (h *NodeHub) Has(name string) bool {
    h.mu.RLock()
    defer h.mu.RUnlock()
    _, ok := h.nodes[name]
    return ok
}

// Send delivers a message to the named node. Returns error if node not connected.
func (h *NodeHub) Send(name string, msg HubMessage) error {
    h.mu.RLock()
    ch, ok := h.nodes[name]
    h.mu.RUnlock()
    if !ok {
        return fmt.Errorf("node %q not connected", name)
    }
    select {
    case ch <- msg:
        return nil
    default:
        return fmt.Errorf("node %q message buffer full", name)
    }
}
```

**Step 4: Run test to verify it passes**

```
go test ./internal/relay/... -run TestHub -v
```
Expected: PASS

**Step 5: Commit**

```bash
git add internal/relay/
git commit -m "feat(relay): add NodeHub for in-memory node agent registry"
```

---

## Task 4: Relay node agent endpoint (`/node/connect`)

**Files:**
- Modify: `internal/tunnel/relay.go` — add hub, `/node/connect` handler

**Context:** We're adding to the existing relay. The hub is passed into `RunRelay` so the SSH server (Task 6) can also use it.

**Step 1: Write failing test**

In `tests/relay_agent_test.go` (create):
```go
//go:build integration
package tests

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "nhooyr.io/websocket"
    "nhooyr.io/websocket/wsjson"

    "github.com/codewiresh/codewire/internal/relay"
    "github.com/codewiresh/codewire/internal/store"
)

func TestNodeConnect(t *testing.T) {
    st, _ := store.NewSQLiteStore(t.TempDir())
    defer st.Close()
    ctx := context.Background()
    _ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

    hub := relay.NewNodeHub()
    mux := http.NewServeMux()
    relay.RegisterNodeConnectHandler(mux, hub, st)

    srv := httptest.NewServer(mux)
    defer srv.Close()

    wsURL := "ws" + srv.URL[4:] + "/node/connect"
    conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
        HTTPHeader: http.Header{"Authorization": {"Bearer tok1"}},
    })
    if err != nil {
        t.Fatalf("dial: %v", err)
    }
    defer conn.CloseNow(ctx)

    if !hub.Has("n1") {
        t.Fatal("expected n1 in hub")
    }

    conn.Close(websocket.StatusNormalClosure, "")
    time.Sleep(100 * time.Millisecond)
    if hub.Has("n1") {
        t.Fatal("expected n1 removed from hub after disconnect")
    }
}
```

**Step 2: Run test to verify it fails**

```
go test ./tests/... -run TestNodeConnect -tags integration -v
```
Expected: compile error — `relay.RegisterNodeConnectHandler` not defined

**Step 3: Implement `RegisterNodeConnectHandler` in `internal/relay/` (or tunnel relay.go)**

Create `internal/relay/node_handler.go`:
```go
package relay

import (
    "context"
    "encoding/json"
    "log/slog"
    "net/http"
    "strings"

    "nhooyr.io/websocket"

    "github.com/codewiresh/codewire/internal/store"
)

// RegisterNodeConnectHandler adds GET /node/connect to mux.
// Nodes connect here with Authorization: Bearer <node-token>.
// The handler registers them in the hub and reads HubMessages to send to the node.
func RegisterNodeConnectHandler(mux *http.ServeMux, hub *NodeHub, st store.Store) {
    mux.HandleFunc("GET /node/connect", func(w http.ResponseWriter, r *http.Request) {
        // Authenticate node.
        token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
        if token == "" {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        node, err := st.NodeGetByToken(r.Context(), token)
        if err != nil || node == nil {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }

        // Upgrade to WebSocket.
        ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
            InsecureSkipVerify: true, // origin check done by token auth
        })
        if err != nil {
            return
        }
        defer ws.CloseNow(r.Context())

        slog.Info("node agent connected", "node", node.Name)

        // Register in hub — messages from SSH handler flow here.
        msgCh := make(chan HubMessage, 16)
        hub.Register(node.Name, msgCh)
        defer hub.Unregister(node.Name)

        _ = st.NodeUpdateLastSeen(r.Context(), node.Name)

        ctx := r.Context()

        // Write loop: relay hub messages to node.
        go func() {
            for {
                select {
                case msg, ok := <-msgCh:
                    if !ok {
                        return
                    }
                    data, _ := json.Marshal(msg)
                    if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
                        return
                    }
                case <-ctx.Done():
                    return
                }
            }
        }()

        // Read loop: keep connection alive (nodes may send pings or status).
        for {
            _, _, err := ws.Read(ctx)
            if err != nil {
                slog.Info("node agent disconnected", "node", node.Name, "err", err)
                return
            }
        }
    })
}
```

**Step 4: Run test to verify it passes**

```
go test ./tests/... -run TestNodeConnect -tags integration -v
```
Expected: PASS

**Step 5: Commit**

```bash
git add internal/relay/ tests/
git commit -m "feat(relay): add /node/connect WebSocket endpoint for node agents"
```

---

## Task 5: Back-connection endpoint (`/node/back/{id}`)

**Files:**
- Create: `internal/relay/back_handler.go`

**Context:** When an SSH session starts, the relay sends an `SSHRequest` to the node via the hub. The node dials back `/node/back/{session_id}` — the relay was waiting on a channel and bridges the WebSocket to the SSH channel.

**Step 1: Write failing test**

In `tests/relay_agent_test.go`, add:
```go
func TestBackConnect(t *testing.T) {
    st, _ := store.NewSQLiteStore(t.TempDir())
    defer st.Close()
    ctx := context.Background()
    _ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

    sessions := relay.NewPendingSessions()
    mux := http.NewServeMux()
    relay.RegisterBackHandler(mux, sessions, st)

    srv := httptest.NewServer(mux)
    defer srv.Close()

    // Pre-register a pending session channel.
    ch := sessions.Expect("sess1")

    wsURL := "ws" + srv.URL[4:] + "/node/back/sess1"
    conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
        HTTPHeader: http.Header{"Authorization": {"Bearer tok1"}},
    })
    if err != nil {
        t.Fatalf("dial: %v", err)
    }
    defer conn.CloseNow(ctx)

    // Wait for the back-connection to be delivered to the pending channel.
    select {
    case nc := <-ch:
        if nc == nil { t.Fatal("nil connection") }
    case <-time.After(2 * time.Second):
        t.Fatal("timeout waiting for back connection")
    }
}
```

**Step 2: Run test to verify it fails**

```
go test ./tests/... -run TestBackConnect -tags integration -v
```
Expected: compile error

**Step 3: Implement `internal/relay/back_handler.go`**

```go
package relay

import (
    "net"
    "net/http"
    "strings"
    "sync"

    "nhooyr.io/websocket"

    "github.com/codewiresh/codewire/internal/store"
)

// PendingSessions tracks back-connections that SSH sessions are waiting for.
type PendingSessions struct {
    mu   sync.Mutex
    waits map[string]chan net.Conn
}

func NewPendingSessions() *PendingSessions {
    return &PendingSessions{waits: make(map[string]chan net.Conn)}
}

// Expect registers a channel that will receive the back-connection for sessionID.
// The caller must call this before signalling the node.
func (p *PendingSessions) Expect(sessionID string) <-chan net.Conn {
    ch := make(chan net.Conn, 1)
    p.mu.Lock()
    p.waits[sessionID] = ch
    p.mu.Unlock()
    return ch
}

func (p *PendingSessions) deliver(sessionID string, conn net.Conn) bool {
    p.mu.Lock()
    ch, ok := p.waits[sessionID]
    if ok {
        delete(p.waits, sessionID)
    }
    p.mu.Unlock()
    if ok {
        ch <- conn
    }
    return ok
}

func (p *PendingSessions) Cancel(sessionID string) {
    p.mu.Lock()
    ch, ok := p.waits[sessionID]
    if ok {
        delete(p.waits, sessionID)
        close(ch)
    }
    p.mu.Unlock()
}

// RegisterBackHandler adds GET /node/back/{session_id} to mux.
func RegisterBackHandler(mux *http.ServeMux, sessions *PendingSessions, st store.Store) {
    mux.HandleFunc("GET /node/back/{session_id}", func(w http.ResponseWriter, r *http.Request) {
        // Authenticate node.
        token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
        if token == "" {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        node, err := st.NodeGetByToken(r.Context(), token)
        if err != nil || node == nil {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }

        sessionID := r.PathValue("session_id")

        ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
        if err != nil {
            return
        }

        // Wrap as net.Conn and deliver to waiting SSH session.
        nc := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
        if !sessions.deliver(sessionID, nc) {
            // No one waiting — session may have timed out.
            ws.Close(websocket.StatusNormalClosure, "no waiter")
            return
        }

        // nc is now owned by the SSH bridge. Block until context done
        // so the HTTP handler doesn't return (which would close the conn).
        <-r.Context().Done()
    })
}
```

**Step 4: Run test**

```
go test ./tests/... -run TestBackConnect -tags integration -v
```
Expected: PASS

**Step 5: Commit**

```bash
git add internal/relay/ tests/
git commit -m "feat(relay): add /node/back/{id} endpoint for SSH back-connections"
```

---

## Task 6: SSH server

**Files:**
- Create: `internal/relay/ssh.go`

**Context:** SSH server listens on `:2222`. Auth: username=nodename, password=nodetoken. On PTY request: send SSHRequest to node via hub, wait for back-connection, bridge raw bytes. Resize messages forwarded as JSON side-channel is not needed — use an out-of-band resize mechanism or ignore for Phase 1. Phase 1: just pipe raw bytes, window resize not forwarded (can add later).

**Step 1: Write the failing test**

In `tests/relay_ssh_test.go` (create):
```go
//go:build integration
package tests

import (
    "context"
    "net"
    "testing"
    "time"

    "golang.org/x/crypto/ssh"

    localrelay "github.com/codewiresh/codewire/internal/relay"
    "github.com/codewiresh/codewire/internal/store"
)

func TestSSHConnectAndShell(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Setup store with a node.
    st, _ := store.NewSQLiteStore(t.TempDir())
    _ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

    hub := localrelay.NewNodeHub()
    sessions := localrelay.NewPendingSessions()

    // Start the SSH server.
    ln, _ := net.Listen("tcp", "127.0.0.1:0")
    sshSrv, err := localrelay.NewSSHServer(st, hub, sessions)
    if err != nil {
        t.Fatal(err)
    }
    go sshSrv.Serve(ctx, ln)

    // Simulate a node back-connecting (echo server).
    // When hub receives SSHRequest for n1, the node dials back.
    msgCh := make(chan localrelay.HubMessage, 4)
    hub.Register("n1", msgCh)
    go func() {
        for msg := range msgCh {
            if msg.Type == "SSHRequest" {
                // Simulate node: create a pipe and deliver as back-connection.
                client, server := net.Pipe()
                sessions.DeliverForTest(msg.SessionID, server)
                // Echo everything back.
                go func() {
                    buf := make([]byte, 4096)
                    for {
                        n, err := client.Read(buf)
                        if err != nil { return }
                        client.Write(buf[:n])
                    }
                }()
            }
        }
    }()

    // SSH client connects.
    sshCfg := &ssh.ClientConfig{
        User:            "n1",
        Auth:            []ssh.AuthMethod{ssh.Password("tok1")},
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
        Timeout:         5 * time.Second,
    }
    client, err := ssh.Dial("tcp", ln.Addr().String(), sshCfg)
    if err != nil {
        t.Fatalf("ssh dial: %v", err)
    }
    defer client.Close()

    sess, err := client.NewSession()
    if err != nil {
        t.Fatalf("ssh session: %v", err)
    }
    defer sess.Close()

    sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{})
    _ = sess.Shell() // start shell (echo server)
    t.Log("SSH shell started successfully")
}
```

**Step 2: Run test to verify it fails**

```
go test ./tests/... -run TestSSHConnect -tags integration -v
```
Expected: compile error — `relay.NewSSHServer` not defined

**Step 3: Implement `internal/relay/ssh.go`**

Add `golang.org/x/crypto/ssh` as a direct dependency:
```bash
go get golang.org/x/crypto/ssh
```

```go
package relay

import (
    "context"
    "crypto/rand"
    "crypto/subtle"
    "encoding/binary"
    "fmt"
    "io"
    "log/slog"
    "net"
    "time"

    "golang.org/x/crypto/ssh"

    "github.com/codewiresh/codewire/internal/store"
)

// SSHServer wraps an ssh.Server with relay-specific auth and routing.
type SSHServer struct {
    config   *ssh.ServerConfig
    hub      *NodeHub
    sessions *PendingSessions
    st       store.Store
}

func NewSSHServer(st store.Store, hub *NodeHub, sessions *PendingSessions) (*SSHServer, error) {
    // Generate an ephemeral host key (TODO: persist to DataDir for stable fingerprint).
    hostKey, err := generateEd25519Key()
    if err != nil {
        return nil, fmt.Errorf("generating host key: %w", err)
    }

    srv := &SSHServer{hub: hub, sessions: sessions, st: st}

    srv.config = &ssh.ServerConfig{
        PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            defer cancel()
            node, err := st.NodeGetByToken(ctx, string(pass))
            if err != nil || node == nil {
                return nil, fmt.Errorf("authentication failed")
            }
            if subtle.ConstantTimeCompare([]byte(c.User()), []byte(node.Name)) != 1 {
                return nil, fmt.Errorf("username does not match node name")
            }
            return &ssh.Permissions{
                Extensions: map[string]string{"node_name": node.Name},
            }, nil
        },
    }
    srv.config.AddHostKey(hostKey)
    return srv, nil
}

// Serve accepts SSH connections on ln until ctx is cancelled.
func (s *SSHServer) Serve(ctx context.Context, ln net.Listener) {
    go func() {
        <-ctx.Done()
        ln.Close()
    }()
    for {
        tc, err := ln.Accept()
        if err != nil {
            return
        }
        go s.handleConn(ctx, tc)
    }
}

func (s *SSHServer) handleConn(ctx context.Context, tc net.Conn) {
    defer tc.Close()

    sshConn, chans, reqs, err := ssh.NewServerConn(tc, s.config)
    if err != nil {
        return
    }
    defer sshConn.Close()
    go ssh.DiscardRequests(reqs)

    nodeName := sshConn.Permissions.Extensions["node_name"]

    for newChan := range chans {
        if newChan.ChannelType() != "session" {
            newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
            continue
        }
        ch, reqs, err := newChan.Accept()
        if err != nil {
            return
        }
        go s.handleSession(ctx, ch, reqs, nodeName)
    }
}

func (s *SSHServer) handleSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, nodeName string) {
    defer ch.Close()

    sessionID := generateSessionID()
    var cols, rows uint32 = 80, 24

    // Process requests until we get a shell/exec or pty request.
    for req := range reqs {
        switch req.Type {
        case "pty-req":
            // Parse terminal size from PTY request payload.
            if len(req.Payload) >= 8 {
                // pty-req payload: string(term), uint32(cols), uint32(rows), ...
                termLen := binary.BigEndian.Uint32(req.Payload[0:4])
                if int(4+termLen+8) <= len(req.Payload) {
                    cols = binary.BigEndian.Uint32(req.Payload[4+termLen:])
                    rows = binary.BigEndian.Uint32(req.Payload[4+termLen+4:])
                }
            }
            if req.WantReply {
                req.Reply(true, nil)
            }
        case "window-change":
            // Forward resize: ignored in Phase 1.
            if req.WantReply {
                req.Reply(true, nil)
            }
        case "shell", "exec":
            if req.WantReply {
                req.Reply(true, nil)
            }
            // Bridge to node.
            s.bridgeToNode(ctx, ch, nodeName, sessionID, int(cols), int(rows))
            return
        default:
            if req.WantReply {
                req.Reply(false, nil)
            }
        }
    }
}

func (s *SSHServer) bridgeToNode(ctx context.Context, ch ssh.Channel, nodeName, sessionID string, cols, rows int) {
    // Register pending back-connection channel before signalling node.
    backCh := s.sessions.Expect(sessionID)
    defer s.sessions.Cancel(sessionID)

    // Signal node via hub.
    err := s.hub.Send(nodeName, HubMessage{
        Type:      "SSHRequest",
        SessionID: sessionID,
        Cols:      cols,
        Rows:      rows,
    })
    if err != nil {
        slog.Error("SSH: node not connected", "node", nodeName, "err", err)
        ch.Stderr().Write([]byte("node not connected\r\n"))
        return
    }

    // Wait for node's back-connection.
    var backConn net.Conn
    select {
    case conn, ok := <-backCh:
        if !ok || conn == nil {
            slog.Error("SSH: back-connection channel closed", "node", nodeName)
            return
        }
        backConn = conn
    case <-time.After(10 * time.Second):
        ch.Stderr().Write([]byte("node connection timed out\r\n"))
        return
    case <-ctx.Done():
        return
    }
    defer backConn.Close()

    slog.Info("SSH: bridging session", "node", nodeName, "session", sessionID)

    // Pipe SSH channel ↔ back-connection.
    done := make(chan struct{}, 2)
    go func() { io.Copy(backConn, ch); done <- struct{}{} }()
    go func() { io.Copy(ch, backConn); done <- struct{}{} }()
    <-done
}

// DeliverForTest allows tests to inject a back-connection directly.
func (p *PendingSessions) DeliverForTest(sessionID string, conn net.Conn) {
    p.deliver(sessionID, conn)
}

func generateSessionID() string {
    b := make([]byte, 16)
    rand.Read(b)
    return fmt.Sprintf("%x", b)
}

func generateEd25519Key() (ssh.Signer, error) {
    // Use crypto/ed25519 via ssh.NewSignerFromKey.
    // golang.org/x/crypto/ssh can generate this.
    private, err := generateEd25519Private()
    if err != nil {
        return nil, err
    }
    return ssh.NewSignerFromKey(private)
}
```

Note: `generateEd25519Private()` uses `crypto/ed25519` — add a helper in the same file:
```go
import "crypto/ed25519"

func generateEd25519Private() (ed25519.PrivateKey, error) {
    _, priv, err := ed25519.GenerateKey(rand.Reader)
    return priv, err
}
```

**Step 4: Run test**

```
go test ./tests/... -run TestSSHConnect -tags integration -v
```
Expected: PASS

**Step 5: Commit**

```bash
git add internal/relay/ tests/
git commit -m "feat(relay): add SSH server with password auth and node back-connection bridge"
```

---

## Task 7: Node agent (`internal/relay/agent.go`)

**Files:**
- Create: `internal/relay/agent.go`

**Context:** This runs inside the node process. It maintains a persistent WebSocket to the relay, handles SSHRequest messages by dialing back to the relay, spawning a bash PTY, and bridging bytes.

**Step 1: Write failing test**

In `tests/relay_agent_integration_test.go`:
```go
//go:build integration
package tests

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "nhooyr.io/websocket"

    localrelay "github.com/codewiresh/codewire/internal/relay"
    "github.com/codewiresh/codewire/internal/store"
)

func TestAgentConnectsToHub(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    st, _ := store.NewSQLiteStore(t.TempDir())
    _ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

    hub := localrelay.NewNodeHub()
    sessions := localrelay.NewPendingSessions()

    mux := http.NewServeMux()
    localrelay.RegisterNodeConnectHandler(mux, hub, st)
    localrelay.RegisterBackHandler(mux, sessions, st)
    srv := httptest.NewServer(mux)
    defer srv.Close()

    // Start node agent.
    relayURL := "http" + srv.URL[4:]
    agentCfg := localrelay.AgentConfig{
        RelayURL:  relayURL,
        NodeName:  "n1",
        NodeToken: "tok1",
    }
    go localrelay.RunAgent(ctx, agentCfg)

    // Wait for agent to connect.
    deadline := time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        if hub.Has("n1") {
            break
        }
        time.Sleep(50 * time.Millisecond)
    }
    if !hub.Has("n1") {
        t.Fatal("agent did not connect to hub")
    }
}
```

**Step 2: Run test to verify it fails**

```
go test ./tests/... -run TestAgentConnects -tags integration -v
```
Expected: compile error

**Step 3: Implement `internal/relay/agent.go`**

```go
package relay

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "os/exec"
    "time"

    "github.com/creack/pty"
    "nhooyr.io/websocket"
)

// AgentConfig configures the node agent.
type AgentConfig struct {
    RelayURL  string // e.g. "https://relay.codespace.sh"
    NodeName  string
    NodeToken string
}

// RunAgent connects to the relay and handles incoming SSH requests.
// It reconnects automatically with exponential backoff.
func RunAgent(ctx context.Context, cfg AgentConfig) {
    backoff := time.Second
    for {
        err := runAgentOnce(ctx, cfg)
        if ctx.Err() != nil {
            return
        }
        slog.Warn("relay agent disconnected", "err", err, "retry_in", backoff)
        select {
        case <-time.After(backoff):
        case <-ctx.Done():
            return
        }
        if backoff < 30*time.Second {
            backoff *= 2
        }
    }
}

func runAgentOnce(ctx context.Context, cfg AgentConfig) error {
    wsURL := toWS(cfg.RelayURL) + "/node/connect"
    ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
        HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.NodeToken}},
    })
    if err != nil {
        return fmt.Errorf("dial: %w", err)
    }
    defer ws.CloseNow(ctx)

    slog.Info("relay agent connected", "relay", cfg.RelayURL, "node", cfg.NodeName)

    for {
        _, data, err := ws.Read(ctx)
        if err != nil {
            return fmt.Errorf("read: %w", err)
        }
        var msg HubMessage
        if err := json.Unmarshal(data, &msg); err != nil {
            continue
        }
        if msg.Type == "SSHRequest" {
            go handleSSHBack(ctx, cfg, msg)
        }
    }
}

func handleSSHBack(ctx context.Context, cfg AgentConfig, msg HubMessage) {
    cols, rows := msg.Cols, msg.Rows
    if cols == 0 { cols = 80 }
    if rows == 0 { rows = 24 }

    // Dial back-connection to relay.
    backURL := toWS(cfg.RelayURL) + "/node/back/" + msg.SessionID
    ws, _, err := websocket.Dial(ctx, backURL, &websocket.DialOptions{
        HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.NodeToken}},
    })
    if err != nil {
        slog.Error("relay agent: back-connect failed", "err", err, "session", msg.SessionID)
        return
    }
    defer ws.CloseNow(ctx)

    nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
    defer nc.Close()

    // Spawn a bash shell attached to a PTY.
    cmd := exec.CommandContext(ctx, "bash", "--login")
    ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
        Rows: uint16(rows), Cols: uint16(cols),
    })
    if err != nil {
        slog.Error("relay agent: pty start failed", "err", err)
        return
    }
    defer func() {
        ptmx.Close()
        cmd.Wait()
    }()

    // Bridge PTY ↔ back-connection.
    go io.Copy(nc, ptmx)
    io.Copy(ptmx, nc)
}

// toWS converts http(s):// to ws(s)://.
func toWS(u string) string {
    if len(u) > 5 && u[:5] == "https" {
        return "wss" + u[5:]
    }
    if len(u) > 4 && u[:4] == "http" {
        return "ws" + u[4:]
    }
    return u
}
```

**Step 4: Run test**

```
go test ./tests/... -run TestAgentConnects -tags integration -v
```
Expected: PASS

**Step 5: Run all relay tests**

```
go test ./internal/relay/... ./tests/... -tags integration -v
```
Expected: all pass

**Step 6: Commit**

```bash
git add internal/relay/ tests/
git commit -m "feat(relay): add node agent with reconnect loop and SSH back-connection"
```

---

## Task 8: Update RunRelay to use new relay package

**Files:**
- Modify: `internal/tunnel/relay.go`
- Create: `internal/relay/relay.go` (new RunRelay entry point)

**Context:** Move the relay entry point from `internal/tunnel/relay.go` to `internal/relay/relay.go`. Remove all `coder/wgtunnel` code. Keep OAuth, KV, invites, node management HTTP endpoints. Add hub, SSH server startup.

**Step 1: Create `internal/relay/relay.go`**

This is a new `RunRelay` that replaces `tunnel.RunRelay`. Key structure:

```go
package relay

import (
    "context"
    "fmt"
    "net"
    "net/http"
    "os"
    "time"

    "github.com/codewiresh/codewire/internal/oauth"
    "github.com/codewiresh/codewire/internal/store"
)

type RelayConfig struct {
    BaseURL        string
    ListenAddr     string   // HTTP listen (default ":8080")
    SSHListenAddr  string   // SSH listen (default ":2222")
    DataDir        string
    AuthMode       string   // "github", "token", "none"
    AuthToken      string
    AllowedUsers   []string
    GitHubClientID     string
    GitHubClientSecret string
}

func RunRelay(ctx context.Context, cfg RelayConfig) error {
    if cfg.ListenAddr == "" { cfg.ListenAddr = ":8080" }
    if cfg.SSHListenAddr == "" { cfg.SSHListenAddr = ":2222" }

    st, err := store.NewSQLiteStore(cfg.DataDir)
    if err != nil { return fmt.Errorf("opening store: %w", err) }
    defer st.Close()

    hub := NewNodeHub()
    sessions := NewPendingSessions()

    sshSrv, err := NewSSHServer(st, hub, sessions)
    if err != nil { return fmt.Errorf("creating ssh server: %w", err) }

    authMiddleware := oauth.RequireAuth(st, cfg.AuthToken)
    mux := http.NewServeMux()

    // Node agent endpoints.
    RegisterNodeConnectHandler(mux, hub, st)
    RegisterBackHandler(mux, sessions, st)

    // Node registration (issues a node token).
    mux.Handle("POST /api/v1/nodes", authMiddleware(http.HandlerFunc(nodeRegisterHandler(st))))
    mux.Handle("DELETE /api/v1/nodes/{name}", authMiddleware(http.HandlerFunc(nodeRevokeHandler(st))))
    mux.HandleFunc("GET /api/v1/nodes", nodesListHandler(st))

    // GitHub OAuth (when AuthMode == "github").
    if cfg.AuthMode == "github" {
        mux.HandleFunc("GET /auth/github/manifest/callback", oauth.ManifestCallbackHandler(st, cfg.BaseURL))
        mux.HandleFunc("GET /auth/github", oauth.LoginHandler(st, cfg.BaseURL, cfg.AllowedUsers))
        mux.HandleFunc("GET /auth/github/callback", oauth.CallbackHandler(st, cfg.BaseURL, cfg.AllowedUsers))
        mux.HandleFunc("GET /auth/session", oauth.SessionInfoHandler(st))
        // ... (keep existing GitHub app credential seeding)
    }

    // Invite endpoints (keep as-is from tunnel/relay.go).
    mux.Handle("POST /api/v1/invites", authMiddleware(http.HandlerFunc(inviteCreateHandler(st))))
    mux.Handle("GET /api/v1/invites", authMiddleware(http.HandlerFunc(inviteListHandler(st))))
    mux.Handle("DELETE /api/v1/invites/{token}", authMiddleware(http.HandlerFunc(inviteDeleteHandler(st))))
    mux.HandleFunc("POST /api/v1/join", rateLimitMiddleware(newRateLimiter(10, time.Minute), joinHandler(st, cfg)))
    mux.HandleFunc("GET /join", joinPageHandler(cfg.BaseURL))

    // KV API (keep as-is).
    mux.HandleFunc("PUT /api/v1/kv/{namespace}/{key}", kvSetHandler(st))
    mux.HandleFunc("GET /api/v1/kv/{namespace}/{key}", kvGetHandler(st))
    mux.HandleFunc("DELETE /api/v1/kv/{namespace}/{key}", kvDeleteHandler(st))
    mux.HandleFunc("GET /api/v1/kv/{namespace}", kvListHandler(st))

    // Health check.
    mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte("ok"))
    })

    // Start SSH server.
    sshLn, err := net.Listen("tcp", cfg.SSHListenAddr)
    if err != nil { return fmt.Errorf("ssh listen: %w", err) }
    go sshSrv.Serve(ctx, sshLn)
    fmt.Fprintf(os.Stderr, "[relay] SSH listening on %s\n", cfg.SSHListenAddr)

    // Start HTTP server.
    httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
    errCh := make(chan error, 1)
    go func() {
        fmt.Fprintf(os.Stderr, "[relay] HTTP listening on %s\n", cfg.ListenAddr)
        if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            errCh <- err
        }
        close(errCh)
    }()

    select {
    case <-ctx.Done():
        shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        httpSrv.Shutdown(shutCtx)
        return nil
    case err := <-errCh:
        return err
    }
}
```

The `nodeRegisterHandler` in `internal/relay/relay.go`:
```go
func nodeRegisterHandler(st store.Store) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var req struct {
            NodeName string `json:"node_name"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeName == "" {
            http.Error(w, "node_name required", http.StatusBadRequest)
            return
        }

        // Generate a random node token.
        b := make([]byte, 32)
        rand.Read(b)
        token := fmt.Sprintf("%x", b)

        auth := oauth.GetAuth(r.Context())
        var githubID *int64
        if auth != nil && auth.UserID != 0 {
            githubID = &auth.UserID
        }

        node := store.NodeRecord{
            Name:         req.NodeName,
            Token:        token,
            GitHubID:     githubID,
            AuthorizedAt: time.Now().UTC(),
            LastSeenAt:   time.Now().UTC(),
        }
        if err := st.NodeRegister(r.Context(), node); err != nil {
            http.Error(w, "internal error", http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]string{
            "status":     "registered",
            "node_token": token,
            "node_name":  req.NodeName,
        })
    }
}
```

Move helper functions (rateLimiter, inviteHandlers, kvHandlers, etc.) from `tunnel/relay.go` to `relay/relay.go`. Keep `internal/tunnel/relay.go` compiling by having it delegate to the new package (or just update `cmd/cw/main.go` to call `relay.RunRelay` directly).

**Step 2: Build**

```
make build
```
Fix any compile errors.

**Step 3: Integration test**

```
go test ./tests/... -tags integration -v
```
Expected: all pass

**Step 4: Commit**

```bash
git add internal/relay/ cmd/
git commit -m "feat(relay): implement new RunRelay without WireGuard, with SSH server and node hub"
```

---

## Task 9: Update setup flow

**Files:**
- Create: `internal/relay/setup.go` (new setup, no WireGuard)

**Context:** `cw setup relay.codespace.sh --token <admintoken>` now registers the node name, receives a node token, and writes `relay_url` + `relay_token` to config.

**Step 1: Implement `internal/relay/setup.go`**

```go
package relay

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"

    "github.com/BurntSushi/toml"
    "github.com/codewiresh/codewire/internal/config"
)

type SetupOptions struct {
    RelayURL    string
    DataDir     string
    InviteToken string
    AuthToken   string
}

// RunSetup registers this node with the relay and writes config.
// It supports two modes: invite token or admin token.
func RunSetup(ctx context.Context, opts SetupOptions) error {
    cfg, _ := config.LoadConfig(opts.DataDir)
    nodeName := "codewire"
    if cfg != nil && cfg.Node.Name != "" {
        nodeName = cfg.Node.Name
    }

    var nodeToken string
    var err error

    if opts.InviteToken != "" {
        nodeToken, err = registerWithInvite(ctx, opts.RelayURL, nodeName, opts.InviteToken)
    } else if opts.AuthToken != "" {
        nodeToken, err = registerWithToken(ctx, opts.RelayURL, nodeName, opts.AuthToken)
    } else {
        return fmt.Errorf("provide --token or --invite to register with relay")
    }

    if err != nil {
        return err
    }

    fmt.Fprintf(os.Stderr, "→ Registered node %q\n", nodeName)

    if err := writeRelayConfig(opts.DataDir, opts.RelayURL, nodeToken); err != nil {
        return fmt.Errorf("writing config: %w", err)
    }
    fmt.Fprintln(os.Stderr, "→ Configuration saved.")
    fmt.Fprintf(os.Stderr, "→ Start node: cw node -d\n")
    return nil
}

func registerWithToken(ctx context.Context, relayURL, nodeName, adminToken string) (string, error) {
    reqBody, _ := json.Marshal(map[string]string{"node_name": nodeName})
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/api/v1/nodes", bytes.NewReader(reqBody))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+adminToken)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return "", fmt.Errorf("contacting relay: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return "", fmt.Errorf("registration failed (%d): %s", resp.StatusCode, body)
    }

    var result struct {
        NodeToken string `json:"node_token"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    return result.NodeToken, nil
}

func registerWithInvite(ctx context.Context, relayURL, nodeName, inviteToken string) (string, error) {
    reqBody, _ := json.Marshal(map[string]string{
        "node_name":    nodeName,
        "invite_token": inviteToken,
    })
    resp, err := http.Post(relayURL+"/api/v1/join", "application/json", bytes.NewReader(reqBody))
    if err != nil {
        return "", fmt.Errorf("contacting relay: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return "", fmt.Errorf("invite rejected (%d): %s", resp.StatusCode, body)
    }

    var result struct {
        NodeToken string `json:"node_token"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    return result.NodeToken, nil
}

func writeRelayConfig(dataDir, relayURL, nodeToken string) error {
    configPath := dataDir + "/config.toml"
    cfg := &config.Config{}
    toml.DecodeFile(configPath, cfg)

    cfg.RelayURL = &relayURL
    cfg.RelayToken = &nodeToken

    f, err := os.Create(configPath)
    if err != nil {
        return err
    }
    defer f.Close()
    return toml.NewEncoder(f).Encode(cfg)
}
```

Update the `joinHandler` in `relay/relay.go` to also return `node_token` (same as register).

**Step 2: Build**

```
make build
```

**Step 3: Commit**

```bash
git add internal/relay/setup.go
git commit -m "feat(relay): new setup flow issues node token, no WireGuard"
```

---

## Task 10: Update node.go — use relay agent instead of WireGuard tunnel

**Files:**
- Modify: `internal/node/node.go`

**Step 1: Replace tunnel import with relay agent**

In `Run()`, replace:
```go
// Start WireGuard tunnel if relay URL is configured.
if n.config.RelayURL != nil {
    go func() {
        tun, err := tunnel.Connect(ctx, *n.config.RelayURL, n.dataDir)
        if err != nil {
            slog.Error("tunnel connection failed", "err", err)
            return
        }
        defer tun.Close()
        slog.Info("tunnel connected", "url", tun.URL())
        if listener := tun.Listener(); listener != nil {
            n.runWSServerOnListener(ctx, listener)
        }
    }()
}
```
With:
```go
// Start relay agent if relay URL and token are configured.
if n.config.RelayURL != nil && n.config.RelayToken != nil {
    go relay.RunAgent(ctx, relay.AgentConfig{
        RelayURL:  *n.config.RelayURL,
        NodeName:  n.config.Node.Name,
        NodeToken: *n.config.RelayToken,
    })
}
```

Update imports: remove `"github.com/codewiresh/codewire/internal/tunnel"`, add `"github.com/codewiresh/codewire/internal/relay"`.

Remove `runWSServerOnListener` method if it's only used for the tunnel listener.

**Step 2: Build**

```
make build
```
Fix any compile errors.

**Step 3: Run all tests**

```
go test ./internal/... ./tests/... -tags integration -timeout 120s -count=1
```
Expected: all pass

**Step 4: Commit**

```bash
git add internal/node/node.go
git commit -m "feat(node): use relay agent instead of WireGuard tunnel"
```

---

## Task 11: Update cmd/cw/main.go

**Files:**
- Modify: `cmd/cw/main.go`

**Context:** The `relayCmd` currently passes `WireguardEndpoint`, `WireguardPort` to `tunnel.RunRelay`. Update to call `relay.RunRelay` with the new `RelayConfig`. The `setupCmd` calls `tunnel.RunSetup`; update to call `relay.RunSetup`.

**Step 1: Update relay command**

Find `relayCmd` in `cmd/cw/main.go`. Replace `tunnel.RunRelay(ctx, tunnel.RelayConfig{...})` with `relay.RunRelay(ctx, relay.RelayConfig{...})`.

Remove flags: `--wg-endpoint`, `--wg-port`.
Add flag: `--ssh-listen` (default `:2222`).

**Step 2: Update setup command**

Replace `tunnel.RunSetup(ctx, tunnel.SetupOptions{...})` with `relay.RunSetup(ctx, relay.SetupOptions{...})`.

**Step 3: Build**

```
make build
```

**Step 4: Smoke test**

```bash
./cw relay --help  # should show --ssh-listen, not --wg-*
./cw setup --help  # should work
```

**Step 5: Commit**

```bash
git add cmd/cw/main.go
git commit -m "feat(cmd): update relay/setup commands to use new relay package"
```

---

## Task 12: Remove coder/wgtunnel

**Files:**
- Delete: `internal/tunnel/tunnel.go`
- Delete: `internal/tunnel/keys.go`
- Keep (for reference during cleanup): `internal/tunnel/relay.go`, `internal/tunnel/setup.go` — these will be deleted once `internal/relay/` fully replaces them
- Delete: `internal/tunnel/relay.go`, `internal/tunnel/setup.go`
- Modify: `go.mod` + `go.sum`

**Step 1: Delete WireGuard-specific files**

```bash
rm internal/tunnel/tunnel.go
rm internal/tunnel/keys.go
rm internal/tunnel/relay.go
rm internal/tunnel/setup.go
rmdir internal/tunnel  # if now empty
```

**Step 2: Update go.mod**

```bash
go mod tidy
```
This removes `github.com/coder/wgtunnel` and its transitive deps (wireguard-go, gvisor, etc.).

**Step 3: Build and fix**

```
make build
```
Fix any remaining references to `internal/tunnel`.

**Step 4: Run all tests**

```
go test ./internal/... ./tests/... -timeout 120s -count=1
```
Expected: all pass

**Step 5: Commit**

```bash
git add -A
git commit -m "chore: remove coder/wgtunnel and internal/tunnel package"
```

---

## Task 13: End-to-end integration test

**Files:**
- Create: `tests/relay_e2e_test.go`

**Context:** Full round-trip: relay starts → node agent connects → SSH client connects → shell responds.

**Step 1: Write the test**

```go
//go:build integration
package tests

import (
    "bytes"
    "context"
    "net"
    "net/http/httptest"
    "testing"
    "time"

    "golang.org/x/crypto/ssh"

    localrelay "github.com/codewiresh/codewire/internal/relay"
    "github.com/codewiresh/codewire/internal/store"
)

func TestRelayE2E(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Setup relay components.
    st, _ := store.NewSQLiteStore(t.TempDir())
    _ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

    hub := localrelay.NewNodeHub()
    sessions := localrelay.NewPendingSessions()

    // HTTP server (node connect + back endpoints).
    httpMux := localrelay.BuildRelayMux(hub, sessions, st)
    httpSrv := httptest.NewServer(httpMux)
    defer httpSrv.Close()

    // SSH server.
    sshSrv, _ := localrelay.NewSSHServer(st, hub, sessions)
    sshLn, _ := net.Listen("tcp", "127.0.0.1:0")
    go sshSrv.Serve(ctx, sshLn)

    // Node agent connects.
    go localrelay.RunAgent(ctx, localrelay.AgentConfig{
        RelayURL:  httpSrv.URL,
        NodeName:  "n1",
        NodeToken: "tok1",
    })

    // Wait for agent.
    time.Sleep(200 * time.Millisecond)

    // SSH client connects.
    sshCfg := &ssh.ClientConfig{
        User:            "n1",
        Auth:            []ssh.AuthMethod{ssh.Password("tok1")},
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
        Timeout:         5 * time.Second,
    }
    client, err := ssh.Dial("tcp", sshLn.Addr().String(), sshCfg)
    if err != nil {
        t.Fatalf("ssh dial: %v", err)
    }
    defer client.Close()

    sess, _ := client.NewSession()
    defer sess.Close()

    sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{})

    var stdout bytes.Buffer
    sess.Stdout = &stdout
    sess.Stdin = bytes.NewBufferString("echo hello-relay\nexit\n")
    _ = sess.Run("")

    if !bytes.Contains(stdout.Bytes(), []byte("hello-relay")) {
        t.Fatalf("expected hello-relay in output, got: %q", stdout.String())
    }
}
```

Add `BuildRelayMux` helper to `internal/relay/relay.go`:
```go
// BuildRelayMux creates an HTTP mux with node agent endpoints.
// Used in tests; in production RunRelay sets up the full mux.
func BuildRelayMux(hub *NodeHub, sessions *PendingSessions, st store.Store) http.Handler {
    mux := http.NewServeMux()
    RegisterNodeConnectHandler(mux, hub, st)
    RegisterBackHandler(mux, sessions, st)
    return mux
}
```

**Step 2: Run**

```
go test ./tests/... -run TestRelayE2E -tags integration -v
```
Expected: PASS (shell echoes "hello-relay")

**Step 3: Run full suite**

```
go test ./internal/... ./tests/... -timeout 120s -count=1
```
Expected: all pass

**Step 4: Commit**

```bash
git add tests/
git commit -m "test: add E2E relay+SSH integration test"
```

---

## Task 14: Update docs

**Files:**
- Modify: `docs/llms.txt`
- Modify: `docs/llms-full.txt` (Section 9: Relay mode)
- Modify: `docs/quickstart.md`

**Step 1: Update llms.txt**

In the Optional section, replace the relay description:
```
- [Relay mode](https://codewire.sh/llms-full.txt): Run a persistent relay at
  relay.codespace.sh. Nodes connect outward via WSS. Users SSH in from anywhere
  (Termius, standard ssh client). No WireGuard, no wildcard DNS.
```

**Step 2: Update llms-full.txt Section 9 (Relay mode)**

Replace the entire relay section. Key content:
```
## 9. Relay mode

The hosted relay at relay.codespace.sh (or self-hosted) lets nodes connect from
anywhere and exposes them via SSH. Replaces Termius + tmux + Tailscale.

### Architecture
- Node connects outward to relay via persistent authenticated WebSocket
- Relay maintains an in-memory hub of connected nodes
- SSH users connect to relay:2222 with username=node-name, password=node-token
- Relay routes SSH sessions to the target node via a back-connection WebSocket

### Setup (node side)
```
cw setup https://relay.codespace.sh --token <admin-token>
cw node -d
```

### Connect from SSH client / Termius
```
ssh mynode@relay.codespace.sh -p 2222
Password: <node-token>
```

### Self-hosted relay
```
cw relay --base-url https://relay.example.com --data-dir /var/lib/codewire
```
Default ports: HTTP :8080, SSH :2222. Put nginx/Caddy in front for TLS.
```

**Step 3: Build**

```
make build
```
Expected: no errors

**Step 4: Commit**

```bash
git add docs/llms.txt docs/llms-full.txt docs/quickstart.md
git commit -m "docs: update relay docs for SSH gateway architecture"
```

---

## Verification

After all tasks complete:

```bash
# Build
make build
echo $?  # 0

# All tests
go test ./internal/... ./tests/... -timeout 120s -count=1
# Expected: PASS

# Manual smoke test (requires running relay)
cw relay --base-url http://localhost:8080 --data-dir /tmp/relay-test &
cw setup http://localhost:8080 --token test --data-dir /tmp/node1
cw node -d --data-dir /tmp/node1
ssh mynode@localhost -p 2222  # password: <printed token>
```

```bash
# go.mod should not contain coder/wgtunnel
grep wgtunnel go.mod  # should be empty
```
