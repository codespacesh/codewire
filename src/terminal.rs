use std::io;
use std::os::fd::{AsRawFd, BorrowedFd};

use anyhow::{Context, Result};
use nix::sys::termios::{self, Termios};
use tokio::signal::unix::{signal, SignalKind};

/// Saved terminal state for restoring on exit.
pub struct RawModeGuard {
    original: Termios,
    fd: i32,
}

impl RawModeGuard {
    /// Put stdin into raw mode. Returns a guard that restores on drop.
    pub fn enable() -> Result<Self> {
        let fd = io::stdin().as_raw_fd();
        let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
        let original = termios::tcgetattr(borrowed).context("tcgetattr")?;

        let mut raw = original.clone();
        termios::cfmakeraw(&mut raw);
        termios::tcsetattr(borrowed, termios::SetArg::TCSANOW, &raw).context("tcsetattr raw")?;

        Ok(Self { original, fd })
    }

    /// Restore the terminal to its original state.
    pub fn restore(&self) {
        let borrowed = unsafe { BorrowedFd::borrow_raw(self.fd) };
        let _ = termios::tcsetattr(borrowed, termios::SetArg::TCSANOW, &self.original);
    }
}

impl Drop for RawModeGuard {
    fn drop(&mut self) {
        self.restore();
    }
}

/// Get the current terminal size (cols, rows).
pub fn terminal_size() -> Result<(u16, u16)> {
    let fd = io::stdout().as_raw_fd();
    let mut size = nix::libc::winsize {
        ws_row: 0,
        ws_col: 0,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let ret = unsafe { nix::libc::ioctl(fd, nix::libc::TIOCGWINSZ, &mut size) };
    if ret == -1 {
        anyhow::bail!("TIOCGWINSZ ioctl failed");
    }
    Ok((size.ws_col, size.ws_row))
}

/// Returns a stream that fires on SIGWINCH (terminal resize).
pub fn resize_signal() -> Result<tokio::signal::unix::Signal> {
    signal(SignalKind::window_change()).context("registering SIGWINCH handler")
}

/// Detach key sequence detector.
///
/// Recognises Ctrl+B followed by 'd' (like tmux). Handles two encodings of
/// Ctrl+B:
///
/// 1. **Legacy** — the single byte `0x02`.
/// 2. **Kitty keyboard protocol** — `\x1b[98;5u` (codepoint 98 = 'b',
///    modifier 5 = 1 + Ctrl). Per the Kitty spec, the codepoint is always
///    the unshifted key, not the control character value.
///
/// And two encodings of 'd':
///
/// 1. **Legacy** — the single byte `0x64`.
/// 2. **Kitty keyboard protocol** — `\x1b[100u` or `\x1b[100;1u`
///    (only with "report all keys" flag).
///
/// While waiting for 'd' after the prefix, terminal-injected escape sequences
/// (focus events `\x1b[I`/`\x1b[O`, cursor position reports `\x1b[R;CH`,
/// mouse reports, etc.) are buffered and forwarded without cancelling the
/// pending detach.
#[derive(Debug)]
pub struct DetachDetector {
    state: DetectState,
    /// Bytes buffered while parsing escapes (forwarded when the escape ends).
    buf: Vec<u8>,
    /// CSI parameter bytes accumulated from Normal-state Kitty detection.
    params: Vec<u8>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DetectState {
    /// Idle — looking for legacy 0x02 or the start of a Kitty CSI.
    Normal,
    /// Saw legacy 0x02 — waiting for 'd' (or escape sequence to skip).
    SawPrefix,
    /// Inside SawPrefix: saw `\x1b`, waiting for `[` or a 2-char escape final.
    SawPrefixEsc,
    /// Inside SawPrefix: saw `\x1b[`, consuming CSI parameter/final bytes.
    SawPrefixCsi,
    /// From Normal: saw `\x1b`, might be start of Kitty CSI.
    Esc,
    /// From Normal: saw `\x1b[`, accumulating CSI params.
    Csi,
}

impl Default for DetachDetector {
    fn default() -> Self {
        Self {
            state: DetectState::Normal,
            buf: Vec::new(),
            params: Vec::new(),
        }
    }
}

impl DetachDetector {
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed a single byte. Returns `(detach_detected, bytes_to_forward)`.
    pub fn feed(&mut self, byte: u8) -> (bool, Vec<u8>) {
        match self.state {
            // ----------------------------------------------------------
            // Normal: looking for legacy Ctrl+B (0x02) or Kitty CSI start
            // ----------------------------------------------------------
            DetectState::Normal => {
                if byte == 0x02 {
                    self.state = DetectState::SawPrefix;
                    return (false, vec![]);
                }
                if byte == 0x1b {
                    self.state = DetectState::Esc;
                    self.buf.clear();
                    self.buf.push(byte);
                    return (false, vec![]);
                }
                (false, vec![byte])
            }

            // ----------------------------------------------------------
            // SawPrefix: we have the Ctrl+B prefix, waiting for 'd'
            // ----------------------------------------------------------
            DetectState::SawPrefix => {
                if byte == b'd' {
                    self.state = DetectState::Normal;
                    return (true, vec![]);
                }
                if byte == 0x1b {
                    // Start of an escape sequence — buffer and skip it.
                    self.state = DetectState::SawPrefixEsc;
                    self.buf.clear();
                    self.buf.push(byte);
                    return (false, vec![]);
                }
                // Any other byte cancels the prefix.
                self.state = DetectState::Normal;
                (false, vec![0x02, byte])
            }

            // ----------------------------------------------------------
            // SawPrefixEsc: inside SawPrefix, saw \x1b
            // ----------------------------------------------------------
            DetectState::SawPrefixEsc => {
                self.buf.push(byte);
                if byte == b'[' {
                    self.state = DetectState::SawPrefixCsi;
                    return (false, vec![]);
                }
                // 2-char escape (e.g. \x1bO for focus-out, \x1bN, etc.).
                // Forward the buffered escape and stay in SawPrefix.
                self.state = DetectState::SawPrefix;
                let fwd = self.buf.drain(..).collect();
                (false, fwd)
            }

            // ----------------------------------------------------------
            // SawPrefixCsi: inside SawPrefix, consuming CSI sequence
            // ----------------------------------------------------------
            DetectState::SawPrefixCsi => {
                self.buf.push(byte);
                if (0x20..=0x3f).contains(&byte) {
                    // Parameter or intermediate byte — keep consuming.
                    return (false, vec![]);
                }
                if (0x40..=0x7e).contains(&byte) {
                    if byte == b'u' {
                        // Kitty key event — check if it's 'd' (codepoint 100).
                        // params are in buf[2..len-1] (after \x1b[, before final u)
                        if self.is_kitty_d() {
                            self.state = DetectState::Normal;
                            self.buf.clear();
                            return (true, vec![]);
                        }
                        // Some other Kitty key — cancel SawPrefix.
                        self.state = DetectState::Normal;
                        let mut fwd = vec![0x02];
                        fwd.extend(self.buf.drain(..));
                        return (false, fwd);
                    }
                    // Non-Kitty CSI (focus event, cursor report, mouse, etc.)
                    // Forward and stay in SawPrefix.
                    self.state = DetectState::SawPrefix;
                    let fwd = self.buf.drain(..).collect();
                    return (false, fwd);
                }
                // Unexpected byte — forward everything and cancel to Normal.
                self.state = DetectState::Normal;
                let mut fwd = vec![0x02];
                fwd.extend(self.buf.drain(..));
                (false, fwd)
            }

            // ----------------------------------------------------------
            // Esc: from Normal, saw \x1b — might be Kitty CSI
            // ----------------------------------------------------------
            DetectState::Esc => {
                self.buf.push(byte);
                if byte == b'[' {
                    self.state = DetectState::Csi;
                    self.params.clear();
                    return (false, vec![]);
                }
                // Not a CSI — forward the buffered bytes.
                self.state = DetectState::Normal;
                let fwd = self.buf.drain(..).collect();
                (false, fwd)
            }

            // ----------------------------------------------------------
            // Csi: from Normal, accumulating CSI params for Kitty check
            // ----------------------------------------------------------
            DetectState::Csi => {
                if (0x30..=0x3f).contains(&byte) {
                    // Parameter byte (digits, semicolons, etc.)
                    self.buf.push(byte);
                    self.params.push(byte);
                    return (false, vec![]);
                }
                if (0x20..=0x2f).contains(&byte) {
                    // Intermediate byte — not Kitty `u`, just buffer.
                    self.buf.push(byte);
                    return (false, vec![]);
                }
                if (0x40..=0x7e).contains(&byte) {
                    // Final byte.
                    self.buf.push(byte);
                    if byte == b'u' && self.is_kitty_ctrl_b() {
                        // Kitty Ctrl+B! Enter SawPrefix.
                        self.state = DetectState::SawPrefix;
                        self.buf.clear();
                        self.params.clear();
                        return (false, vec![]);
                    }
                    // Some other CSI — forward everything.
                    self.state = DetectState::Normal;
                    let fwd = self.buf.drain(..).collect();
                    self.params.clear();
                    return (false, fwd);
                }
                // Unexpected byte — forward and reset.
                self.buf.push(byte);
                self.state = DetectState::Normal;
                let fwd = self.buf.drain(..).collect();
                self.params.clear();
                (false, fwd)
            }
        }
    }

    /// Parse (codepoint, modifier) from a Kitty CSI u parameter byte slice.
    ///
    /// Format: `codepoint[:shifted][;modifier[:event_type][;text]]`
    /// Modifier defaults to 1 (no modifiers) when absent.
    fn parse_kitty(params: &[u8]) -> Option<(u32, u32)> {
        let s = std::str::from_utf8(params).ok()?;
        let mut parts = s.split(';');
        let codepoint = parts.next()?.split(':').next()?.parse::<u32>().ok()?;
        let modifier = parts
            .next()
            .and_then(|m| m.split(':').next())
            .and_then(|m| m.parse::<u32>().ok())
            .unwrap_or(1);
        Some((codepoint, modifier))
    }

    /// Check if `self.params` (from Csi state) is Kitty Ctrl+B.
    /// Codepoint 98 = 'b', modifier 5 = 1 + Ctrl.
    fn is_kitty_ctrl_b(&self) -> bool {
        matches!(Self::parse_kitty(&self.params), Some((98, 5)))
    }

    /// Check if `self.buf` (from SawPrefixCsi state) is Kitty 'd'.
    /// buf layout: `[\x1b, [, <params...>, u]`
    /// Codepoint 100 = 'd', modifier absent or 1 = no modifiers.
    fn is_kitty_d(&self) -> bool {
        if self.buf.len() < 4 {
            return false;
        }
        let params = &self.buf[2..self.buf.len() - 1];
        matches!(Self::parse_kitty(params), Some((100, 1)))
    }

    /// Feed a buffer. Returns `(detach_detected, bytes_to_forward)`.
    pub fn feed_buf(&mut self, buf: &[u8]) -> (bool, Vec<u8>) {
        let mut forward = Vec::with_capacity(buf.len());
        for &byte in buf {
            let (detach, bytes) = self.feed(byte);
            if detach {
                return (true, forward);
            }
            forward.extend_from_slice(&bytes);
        }
        (false, forward)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // --- Legacy detach (unchanged behaviour) ---

    #[test]
    fn detach_sequence() {
        let mut d = DetachDetector::new();
        assert_eq!(d.feed(0x02), (false, vec![]));
        assert_eq!(d.feed(b'd'), (true, vec![]));
    }

    #[test]
    fn not_detach_forwards_prefix() {
        let mut d = DetachDetector::new();
        assert_eq!(d.feed(0x02), (false, vec![]));
        assert_eq!(d.feed(b'x'), (false, vec![0x02, b'x']));
    }

    #[test]
    fn regular_bytes_pass_through() {
        let mut d = DetachDetector::new();
        assert_eq!(d.feed(b'a'), (false, vec![b'a']));
        assert_eq!(d.feed(b'b'), (false, vec![b'b']));
    }

    #[test]
    fn feed_buf_detach() {
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"hello\x02d");
        assert!(detach);
        assert_eq!(fwd, b"hello");
    }

    // --- Escape sequences interleaved with detach ---

    #[test]
    fn detach_with_interleaved_cursor_report() {
        // Ctrl+B, then cursor position report \x1b[24;80R, then 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[24;80Rd");
        assert!(detach, "should detect detach through cursor report");
        // The escape sequence should be forwarded
        assert_eq!(fwd, b"\x1b[24;80R");
    }

    #[test]
    fn detach_with_focus_event() {
        // Ctrl+B, then focus-in \x1b[I, then 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[Id");
        assert!(detach, "should detect detach through focus event");
        assert_eq!(fwd, b"\x1b[I");
    }

    #[test]
    fn detach_with_focus_out_event() {
        // Ctrl+B, then focus-out \x1b[O, then 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[Od");
        assert!(detach, "should detect detach through focus-out event");
        assert_eq!(fwd, b"\x1b[O");
    }

    #[test]
    fn detach_with_multiple_escape_sequences() {
        // Ctrl+B, focus-in, cursor report, then 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[I\x1b[24;80Rd");
        assert!(detach, "should detect detach through multiple escapes");
        assert_eq!(fwd, b"\x1b[I\x1b[24;80R");
    }

    #[test]
    fn detach_with_mouse_report() {
        // Ctrl+B, then SGR mouse report \x1b[<0;10;20M, then 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[<0;10;20Md");
        assert!(detach, "should detect detach through mouse report");
        assert_eq!(fwd, b"\x1b[<0;10;20M");
    }

    // --- Kitty keyboard protocol ---
    //
    // Per the Kitty spec (https://sw.kovidgoyal.net/kitty/keyboard-protocol/):
    //   Ctrl+B = \x1b[98;5u  (codepoint 98 = 'b', modifier 5 = 1 + Ctrl)
    //   'd'    = \x1b[100;1u (codepoint 100, modifier 1 = no modifiers)
    //            or \x1b[100u (modifier omitted = no modifiers)
    //            or raw 0x64 (when "report all keys" flag is not set)

    #[test]
    fn detach_kitty_ctrl_b_raw_d() {
        // Kitty Ctrl+B followed by raw 'd' (flag 1: only modified keys encoded)
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5ud");
        assert!(detach, "should detect detach with Kitty Ctrl+B + raw d");
        assert!(fwd.is_empty());
    }

    #[test]
    fn detach_kitty_ctrl_b_kitty_d() {
        // Both Ctrl+B and 'd' fully Kitty-encoded (flag 8: all keys encoded)
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[100;1u");
        assert!(detach, "should detect detach with fully Kitty-encoded sequence");
        assert!(fwd.is_empty());
    }

    #[test]
    fn detach_kitty_ctrl_b_kitty_d_no_modifier() {
        // Kitty 'd' without explicit modifier: \x1b[100u
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[100u");
        assert!(detach, "should detect detach with Kitty d (no modifier)");
        assert!(fwd.is_empty());
    }

    #[test]
    fn detach_legacy_ctrl_b_kitty_d() {
        // Legacy Ctrl+B (0x02) but Kitty 'd' (\x1b[100;1u)
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1b[100;1u");
        assert!(detach, "should detect detach with legacy prefix + Kitty d");
        assert!(fwd.is_empty());
    }

    #[test]
    fn kitty_ctrl_b_with_interleaved_escape() {
        // Kitty Ctrl+B, then a focus event, then raw 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[Id");
        assert!(detach, "should detach with Kitty prefix + escape + d");
        assert_eq!(fwd, b"\x1b[I");
    }

    #[test]
    fn kitty_ctrl_b_focus_then_kitty_d() {
        // Full Kitty mode: Ctrl+B, focus event, then Kitty 'd'
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[I\x1b[100;1u");
        assert!(detach, "should detach with Kitty prefix + focus + Kitty d");
        assert_eq!(fwd, b"\x1b[I");
    }

    #[test]
    fn kitty_non_ctrl_b_passes_through() {
        // \x1b[97;1u is Kitty encoding for 'a' (codepoint=97, modifier=1)
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[97;1u");
        assert!(!detach, "should not detach on non-Ctrl+B Kitty sequence");
        assert_eq!(fwd, b"\x1b[97;1u");
    }

    #[test]
    fn kitty_ctrl_b_then_kitty_non_d_cancels() {
        // Kitty Ctrl+B then Kitty 'x' (\x1b[120;1u) — should cancel
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[120;1u");
        assert!(!detach, "should cancel on Kitty non-d key after prefix");
        assert_eq!(fwd, b"\x02\x1b[120;1u");
    }

    #[test]
    fn kitty_ctrl_d_not_detach() {
        // Kitty Ctrl+B then Kitty Ctrl+d (\x1b[100;5u) — modifier 5 means Ctrl
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5u\x1b[100;5u");
        assert!(!detach, "Ctrl+d (modifier 5) should not trigger detach");
        assert_eq!(fwd, b"\x02\x1b[100;5u");
    }

    #[test]
    fn escape_in_normal_state_passes_through() {
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[6n");
        assert!(!detach);
        assert_eq!(fwd, b"\x1b[6n");
    }

    #[test]
    fn two_char_escape_in_saw_prefix() {
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x02\x1bNd");
        assert!(detach, "should detach after 2-char escape in SawPrefix");
        assert_eq!(fwd, b"\x1bN");
    }

    #[test]
    fn normal_text_between_detach_attempts() {
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"abc\x02d");
        assert!(detach);
        assert_eq!(fwd, b"abc");
    }

    #[test]
    fn kitty_ctrl_b_cancel_raw() {
        // Kitty Ctrl+B followed by raw non-'d' cancels
        let mut d = DetachDetector::new();
        let (detach, fwd) = d.feed_buf(b"\x1b[98;5ux");
        assert!(!detach);
        assert_eq!(fwd, b"\x02x");
    }
}
