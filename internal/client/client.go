package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"nhooyr.io/websocket"

	"github.com/codewiresh/codewire/internal/connection"
	"github.com/codewiresh/codewire/internal/protocol"
)

// Target describes where to connect: either a local Unix socket or a remote
// WebSocket endpoint.
type Target struct {
	Local string // dataDir path (empty if remote)
	URL   string // ws:// or wss:// URL for remote
	Token string // auth token for remote
}

// IsLocal returns true when the target is a local Unix socket connection.
func (t *Target) IsLocal() bool { return t.Local != "" }

// Connect establishes a connection to the target and returns a FrameReader
// and FrameWriter pair. The caller is responsible for closing both.
func (t *Target) Connect() (connection.FrameReader, connection.FrameWriter, error) {
	if t.IsLocal() {
		sockPath := filepath.Join(t.Local, "codewire.sock")
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to local socket: %w", err)
		}
		return connection.NewUnixReader(conn), connection.NewUnixWriter(conn), nil
	}

	// Determine WebSocket URL.
	wsURL := t.URL
	if strings.HasPrefix(wsURL, "https://") {
		// Relay URL: convert https:// → wss://
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
		if !strings.HasSuffix(wsURL, "/ws") {
			wsURL += "/ws"
		}
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
		if !strings.HasSuffix(wsURL, "/ws") {
			wsURL += "/ws"
		}
	} else if !strings.HasSuffix(wsURL, "/ws") {
		wsURL += "/ws"
	}

	// Send token via Authorization header only (not in URL query to avoid log exposure).
	ctx := context.Background()
	opts := &websocket.DialOptions{}
	if t.Token != "" {
		opts.HTTPHeader = make(map[string][]string)
		opts.HTTPHeader["Authorization"] = []string{"Bearer " + t.Token}
	}

	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to remote server: %w", err)
	}
	// Remove the default read limit so large frames are not rejected.
	conn.SetReadLimit(-1)
	return connection.NewWSReader(ctx, conn), connection.NewWSWriter(ctx, conn), nil
}

// requestResponse opens a connection, sends a single request, reads a single
// control frame response, and closes the connection. It is the building block
// for simple one-shot commands.
func requestResponse(target *Target, req *protocol.Request) (*protocol.Response, error) {
	reader, writer, err := target.Connect()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	defer writer.Close()

	if err := writer.SendRequest(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	frame, err := reader.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if frame == nil {
		return nil, fmt.Errorf("connection closed before response")
	}
	if frame.Type != protocol.FrameControl {
		return nil, fmt.Errorf("expected control frame, got type 0x%02x", frame.Type)
	}

	var resp protocol.Response
	if err := json.Unmarshal(frame.Payload, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &resp, nil
}

// formatError appends helpful hints to common error messages.
func formatError(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "not found") {
		return message + "\n\nUse 'cw list' to see active sessions"
	}
	if strings.Contains(lower, "not running") {
		return message + "\n\nUse 'cw status <id>' to check session status"
	}
	return message
}
