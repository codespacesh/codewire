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
	defer ws.CloseNow()

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
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	// Dial back-connection to relay.
	backURL := toWS(cfg.RelayURL) + "/node/back/" + msg.SessionID
	ws, _, err := websocket.Dial(ctx, backURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.NodeToken}},
	})
	if err != nil {
		slog.Error("relay agent: back-connect failed", "err", err, "session", msg.SessionID)
		return
	}
	defer ws.CloseNow()

	// Use a cancellable child context: when bash exits (ptmx→nc copy ends),
	// cancel the context to unblock nc.Read() in the other direction.
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	nc := websocket.NetConn(childCtx, ws, websocket.MessageBinary)
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

	// ptmx→nc: send bash output to relay. When bash exits (ptmx EOF/EIO),
	// cancel the child context to unblock nc.Read() in the other direction.
	go func() {
		io.Copy(nc, ptmx)
		cancel()
	}()

	// nc→ptmx: relay stdin to bash. Returns when nc.Read() returns error
	// (either relay closed the connection or childCtx was cancelled above).
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
