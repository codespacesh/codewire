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
/// Default: Ctrl+B followed by 'd' (like tmux).
#[derive(Default)]
pub struct DetachDetector {
    saw_prefix: bool,
}

impl DetachDetector {
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed a byte. Returns (detach_detected, bytes_to_forward).
    pub fn feed(&mut self, byte: u8) -> (bool, Vec<u8>) {
        if self.saw_prefix {
            self.saw_prefix = false;
            if byte == b'd' {
                return (true, vec![]);
            }
            return (false, vec![0x02, byte]);
        }

        if byte == 0x02 {
            self.saw_prefix = true;
            return (false, vec![]);
        }

        (false, vec![byte])
    }

    /// Feed a buffer. Returns (detach_detected, bytes_to_forward).
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
}
