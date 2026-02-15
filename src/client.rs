use std::path::PathBuf;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::connection::{FrameReader, FrameWriter};
use crate::protocol::{self, Frame, Request, Response, SessionInfo};
use crate::status_bar::StatusBar;
use crate::terminal::{resize_signal, terminal_size, DetachDetector, RawModeGuard};

// ---------------------------------------------------------------------------
// Connection target — local node or remote server
// ---------------------------------------------------------------------------

/// Where to connect — local node or remote server.
#[derive(Clone)]
pub enum Target {
    Local(PathBuf),
    Remote { url: String, token: String },
}

impl Target {
    /// Connect and return (reader, writer) pair.
    pub async fn connect(&self) -> Result<(FrameReader, FrameWriter)> {
        match self {
            Target::Local(data_dir) => {
                let sock = data_dir.join("server.sock");
                let stream = tokio::net::UnixStream::connect(&sock)
                    .await
                    .with_context(|| {
                        format!(
                            "Failed to connect to CodeWire node at {}\nThe node may not be running. It should auto-start on first command.",
                            sock.display()
                        )
                    })?;
                let (r, w) = stream.into_split();
                Ok((FrameReader::Unix(r), FrameWriter::Unix(w)))
            }
            #[cfg(feature = "ws")]
            Target::Remote { url, token } => {
                use tokio_tungstenite::connect_async;
                let ws_url = format!("{}/ws?token={}", url, token);
                let (ws, _response) = connect_async(&ws_url)
                    .await
                    .with_context(|| format!("connecting to {}", url))?;
                let (ws_writer, ws_reader) = futures::StreamExt::split(ws);
                Ok((
                    FrameReader::WsClient(ws_reader),
                    FrameWriter::WsClient(ws_writer),
                ))
            }
            #[cfg(not(feature = "ws"))]
            Target::Remote { .. } => {
                bail!("Remote connections require the 'ws' feature")
            }
        }
    }
}

/// Format error message with helpful context
fn format_error(message: &str) -> String {
    if message.contains("not found") {
        format!("{}\n\nUse 'cw list' to see active sessions", message)
    } else if message.contains("not running") {
        format!(
            "{}\n\nUse 'cw status <id>' to check session status",
            message
        )
    } else {
        message.to_string()
    }
}

/// Send a request and read a single response.
async fn request_response(target: &Target, req: &Request) -> Result<Response> {
    let (mut reader, mut writer) = target.connect().await?;

    writer.send_request(req).await?;

    let frame = reader
        .read_frame()
        .await?
        .context("unexpected EOF from node")?;

    match frame {
        Frame::Control(payload) => protocol::parse_response(&payload),
        Frame::Data(_) => bail!("unexpected data frame"),
    }
}

// ---------------------------------------------------------------------------
// Public commands
// ---------------------------------------------------------------------------

pub async fn list(target: &Target, json: bool) -> Result<()> {
    let resp = request_response(target, &Request::ListSessions).await?;

    match resp {
        Response::SessionList { sessions } => {
            if json {
                println!("{}", serde_json::to_string_pretty(&sessions)?);
            } else if sessions.is_empty() {
                println!("No sessions.");
            } else {
                print_session_table(&sessions);
            }
        }
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn run(target: &Target, command: Vec<String>, working_dir: String) -> Result<()> {
    let display = command.join(" ");
    let resp = request_response(
        target,
        &Request::Launch {
            command,
            working_dir,
        },
    )
    .await?;

    match resp {
        Response::Launched { id } => {
            println!("Session {id} launched: {display}");
        }
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn attach(target: &Target, id: Option<u32>, no_history: bool) -> Result<()> {
    // Auto-select session if ID not provided
    let (id, auto_selected) = if let Some(id) = id {
        (id, false)
    } else {
        // List all sessions
        let resp = request_response(target, &Request::ListSessions).await?;
        match resp {
            Response::SessionList { sessions } => {
                // Filter for running unattached sessions
                let mut candidates: Vec<_> = sessions
                    .into_iter()
                    .filter(|s| s.status == "running" && !s.attached)
                    .collect();

                if candidates.is_empty() {
                    bail!(
                        "No unattached running sessions available.\n\n\
                        Use 'cw list' to see all sessions or 'cw launch' to start a new one"
                    );
                }

                // Sort by created_at (oldest first)
                candidates.sort_by(|a, b| a.created_at.cmp(&b.created_at));

                (candidates[0].id, true)
            }
            Response::Error { message } => bail!("{}", format_error(&message)),
            _ => bail!("unexpected response from ListSessions"),
        }
    };

    let (mut reader, mut writer) = target.connect().await?;

    // Request attach
    writer
        .send_request(&Request::Attach {
            id,
            include_history: !no_history,
            history_lines: None,
        })
        .await?;

    // Read response
    let frame = reader.read_frame().await?.context("unexpected EOF")?;
    let resp = match frame {
        Frame::Control(payload) => protocol::parse_response(&payload)?,
        _ => bail!("unexpected frame"),
    };

    match resp {
        Response::Attached { id } => {
            if auto_selected {
                eprintln!("[cw] attached to session {id} (auto-selected) (Ctrl+B d to detach)");
            } else {
                eprintln!("[cw] attached to session {id} (Ctrl+B d to detach)");
            }
        }
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }

    // Enter raw mode
    let _guard = RawModeGuard::enable()?;

    // Set up status bar
    let mut stdout = tokio::io::stdout();
    let mut status_bar = if let Ok((cols, rows)) = terminal_size() {
        let bar = StatusBar::new(id, cols, rows);
        let setup = bar.setup();
        if !setup.is_empty() {
            stdout.write_all(&setup).await?;
            stdout.flush().await?;
        }
        // Send reduced PTY size to node
        let (pty_cols, pty_rows) = bar.pty_size();
        writer
            .send_request(&Request::Resize {
                cols: pty_cols,
                rows: pty_rows,
            })
            .await?;
        bar
    } else {
        StatusBar::new(id, 80, 24)
    };

    // Set up SIGWINCH handler
    let mut winch = resize_signal()?;
    let mut tick = tokio::time::interval(Duration::from_secs(10));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    let mut stdin = tokio::io::stdin();
    let mut detach = DetachDetector::new();
    let mut input_buf = [0u8; 4096];

    loop {
        tokio::select! {
            // Read from node (PTY output) → write to stdout
            frame = reader.read_frame() => {
                match frame? {
                    Some(Frame::Data(bytes)) => {
                        stdout.write_all(&bytes).await?;
                        stdout.flush().await?;
                    }
                    Some(Frame::Control(payload)) => {
                        let resp = protocol::parse_response(&payload)?;
                        match resp {
                            Response::Detached => {
                                stdout.write_all(&status_bar.teardown()).await?;
                                stdout.flush().await?;
                                drop(_guard);
                                eprintln!("\r\n[cw] detached from session {id}");
                                std::process::exit(0);
                            }
                            Response::Error { message } => {
                                stdout.write_all(&status_bar.teardown()).await?;
                                stdout.flush().await?;
                                drop(_guard);
                                eprintln!("\r\n[cw] session ended: {message}");
                                std::process::exit(0);
                            }
                            _ => {}
                        }
                    }
                    None => {
                        // Node disconnected
                        stdout.write_all(&status_bar.teardown()).await?;
                        stdout.flush().await?;
                        drop(_guard);
                        eprintln!("\r\n[cw] connection lost");
                        std::process::exit(1);
                    }
                }
            }

            // Read from stdin → send to node (PTY input)
            n = stdin.read(&mut input_buf) => {
                let n = n?;
                if n == 0 {
                    break;
                }
                let (should_detach, forward) = detach.feed_buf(&input_buf[..n]);
                if should_detach {
                    writer.send_request(&Request::Detach).await?;
                    // Wait for detach confirmation
                    continue;
                }
                if !forward.is_empty() {
                    writer.send_data(&forward).await?;
                }
            }

            // Terminal resize
            _ = winch.recv() => {
                if let Ok((cols, rows)) = terminal_size() {
                    let resize_seq = status_bar.resize(cols, rows);
                    stdout.write_all(&resize_seq).await?;
                    stdout.flush().await?;
                    let (pty_cols, pty_rows) = status_bar.pty_size();
                    writer.send_request(&Request::Resize { cols: pty_cols, rows: pty_rows }).await?;
                }
            }

            // Tick: redraw status bar to update age counter
            _ = tick.tick() => {
                let draw = status_bar.draw();
                if !draw.is_empty() {
                    stdout.write_all(&draw).await?;
                    stdout.flush().await?;
                }
            }
        }
    }

    Ok(())
}

pub async fn kill(target: &Target, id: u32) -> Result<()> {
    let resp = request_response(target, &Request::Kill { id }).await?;
    match resp {
        Response::Killed { id } => println!("Session {id} killed."),
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn kill_all(target: &Target) -> Result<()> {
    let resp = request_response(target, &Request::KillAll).await?;
    match resp {
        Response::KilledAll { count } => println!("Killed {count} session(s)."),
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn logs(target: &Target, id: u32, follow: bool, tail: Option<usize>) -> Result<()> {
    let (mut reader, mut writer) = target.connect().await?;

    writer
        .send_request(&Request::Logs { id, follow, tail })
        .await?;

    let mut stdout = tokio::io::stdout();

    loop {
        let frame = reader.read_frame().await?;
        match frame {
            Some(Frame::Control(payload)) => {
                let resp = protocol::parse_response(&payload)?;
                match resp {
                    Response::LogData { data, done } => {
                        if !data.is_empty() {
                            stdout.write_all(data.as_bytes()).await?;
                            stdout.flush().await?;
                        }
                        if done {
                            break;
                        }
                    }
                    Response::Error { message } => bail!("{}", format_error(&message)),
                    _ => {}
                }
            }
            None => break,
            _ => {}
        }
    }

    Ok(())
}

pub async fn send_input(
    target: &Target,
    id: u32,
    input: Option<String>,
    stdin: bool,
    file: Option<PathBuf>,
    no_newline: bool,
) -> Result<()> {
    // Collect input from various sources
    let mut data = if let Some(text) = input {
        text.into_bytes()
    } else if stdin {
        let mut buf = Vec::new();
        tokio::io::stdin().read_to_end(&mut buf).await?;
        buf
    } else if let Some(path) = file {
        std::fs::read(&path).with_context(|| format!("reading {}", path.display()))?
    } else {
        bail!("must provide input via argument, --stdin, or --file");
    };

    // Add newline unless disabled
    if !no_newline && !data.ends_with(b"\n") {
        data.push(b'\n');
    }

    let resp = request_response(target, &Request::SendInput { id, data }).await?;

    match resp {
        Response::InputSent { id, bytes } => {
            println!("Sent {bytes} bytes to session {id}");
        }
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn watch_session(
    target: &Target,
    id: u32,
    tail: Option<usize>,
    no_history: bool,
    timeout: Option<u64>,
) -> Result<()> {
    let (mut reader, mut writer) = target.connect().await?;

    // Send watch request
    writer
        .send_request(&Request::WatchSession {
            id,
            include_history: !no_history,
            history_lines: tail,
        })
        .await?;

    let mut stdout = tokio::io::stdout();

    // Set up timeout if requested
    let timeout_future = if let Some(seconds) = timeout {
        tokio::time::sleep(tokio::time::Duration::from_secs(seconds))
    } else {
        tokio::time::sleep(tokio::time::Duration::from_secs(u64::MAX))
    };

    tokio::pin!(timeout_future);

    loop {
        tokio::select! {
            frame = reader.read_frame() => {
                match frame? {
                    Some(Frame::Control(payload)) => {
                        let resp = protocol::parse_response(&payload)?;
                        match resp {
                            Response::WatchUpdate { status, output, done, .. } => {
                                if let Some(text) = output {
                                    stdout.write_all(text.as_bytes()).await?;
                                    stdout.flush().await?;
                                }
                                if done {
                                    eprintln!("\n[cw] session {status}");
                                    break;
                                }
                            }
                            Response::Error { message } => bail!("{}", format_error(&message)),
                            _ => {}
                        }
                    }
                    None => break,
                    _ => {}
                }
            }

            _ = &mut timeout_future => {
                eprintln!("\n[cw] watch timeout");
                break;
            }
        }
    }

    Ok(())
}

pub async fn get_status(target: &Target, id: u32, json: bool) -> Result<()> {
    let resp = request_response(target, &Request::GetStatus { id }).await?;

    match resp {
        Response::SessionStatus { info, output_size } => {
            if json {
                let mut obj = serde_json::to_value(&info)?;
                if let Some(o) = obj.as_object_mut() {
                    o.insert("output_size".to_string(), serde_json::json!(output_size));
                }
                println!("{}", serde_json::to_string_pretty(&obj)?);
            } else {
                println!("Session {}", info.id);
                println!("  Status: {}", info.status);
                println!("  Command: {}", info.prompt);
                println!("  Working Dir: {}", info.working_dir);
                println!("  Created: {}", info.created_at);
                println!("  Attached: {}", if info.attached { "yes" } else { "no" });
                if let Some(pid) = info.pid {
                    println!("  PID: {}", pid);
                }
                println!("  Output Size: {} bytes", output_size);
                if let Some(snippet) = info.last_output_snippet {
                    println!("  Last Output:");
                    for line in snippet.lines() {
                        println!("    {}", line);
                    }
                }
            }
        }
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn print_session_table(sessions: &[SessionInfo]) {
    println!(
        "{:<5} {:<12} {:<20} {:<8} PROMPT",
        "ID", "STATUS", "CREATED", "ATTACHED"
    );
    println!("{}", "-".repeat(70));

    for s in sessions {
        let created = format_relative_time(&s.created_at);
        let prompt = if s.prompt.len() > 50 {
            format!("{}...", &s.prompt[..47])
        } else {
            s.prompt.clone()
        };
        println!(
            "{:<5} {:<12} {:<20} {:<8} {}",
            s.id,
            s.status,
            created,
            if s.attached { "yes" } else { "no" },
            prompt
        );
    }
}

fn format_relative_time(iso: &str) -> String {
    let Ok(dt) = chrono::DateTime::parse_from_rfc3339(iso) else {
        return iso.to_string();
    };
    let now = chrono::Utc::now();
    let dur = now.signed_duration_since(dt);

    if dur.num_seconds() < 60 {
        format!("{}s ago", dur.num_seconds())
    } else if dur.num_minutes() < 60 {
        format!("{}m ago", dur.num_minutes())
    } else if dur.num_hours() < 24 {
        format!("{}h ago", dur.num_hours())
    } else {
        format!("{}d ago", dur.num_days())
    }
}
