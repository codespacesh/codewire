package connection

import (
	"encoding/json"
	"net"
	"sync"

	"github.com/codespacesh/codewire/internal/protocol"
)

// UnixReader reads protocol frames from a Unix socket connection.
type UnixReader struct {
	conn net.Conn
}

// NewUnixReader creates a new UnixReader wrapping the given connection.
func NewUnixReader(conn net.Conn) *UnixReader {
	return &UnixReader{conn: conn}
}

// ReadFrame reads a single protocol frame from the underlying connection.
// Returns (nil, nil) on clean EOF.
func (r *UnixReader) ReadFrame() (*protocol.Frame, error) {
	return protocol.ReadFrame(r.conn)
}

// Close closes the underlying connection.
func (r *UnixReader) Close() error {
	return r.conn.Close()
}

// UnixWriter writes protocol frames to a Unix socket connection.
// It is safe for concurrent use.
type UnixWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

// NewUnixWriter creates a new UnixWriter wrapping the given connection.
func NewUnixWriter(conn net.Conn) *UnixWriter {
	return &UnixWriter{conn: conn}
}

// WriteFrame writes a single protocol frame to the underlying connection.
func (w *UnixWriter) WriteFrame(f *protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return protocol.WriteFrame(w.conn, f)
}

// SendResponse marshals a Response to JSON and sends it as a control frame.
func (w *UnixWriter) SendResponse(resp *protocol.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameControl, Payload: data})
}

// SendRequest marshals a Request to JSON and sends it as a control frame.
func (w *UnixWriter) SendRequest(req *protocol.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameControl, Payload: data})
}

// SendData sends raw bytes as a data frame.
func (w *UnixWriter) SendData(data []byte) error {
	return w.WriteFrame(&protocol.Frame{Type: protocol.FrameData, Payload: data})
}

// Close closes the underlying connection.
func (w *UnixWriter) Close() error {
	return w.conn.Close()
}
