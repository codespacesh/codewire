use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::mpsc;
use tokio::time::{Duration, sleep};
use tracing::{error, info, warn};

use crate::protocol::{
    self, Frame, Request, Response, read_frame, send_data, send_response,
};
use crate::session::{SessionManager, SessionStatus};

pub struct Daemon {
    manager: Arc<SessionManager>,
    socket_path: PathBuf,
    pid_path: PathBuf,
}

impl Daemon {
    pub fn new(data_dir: &Path) -> Result<Self> {
        let (manager, persist_rx) = SessionManager::new(data_dir.to_path_buf())?;
        let manager = Arc::new(manager);

        // Spawn persistence manager task
        let manager_persist = manager.clone();
        tokio::spawn(async move {
            persistence_manager(manager_persist, persist_rx).await;
        });

        Ok(Self {
            manager,
            socket_path: data_dir.join("server.sock"),
            pid_path: data_dir.join("daemon.pid"),
        })
    }

    pub async fn run(&self) -> Result<()> {
        // Write PID file
        std::fs::write(&self.pid_path, std::process::id().to_string())?;

        // Clean up stale socket
        if self.socket_path.exists() {
            std::fs::remove_file(&self.socket_path)?;
        }

        let listener = UnixListener::bind(&self.socket_path)
            .context("binding Unix socket")?;

        info!(path = %self.socket_path.display(), "daemon listening");

        // Periodic status refresh (still needed to update from watch channels)
        let manager_refresh = self.manager.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(5));
            loop {
                interval.tick().await;
                manager_refresh.refresh_statuses();
            }
        });

        loop {
            match listener.accept().await {
                Ok((stream, _)) => {
                    let manager = self.manager.clone();
                    tokio::spawn(async move {
                        if let Err(e) = handle_client(stream, manager).await {
                            warn!(?e, "client handler error");
                        }
                    });
                }
                Err(e) => {
                    error!(?e, "accept error");
                }
            }
        }
    }
}

impl Drop for Daemon {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.socket_path);
        let _ = std::fs::remove_file(&self.pid_path);
    }
}

async fn handle_client(
    stream: UnixStream,
    manager: Arc<SessionManager>,
) -> Result<()> {
    let (mut reader, mut writer) = stream.into_split();

    // Read the first frame — must be a control message
    let frame = read_frame(&mut reader).await?;
    let Some(Frame::Control(payload)) = frame else {
        send_response(
            &mut writer,
            &Response::Error {
                message: "expected control message".into(),
            },
        )
        .await?;
        return Ok(());
    };

    let request: Request = protocol::parse_request(&payload)?;

    match request {
        Request::ListSessions => {
            let sessions = manager.list();
            send_response(&mut writer, &Response::SessionList { sessions }).await?;
        }

        Request::Launch { command, working_dir } => {
            let result = manager.launch(command, working_dir);
            match result {
                Ok(id) => send_response(&mut writer, &Response::Launched { id }).await?,
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?
                }
            }
        }

        Request::Attach { id } => {
            let attach_result = manager.attach(id);
            match attach_result {
                Ok((mut broadcast_rx, pty_input_tx, mut status_rx)) => {
                    send_response(&mut writer, &Response::Attached { id }).await?;

                    // Bridge: PTY output → client, client input → PTY
                    let detached = handle_attach_session(
                        &mut reader,
                        &mut writer,
                        &mut broadcast_rx,
                        &pty_input_tx,
                        &mut status_rx,
                        id,
                        &manager,
                    )
                    .await;

                    // Always detach on exit
                    if let Err(e) = manager.detach(id) {
                        warn!(id, ?e, "Failed to detach session on client disconnect");
                    }

                    if let Err(e) = detached {
                        warn!(id, ?e, "attach session ended with error");
                    }
                }
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?;
                }
            }
        }

        Request::Kill { id } => {
            let result = manager.kill(id);
            match result {
                Ok(()) => send_response(&mut writer, &Response::Killed { id }).await?,
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?
                }
            }
        }

        Request::KillAll => {
            let count = manager.kill_all();
            send_response(&mut writer, &Response::KilledAll { count }).await?;
        }

        Request::Resize { .. } => {
            // Resize only makes sense during attach, but handle gracefully
            send_response(&mut writer, &Response::Resized).await?;
        }

        Request::Detach => {
            send_response(&mut writer, &Response::Detached).await?;
        }

        Request::Logs { id, follow, tail } => {
            let log_path = manager.log_path(id);
            match log_path {
                Ok(path) => {
                    handle_logs(&mut writer, &path, follow, tail).await?;
                }
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?;
                }
            }
        }

        Request::SendInput { id, data } => {
            let result = manager.send_input(id, data);
            match result {
                Ok(bytes) => {
                    send_response(&mut writer, &Response::InputSent { id, bytes }).await?
                }
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?
                }
            }
        }

        Request::GetStatus { id } => {
            let result = manager.get_status(id);
            match result {
                Ok((info, output_size)) => {
                    send_response(&mut writer, &Response::SessionStatus { info, output_size })
                        .await?
                }
                Err(e) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: e.to_string(),
                        },
                    )
                    .await?
                }
            }
        }

        Request::WatchSession {
            id,
            include_history,
            history_lines,
        } => {
            handle_watch_session(
                &mut reader,
                &mut writer,
                &manager,
                id,
                include_history,
                history_lines,
            )
            .await?;
        }
    }

    Ok(())
}

/// Handle an attached session: bridge PTY ↔ client.
async fn handle_attach_session(
    reader: &mut tokio::net::unix::OwnedReadHalf,
    writer: &mut tokio::net::unix::OwnedWriteHalf,
    broadcast_rx: &mut tokio::sync::broadcast::Receiver<Vec<u8>>,
    pty_input_tx: &tokio::sync::mpsc::Sender<Vec<u8>>,
    status_rx: &mut tokio::sync::watch::Receiver<SessionStatus>,
    session_id: u32,
    manager: &Arc<SessionManager>,
) -> Result<()> {
    loop {
        tokio::select! {
            // PTY output → client
            data = broadcast_rx.recv() => {
                match data {
                    Ok(bytes) => {
                        send_data(writer, &bytes).await?;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                        warn!(session_id, n, "client lagged, skipped messages");
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => {
                        info!(session_id, "broadcast closed");
                        break;
                    }
                }
            }

            // Client input → PTY (or control messages)
            frame = read_frame(reader) => {
                match frame? {
                    Some(Frame::Data(bytes)) => {
                        if let Err(e) = pty_input_tx.send(bytes).await {
                            error!(session_id, ?e, "Failed to send client input to PTY - data loss");
                        }
                    }
                    Some(Frame::Control(payload)) => {
                        let req: Request = protocol::parse_request(&payload)?;
                        match req {
                            Request::Detach => {
                                send_response(writer, &Response::Detached).await?;
                                break;
                            }
                            Request::Resize { cols, rows } => {
                                if let Err(e) = manager.resize(session_id, cols, rows) {
                                    warn!(session_id, ?e, "Failed to resize PTY");
                                }
                            }
                            _ => {}
                        }
                    }
                    None => {
                        // Client disconnected
                        info!(session_id, "client disconnected");
                        break;
                    }
                }
            }

            // Session ended
            _ = status_rx.changed() => {
                let status = status_rx.borrow().clone();
                if status != SessionStatus::Running {
                    info!(session_id, %status, "session ended while attached");
                    // Send remaining output then notify
                    if let Err(e) = send_response(writer, &Response::Error {
                        message: format!("session {status}"),
                    }).await {
                        warn!(session_id, ?e, "Failed to send session end notification");
                    }
                    break;
                }
            }
        }
    }
    Ok(())
}

/// Watch a session in real-time, streaming output and status updates
async fn handle_watch_session(
    _reader: &mut tokio::net::unix::OwnedReadHalf,
    writer: &mut tokio::net::unix::OwnedWriteHalf,
    manager: &Arc<SessionManager>,
    id: u32,
    include_history: bool,
    history_lines: Option<usize>,
) -> Result<()> {
    // Subscribe to output and status
    let mut broadcast_rx = manager.subscribe_output(id)?;
    let mut status_rx = manager.subscribe_status(id)?;

    // Send historical output if requested
    if include_history {
        let log_path = manager.log_path(id)?;
        if log_path.exists() {
            let content = std::fs::read_to_string(&log_path).unwrap_or_default();
            let output = if let Some(n) = history_lines {
                let lines: Vec<&str> = content.lines().collect();
                let start = lines.len().saturating_sub(n);
                lines[start..].join("\n")
            } else {
                content
            };

            if !output.is_empty() {
                send_response(
                    writer,
                    &Response::WatchUpdate {
                        id,
                        status: "running".to_string(),
                        output: Some(output),
                        done: false,
                    },
                )
                .await?;
            }
        }
    }

    // Stream new output and status changes
    loop {
        tokio::select! {
            data = broadcast_rx.recv() => {
                match data {
                    Ok(bytes) => {
                        let output = String::from_utf8_lossy(&bytes).to_string();
                        send_response(
                            writer,
                            &Response::WatchUpdate {
                                id,
                                status: "running".to_string(),
                                output: Some(output),
                                done: false,
                            },
                        )
                        .await?;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                        // Skip lagged messages
                        continue;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => {
                        // Broadcast closed, session likely ended
                        break;
                    }
                }
            }

            _ = status_rx.changed() => {
                let status = status_rx.borrow().clone();
                let status_str = status.to_string();
                let done = status != crate::session::SessionStatus::Running;

                send_response(
                    writer,
                    &Response::WatchUpdate {
                        id,
                        status: status_str,
                        output: None,
                        done,
                    },
                )
                .await?;

                if done {
                    break;
                }
            }
        }
    }

    Ok(())
}

/// Send log file contents to the client.
async fn handle_logs(
    writer: &mut tokio::net::unix::OwnedWriteHalf,
    path: &Path,
    follow: bool,
    tail: Option<usize>,
) -> Result<()> {
    let content = if path.exists() {
        std::fs::read_to_string(path).unwrap_or_default()
    } else {
        String::new()
    };

    let output = if let Some(n) = tail {
        let lines: Vec<&str> = content.lines().collect();
        let start = lines.len().saturating_sub(n);
        lines[start..].join("\n")
    } else {
        content
    };

    send_response(
        writer,
        &Response::LogData {
            data: output,
            done: !follow,
        },
    )
    .await?;

    if follow {
        // Tail the file — simple poll approach
        let mut last_len = path.metadata().map(|m| m.len()).unwrap_or(0);
        loop {
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            let current_len = path.metadata().map(|m| m.len()).unwrap_or(0);
            if current_len > last_len {
                if let Ok(data) = std::fs::read(path) {
                    let new_data = &data[last_len as usize..];
                    send_response(
                        writer,
                        &Response::LogData {
                            data: String::from_utf8_lossy(new_data).into_owned(),
                            done: false,
                        },
                    )
                    .await?;
                    last_len = current_len;
                }
            }
        }
    }

    Ok(())
}

/// Event-driven persistence manager with debouncing.
/// Only writes sessions.json when state changes, debounced to avoid thrashing.
async fn persistence_manager(
    manager: Arc<SessionManager>,
    mut persist_rx: mpsc::UnboundedReceiver<()>,
) {
    let mut pending = false;

    loop {
        tokio::select! {
            // Receive persistence event
            event = persist_rx.recv() => {
                if event.is_none() {
                    // Channel closed, exit
                    break;
                }
                pending = true;
            }

            // Debounce timer - wait 500ms after last event
            _ = sleep(Duration::from_millis(500)), if pending => {
                pending = false;
                manager.persist_meta();
            }
        }
    }

    info!("persistence manager exited");
}
