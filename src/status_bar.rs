use std::io::Write;
use std::time::Instant;

/// Tmux-style status bar drawn on the last terminal row.
///
/// Reports a reduced PTY size so the child process sees (cols, rows-1),
/// then redraws a reverse-video status line on the last row after each
/// data frame. No scroll region or alternate screen — content scrolls
/// naturally into the terminal's scrollback buffer.
pub struct StatusBar {
    session_id: u32,
    status: String,
    started: Instant,
    rows: u16,
    cols: u16,
    enabled: bool,
}

impl StatusBar {
    pub fn new(session_id: u32, cols: u16, rows: u16) -> Self {
        Self {
            session_id,
            status: "running".into(),
            started: Instant::now(),
            rows,
            cols,
            enabled: rows >= 5,
        }
    }

    /// Returns the PTY size to report to the node.
    /// One row shorter when the status bar is enabled.
    pub fn pty_size(&self) -> (u16, u16) {
        if self.enabled {
            (self.cols, self.rows - 1)
        } else {
            (self.cols, self.rows)
        }
    }

    /// Draw the initial status bar on the last row.
    pub fn setup(&self) -> Vec<u8> {
        if !self.enabled {
            return Vec::new();
        }
        self.draw()
    }

    /// Clean up terminal state: restore cursor visibility and reset terminal
    /// modes the child process may have enabled, then clear the status bar.
    ///
    /// Mode resets are always emitted (even when the status bar is disabled due
    /// to a small terminal) because the child process can hide the cursor or
    /// enable Kitty keyboard / mouse / focus modes regardless of bar state.
    pub fn teardown(&self) -> Vec<u8> {
        let mut out = Vec::new();

        // Exit alternate screen if the child process entered it (harmless no-op otherwise)
        out.extend_from_slice(b"\x1b[?1049l");
        // Show cursor (child may have hidden it with \x1b[?25l)
        out.extend_from_slice(b"\x1b[?25h");
        // Pop Kitty keyboard mode (child may have pushed with \x1b[>Nu)
        out.extend_from_slice(b"\x1b[<u");
        // Disable focus event reporting
        out.extend_from_slice(b"\x1b[?1004l");
        // Disable mouse tracking
        out.extend_from_slice(b"\x1b[?1000l");
        // Disable SGR mouse encoding
        out.extend_from_slice(b"\x1b[?1006l");

        // Bar-specific cleanup: clear the status bar line
        if self.enabled {
            // Save cursor
            out.extend_from_slice(b"\x1b7");
            // Move to status bar row and clear it
            let _ = write!(out, "\x1b[{};1H", self.rows);
            out.extend_from_slice(b"\x1b[2K");
            // Restore cursor
            out.extend_from_slice(b"\x1b8");
        }

        out
    }

    /// Draw the status bar (save cursor, render, restore cursor).
    pub fn draw(&self) -> Vec<u8> {
        if !self.enabled {
            return Vec::new();
        }
        let elapsed = self.started.elapsed();
        let age = format_duration(elapsed.as_secs());

        let content = format!(
            " [cw] session {} | {} | {} | Ctrl+B d",
            self.session_id, self.status, age
        );

        // Pad or truncate to fill the row
        let cols = self.cols as usize;
        let padded = if content.len() >= cols {
            content[..cols].to_string()
        } else {
            format!("{:<width$}", content, width = cols)
        };

        let mut out = Vec::new();
        // Save cursor
        out.extend_from_slice(b"\x1b7");
        // Move to status bar row (last row)
        let _ = write!(out, "\x1b[{};1H", self.rows);
        // Reverse video + write content + reset
        let _ = write!(out, "\x1b[7m{}\x1b[0m", padded);
        // Restore cursor
        out.extend_from_slice(b"\x1b8");
        out
    }

    /// Handle terminal resize: update dimensions and redraw.
    pub fn resize(&mut self, cols: u16, rows: u16) -> Vec<u8> {
        self.cols = cols;
        self.rows = rows;
        self.enabled = rows >= 5;

        if !self.enabled {
            return Vec::new();
        }

        self.draw()
    }
}

fn format_duration(secs: u64) -> String {
    if secs < 60 {
        format!("{}s", secs)
    } else if secs < 3600 {
        format!("{}m", secs / 60)
    } else {
        format!("{}h{}m", secs / 3600, (secs % 3600) / 60)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pty_size_reduces_rows() {
        let bar = StatusBar::new(1, 80, 24);
        assert_eq!(bar.pty_size(), (80, 23));
    }

    #[test]
    fn pty_size_full_when_disabled() {
        let bar = StatusBar::new(1, 80, 4);
        assert!(!bar.enabled);
        assert_eq!(bar.pty_size(), (80, 4));
    }

    #[test]
    fn setup_draws_bar_without_scroll_region() {
        let bar = StatusBar::new(1, 80, 24);
        let out = String::from_utf8(bar.setup()).unwrap();
        // No alternate screen or scroll region
        assert!(!out.contains("\x1b[?1049h"));
        assert!(!out.contains("\x1b[1;23r"));
        // Draws the bar
        assert!(out.contains("\x1b[7m")); // reverse video
        assert!(out.contains("session 1"));
    }

    #[test]
    fn teardown_exits_alt_screen_and_clears_bar() {
        let bar = StatusBar::new(1, 80, 24);
        let out = String::from_utf8(bar.teardown()).unwrap();
        // Exits alternate screen (child process may have entered it)
        assert!(out.contains("\x1b[?1049l"));
        // Shows cursor
        assert!(out.contains("\x1b[?25h"));
        // Pops Kitty keyboard mode
        assert!(out.contains("\x1b[<u"));
        // Disables focus events, mouse tracking, SGR mouse
        assert!(out.contains("\x1b[?1004l"));
        assert!(out.contains("\x1b[?1000l"));
        assert!(out.contains("\x1b[?1006l"));
        // Clears the last row
        assert!(out.contains("\x1b[24;1H")); // move to last row
        assert!(out.contains("\x1b[2K")); // clear line
    }

    #[test]
    fn draw_contains_session_info() {
        let bar = StatusBar::new(42, 80, 24);
        let out = String::from_utf8(bar.draw()).unwrap();
        assert!(out.contains("session 42"));
        assert!(out.contains("running"));
        assert!(out.contains("Ctrl+B d"));
        assert!(out.contains("\x1b[7m")); // reverse video
    }

    #[test]
    fn disabled_produces_empty_setup_and_draw() {
        let bar = StatusBar::new(1, 80, 3);
        assert!(bar.setup().is_empty());
        assert!(bar.draw().is_empty());
    }

    #[test]
    fn disabled_teardown_still_resets_modes() {
        let bar = StatusBar::new(1, 80, 3);
        assert!(!bar.enabled);
        let out = String::from_utf8(bar.teardown()).unwrap();
        // Mode resets are always emitted regardless of bar state
        assert!(out.contains("\x1b[?25h"), "should show cursor");
        assert!(out.contains("\x1b[<u"), "should pop Kitty keyboard");
        assert!(out.contains("\x1b[?1004l"), "should disable focus events");
        assert!(out.contains("\x1b[?1000l"), "should disable mouse tracking");
        assert!(out.contains("\x1b[?1006l"), "should disable SGR mouse");
        // But should NOT contain bar-specific cleanup
        assert!(!out.contains("\x1b[2K"), "should not clear bar line when disabled");
    }

    #[test]
    fn format_duration_display() {
        assert_eq!(format_duration(0), "0s");
        assert_eq!(format_duration(45), "45s");
        assert_eq!(format_duration(60), "1m");
        assert_eq!(format_duration(300), "5m");
        assert_eq!(format_duration(3661), "1h1m");
    }
}
