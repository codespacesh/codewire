package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame type constants matching the Rust wire format.
const (
	FrameControl byte   = 0x00
	FrameData    byte   = 0x01
	MaxPayload   uint32 = 16 * 1024 * 1024 // 16 MB
)

// Frame represents a wire-protocol frame with a type byte and payload.
// Wire format: [type:u8][length:u32 BE][payload]
type Frame struct {
	Type    byte
	Payload []byte
}

// ReadFrame reads a single frame from the reader.
// Returns (nil, nil) on clean EOF during the header read.
func ReadFrame(r io.Reader) (*Frame, error) {
	var header [5]byte
	_, err := io.ReadFull(r, header[:])
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, nil
		}
		return nil, fmt.Errorf("reading frame header: %w", err)
	}

	frameType := header[0]
	length := binary.BigEndian.Uint32(header[1:5])

	if length > MaxPayload {
		return nil, fmt.Errorf("frame payload too large: %d bytes", length)
	}

	payload := make([]byte, length)
	if length > 0 {
		_, err = io.ReadFull(r, payload)
		if err != nil {
			return nil, fmt.Errorf("reading frame payload: %w", err)
		}
	}

	switch frameType {
	case FrameControl, FrameData:
		return &Frame{Type: frameType, Payload: payload}, nil
	default:
		return nil, fmt.Errorf("unknown frame type: 0x%02x", frameType)
	}
}

// WriteFrame writes a single frame to the writer.
func WriteFrame(w io.Writer, f *Frame) error {
	var header [5]byte
	header[0] = f.Type
	binary.BigEndian.PutUint32(header[1:5], uint32(len(f.Payload)))

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("writing frame payload: %w", err)
		}
	}
	return nil
}
