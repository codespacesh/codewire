package terminal

import (
	"strconv"
	"strings"
)

type detectState int

const (
	stateNormal detectState = iota
	stateSawPrefix
	stateSawPrefixEsc
	stateSawPrefixCsi
	stateEsc
	stateCsi
)

// DetachDetector recognises Ctrl+B followed by 'd' (like tmux). Handles two
// encodings of Ctrl+B:
//
//  1. Legacy -- the single byte 0x02.
//  2. Kitty keyboard protocol -- \x1b[98;5u (codepoint 98 = 'b',
//     modifier 5 = 1 + Ctrl).
//
// And two encodings of 'd':
//
//  1. Legacy -- the single byte 0x64.
//  2. Kitty keyboard protocol -- \x1b[100u or \x1b[100;1u.
//
// While waiting for 'd' after the prefix, terminal-injected escape sequences
// (focus events, cursor position reports, mouse reports, etc.) are buffered
// and forwarded without cancelling the pending detach.
type DetachDetector struct {
	state  detectState
	buf    []byte // buffered bytes during escape parsing
	params []byte // CSI parameter bytes for Kitty detection
}

func NewDetachDetector() *DetachDetector {
	return &DetachDetector{state: stateNormal}
}

// Feed processes a single byte. Returns (detachDetected, bytesToForward).
func (d *DetachDetector) Feed(b byte) (bool, []byte) {
	switch d.state {
	// ----------------------------------------------------------
	// Normal: looking for legacy Ctrl+B (0x02) or Kitty CSI start
	// ----------------------------------------------------------
	case stateNormal:
		if b == 0x02 {
			d.state = stateSawPrefix
			return false, nil
		}
		if b == 0x1b {
			d.state = stateEsc
			d.buf = d.buf[:0]
			d.buf = append(d.buf, b)
			return false, nil
		}
		return false, []byte{b}

	// ----------------------------------------------------------
	// SawPrefix: we have the Ctrl+B prefix, waiting for 'd'
	// ----------------------------------------------------------
	case stateSawPrefix:
		if b == 'd' {
			d.state = stateNormal
			return true, nil
		}
		if b == 0x1b {
			// Start of an escape sequence -- buffer and skip it.
			d.state = stateSawPrefixEsc
			d.buf = d.buf[:0]
			d.buf = append(d.buf, b)
			return false, nil
		}
		// Any other byte cancels the prefix.
		d.state = stateNormal
		return false, []byte{0x02, b}

	// ----------------------------------------------------------
	// SawPrefixEsc: inside SawPrefix, saw \x1b
	// ----------------------------------------------------------
	case stateSawPrefixEsc:
		d.buf = append(d.buf, b)
		if b == '[' {
			d.state = stateSawPrefixCsi
			return false, nil
		}
		// 2-char escape (e.g. \x1bO for focus-out, \x1bN, etc.).
		// Forward the buffered escape and stay in SawPrefix.
		d.state = stateSawPrefix
		fwd := make([]byte, len(d.buf))
		copy(fwd, d.buf)
		d.buf = d.buf[:0]
		return false, fwd

	// ----------------------------------------------------------
	// SawPrefixCsi: inside SawPrefix, consuming CSI sequence
	// ----------------------------------------------------------
	case stateSawPrefixCsi:
		d.buf = append(d.buf, b)
		if b >= 0x20 && b <= 0x3f {
			// Parameter or intermediate byte -- keep consuming.
			return false, nil
		}
		if b >= 0x40 && b <= 0x7e {
			// Final byte.
			if b == 'u' {
				// Kitty key event -- check if it's 'd' (codepoint 100).
				if d.isKittyD() {
					d.state = stateNormal
					d.buf = d.buf[:0]
					return true, nil
				}
				// Some other Kitty key -- cancel SawPrefix.
				d.state = stateNormal
				fwd := make([]byte, 1+len(d.buf))
				fwd[0] = 0x02
				copy(fwd[1:], d.buf)
				d.buf = d.buf[:0]
				return false, fwd
			}
			// Non-Kitty CSI (focus event, cursor report, mouse, etc.)
			// Forward and stay in SawPrefix.
			d.state = stateSawPrefix
			fwd := make([]byte, len(d.buf))
			copy(fwd, d.buf)
			d.buf = d.buf[:0]
			return false, fwd
		}
		// Unexpected byte -- forward everything and cancel to Normal.
		d.state = stateNormal
		fwd := make([]byte, 1+len(d.buf))
		fwd[0] = 0x02
		copy(fwd[1:], d.buf)
		d.buf = d.buf[:0]
		return false, fwd

	// ----------------------------------------------------------
	// Esc: from Normal, saw \x1b -- might be Kitty CSI
	// ----------------------------------------------------------
	case stateEsc:
		d.buf = append(d.buf, b)
		if b == '[' {
			d.state = stateCsi
			d.params = d.params[:0]
			return false, nil
		}
		// Not a CSI -- forward the buffered bytes.
		d.state = stateNormal
		fwd := make([]byte, len(d.buf))
		copy(fwd, d.buf)
		d.buf = d.buf[:0]
		return false, fwd

	// ----------------------------------------------------------
	// Csi: from Normal, accumulating CSI params for Kitty check
	// ----------------------------------------------------------
	case stateCsi:
		if b >= 0x30 && b <= 0x3f {
			// Parameter byte (digits, semicolons, etc.)
			d.buf = append(d.buf, b)
			d.params = append(d.params, b)
			return false, nil
		}
		if b >= 0x20 && b <= 0x2f {
			// Intermediate byte -- not Kitty 'u', just buffer.
			d.buf = append(d.buf, b)
			return false, nil
		}
		if b >= 0x40 && b <= 0x7e {
			// Final byte.
			d.buf = append(d.buf, b)
			if b == 'u' && d.isKittyCtrlB() {
				// Kitty Ctrl+B! Enter SawPrefix.
				d.state = stateSawPrefix
				d.buf = d.buf[:0]
				d.params = d.params[:0]
				return false, nil
			}
			// Some other CSI -- forward everything.
			d.state = stateNormal
			fwd := make([]byte, len(d.buf))
			copy(fwd, d.buf)
			d.buf = d.buf[:0]
			d.params = d.params[:0]
			return false, fwd
		}
		// Unexpected byte -- forward and reset.
		d.buf = append(d.buf, b)
		d.state = stateNormal
		fwd := make([]byte, len(d.buf))
		copy(fwd, d.buf)
		d.buf = d.buf[:0]
		d.params = d.params[:0]
		return false, fwd
	}

	// Unreachable, but satisfy the compiler.
	return false, []byte{b}
}

// FeedBuf processes a buffer. Returns (detachDetected, bytesToForward).
func (d *DetachDetector) FeedBuf(buf []byte) (bool, []byte) {
	forward := make([]byte, 0, len(buf))
	for _, b := range buf {
		detach, bytes := d.Feed(b)
		if detach {
			return true, forward
		}
		forward = append(forward, bytes...)
	}
	return false, forward
}

// parseKitty parses (codepoint, modifier) from Kitty CSI u params.
// Format: "codepoint[:shifted][;modifier[:event_type][;text]]"
// Modifier defaults to 1 when absent.
func parseKitty(params []byte) (codepoint, modifier uint32, ok bool) {
	s := string(params)
	parts := strings.SplitN(s, ";", 3)
	cpStr := strings.SplitN(parts[0], ":", 2)[0]
	cp, err := strconv.ParseUint(cpStr, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	mod := uint32(1) // default modifier
	if len(parts) >= 2 {
		modStr := strings.SplitN(parts[1], ":", 2)[0]
		m, err := strconv.ParseUint(modStr, 10, 32)
		if err != nil {
			return 0, 0, false
		}
		mod = uint32(m)
	}
	return uint32(cp), mod, true
}

// isKittyCtrlB checks if params represent Kitty Ctrl+B (codepoint 98, modifier 5).
func (d *DetachDetector) isKittyCtrlB() bool {
	cp, mod, ok := parseKitty(d.params)
	return ok && cp == 98 && mod == 5
}

// isKittyD checks if buf contains Kitty 'd' (codepoint 100, modifier 1).
// buf layout: [\x1b, [, <params...>, u]
func (d *DetachDetector) isKittyD() bool {
	if len(d.buf) < 4 {
		return false
	}
	params := d.buf[2 : len(d.buf)-1]
	cp, mod, ok := parseKitty(params)
	return ok && cp == 100 && mod == 1
}
