package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"nhooyr.io/websocket"

	"github.com/codespacesh/codewire/internal/protocol"
)

// WSReader reads protocol frames from a WebSocket connection.
// Control frames map to Text messages, data frames map to Binary messages,
// matching the Rust WebSocket transport.
type WSReader struct {
	conn *websocket.Conn
	ctx  context.Context
}

// NewWSReader creates a new WSReader wrapping the given WebSocket connection.
func NewWSReader(ctx context.Context, conn *websocket.Conn) *WSReader {
	return &WSReader{conn: conn, ctx: ctx}
}

// ReadFrame reads a single protocol frame from the WebSocket.
// Text messages become control frames, binary messages become data frames.
// Returns (nil, nil) on normal close, matching the Rust EOF convention.
func (r *WSReader) ReadFrame() (*protocol.Frame, error) {
	msgType, data, err := r.conn.Read(r.ctx)
	if err != nil {
		// Normal close is treated as a clean EOF — return nil, nil.
		var closeErr websocket.CloseError
		if errors.As(err, &closeErr) {
			return nil, nil
		}
		return nil, err
	}

	switch msgType {
	case websocket.MessageText:
		return &protocol.Frame{Type: protocol.FrameControl, Payload: data}, nil
	case websocket.MessageBinary:
		return &protocol.Frame{Type: protocol.FrameData, Payload: data}, nil
	default:
		return nil, fmt.Errorf("unexpected websocket message type: %d", msgType)
	}
}

// Close sends a normal closure message and closes the WebSocket.
func (r *WSReader) Close() error {
	return r.conn.Close(websocket.StatusNormalClosure, "")
}

// WSWriter writes protocol frames to a WebSocket connection.
// It is safe for concurrent use.
type WSWriter struct {
	conn *websocket.Conn
	ctx  context.Context
	mu   sync.Mutex
}

// NewWSWriter creates a new WSWriter wrapping the given WebSocket connection.
func NewWSWriter(ctx context.Context, conn *websocket.Conn) *WSWriter {
	return &WSWriter{conn: conn, ctx: ctx, mu: sync.Mutex{}}
}

// WriteFrame writes a single protocol frame to the WebSocket.
// Control frames are sent as text messages, data frames as binary messages.
func (w *WSWriter) WriteFrame(f *protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch f.Type {
	case protocol.FrameControl:
		return w.conn.Write(w.ctx, websocket.MessageText, f.Payload)
	case protocol.FrameData:
		return w.conn.Write(w.ctx, websocket.MessageBinary, f.Payload)
	default:
		return fmt.Errorf("unknown frame type: %d", f.Type)
	}
}

// SendResponse marshals a Response to JSON and sends it as a control frame.
func (w *WSWriter) SendResponse(resp *protocol.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameControl, Payload: data})
}

// SendRequest marshals a Request to JSON and sends it as a control frame.
func (w *WSWriter) SendRequest(req *protocol.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameControl, Payload: data})
}

// SendData sends raw bytes as a data frame.
func (w *WSWriter) SendData(data []byte) error {
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameData, Payload: data})
}

// Close sends a normal closure message and closes the WebSocket.
func (w *WSWriter) Close() error {
	return w.conn.Close(websocket.StatusNormalClosure, "")
}
