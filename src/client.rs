use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

use crate::protocol::{
    self, Frame, Request, Response, SessionInfo, read_frame, send_data, send_request,
};
use crate::terminal::{DetachDetector, RawModeGuard, resize_signal, terminal_size};

/// Connect to the daemon socket.
async fn connect(data_dir: &Path) -> Result<UnixStream> {
    let sock = data_dir.join("server.sock");
    UnixStream::connect(&sock)
        .await
        .with_context(|| {
            format!(
                "Failed to connect to CodeWire daemon at {}\nThe daemon may not be running. It should auto-start on first command.",
                sock.display()
            )
        })
}

/// Format error message with helpful context
fn format_error(message: &str) -> String {
    if message.contains("not found") {
        format!("{}\n\nUse 'cw list' to see active sessions", message)
    } else if message.contains("not running") {
        format!("{}\n\nUse 'cw status <id>' to check session status", message)
    } else {
        message.to_string()
    }
}

/// Send a request and read a single response.
async fn request_response(data_dir: &Path, req: &Request) -> Result<Response> {
    let stream = connect(data_dir).await?;
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, req).await?;

    let frame = read_frame(&mut reader)
        .await?
        .context("unexpected EOF from daemon")?;

    match frame {
        Frame::Control(payload) => protocol::parse_response(&payload),
        Frame::Data(_) => bail!("unexpected data frame"),
    }
}

// ---------------------------------------------------------------------------
// Public commands
// ---------------------------------------------------------------------------

pub async fn list(data_dir: &Path, json: bool) -> Result<()> {
    let resp = request_response(data_dir, &Request::ListSessions).await?;

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

pub async fn launch(data_dir: &Path, command: Vec<String>, working_dir: String) -> Result<()> {
    let display = command.join(" ");
    let resp = request_response(
        data_dir,
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

pub async fn attach(data_dir: &Path, id: Option<u32>) -> Result<()> {
    // Auto-select session if ID not provided
    let (id, auto_selected) = if let Some(id) = id {
        (id, false)
    } else {
        // List all sessions
        let resp = request_response(data_dir, &Request::ListSessions).await?;
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

    let stream = connect(data_dir).await?;
    let (mut reader, mut writer) = stream.into_split();

    // Request attach
    send_request(&mut writer, &Request::Attach { id }).await?;

    // Read response
    let frame = read_frame(&mut reader)
        .await?
        .context("unexpected EOF")?;
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

    // Send initial terminal size
    if let Ok((cols, rows)) = terminal_size() {
        send_request(&mut writer, &Request::Resize { cols, rows }).await?;
    }

    // Set up SIGWINCH handler
    let mut winch = resize_signal()?;

    let mut stdin = tokio::io::stdin();
    let mut stdout = tokio::io::stdout();
    let mut detach = DetachDetector::new();
    let mut input_buf = [0u8; 4096];

    loop {
        tokio::select! {
            // Read from daemon (PTY output) → write to stdout
            frame = read_frame(&mut reader) => {
                match frame? {
                    Some(Frame::Data(bytes)) => {
                        stdout.write_all(&bytes).await?;
                        stdout.flush().await?;
                    }
                    Some(Frame::Control(payload)) => {
                        let resp = protocol::parse_response(&payload)?;
                        match resp {
                            Response::Detached => {
                                drop(_guard);
                                eprintln!("\r\n[cw] detached from session {id}");
                                return Ok(());
                            }
                            Response::Error { message } => {
                                drop(_guard);
                                eprintln!("\r\n[cw] session ended: {message}");
                                return Ok(());
                            }
                            _ => {}
                        }
                    }
                    None => {
                        // Daemon disconnected
                        drop(_guard);
                        eprintln!("\r\n[cw] connection lost");
                        return Ok(());
                    }
                }
            }

            // Read from stdin → send to daemon (PTY input)
            n = stdin.read(&mut input_buf) => {
                let n = n?;
                if n == 0 {
                    break;
                }
                let (should_detach, forward) = detach.feed_buf(&input_buf[..n]);
                if should_detach {
                    send_request(&mut writer, &Request::Detach).await?;
                    // Wait for detach confirmation
                    continue;
                }
                if !forward.is_empty() {
                    send_data(&mut writer, &forward).await?;
                }
            }

            // Terminal resize
            _ = winch.recv() => {
                if let Ok((cols, rows)) = terminal_size() {
                    send_request(&mut writer, &Request::Resize { cols, rows }).await?;
                }
            }
        }
    }

    Ok(())
}

pub async fn kill(data_dir: &Path, id: u32) -> Result<()> {
    let resp = request_response(data_dir, &Request::Kill { id }).await?;
    match resp {
        Response::Killed { id } => println!("Session {id} killed."),
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn kill_all(data_dir: &Path) -> Result<()> {
    let resp = request_response(data_dir, &Request::KillAll).await?;
    match resp {
        Response::KilledAll { count } => println!("Killed {count} session(s)."),
        Response::Error { message } => bail!("{}", format_error(&message)),
        _ => bail!("unexpected response"),
    }
    Ok(())
}

pub async fn logs(data_dir: &Path, id: u32, follow: bool, tail: Option<usize>) -> Result<()> {
    let stream = connect(data_dir).await?;
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, &Request::Logs { id, follow, tail }).await?;

    let mut stdout = tokio::io::stdout();

    loop {
        let frame = read_frame(&mut reader).await?;
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
    data_dir: &Path,
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

    let resp = request_response(data_dir, &Request::SendInput { id, data }).await?;

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
    data_dir: &Path,
    id: u32,
    tail: Option<usize>,
    no_history: bool,
    timeout: Option<u64>,
) -> Result<()> {
    let stream = connect(data_dir).await?;
    let (mut reader, mut writer) = stream.into_split();

    // Send watch request
    send_request(
        &mut writer,
        &Request::WatchSession {
            id,
            include_history: !no_history,
            history_lines: tail,
        },
    )
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
            frame = read_frame(&mut reader) => {
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

pub async fn get_status(data_dir: &Path, id: u32, json: bool) -> Result<()> {
    let resp = request_response(data_dir, &Request::GetStatus { id }).await?;

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
        "{:<5} {:<12} {:<20} {:<8} {}",
        "ID", "STATUS", "CREATED", "ATTACHED", "PROMPT"
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
