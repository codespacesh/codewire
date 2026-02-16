package connection

import "github.com/codespacesh/codewire/internal/protocol"

// FrameReader reads protocol frames from a transport.
type FrameReader interface {
	ReadFrame() (*protocol.Frame, error)
	Close() error
}

// FrameWriter writes protocol frames to a transport.
type FrameWriter interface {
	WriteFrame(f *protocol.Frame) error
	SendResponse(resp *protocol.Response) error
	SendRequest(req *protocol.Request) error
	SendData(data []byte) error
	Close() error
}
