use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::net::UnixListener;
use tokio::sync::mpsc;
use tokio::time::{sleep, Duration};
use tracing::{error, info, warn};

use crate::config::Config;
use crate::connection::{FrameReader, FrameWriter};
use crate::protocol::{self, Frame, Request, Response};
use crate::session::{SessionManager, SessionStatus};

pub struct Daemon {
    manager: Arc<SessionManager>,
    socket_path: PathBuf,
    pid_path: PathBuf,
    config: Config,
    data_dir: PathBuf,
}

impl Daemon {
    pub fn new(data_dir: &Path) -> Result<Self> {
        let config = Config::load(data_dir)?;
        let (manager, persist_rx) = SessionManager::new(data_dir.to_path_buf())?;
        let manager = Arc::new(manager);

        // Spawn persistence manager task
        let manager_persist = manager.clone();
        tokio::spawn(async move {
            persistence_manager(manager_persist, persist_rx).await;
        });

        // Generate auth token (for WebSocket connections)
        let _ = crate::auth::load_or_generate_token(data_dir);

        Ok(Self {
            manager,
            socket_path: data_dir.join("server.sock"),
            pid_path: data_dir.join("daemon.pid"),
            config,
            data_dir: data_dir.to_path_buf(),
        })
    }

    pub async fn run(&self) -> Result<()> {
        // Write PID file
        std::fs::write(&self.pid_path, std::process::id().to_string())?;

        // Clean up stale socket
        if self.socket_path.exists() {
            std::fs::remove_file(&self.socket_path)?;
        }

        let listener = UnixListener::bind(&self.socket_path).context("binding Unix socket")?;

        info!(path = %self.socket_path.display(), "daemon listening on Unix socket");

        // Start WebSocket listener if configured
        #[cfg(feature = "ws")]
        if let Some(ref listen_addr) = self.config.daemon.listen {
            let manager = self.manager.clone();
            let data_dir = self.data_dir.clone();
            let addr = listen_addr.clone();
            tokio::spawn(async move {
                if let Err(e) = run_ws_server(&addr, manager, &data_dir).await {
                    error!(?e, "WebSocket server error");
                }
            });
        }

        // Start NATS fleet integration if configured
        #[cfg(feature = "nats")]
        if let Some(ref nats_config) = self.config.nats {
            let manager = self.manager.clone();
            let daemon_config = self.config.daemon.clone();
            let nats_cfg = nats_config.clone();
            tokio::spawn(async move {
                if let Err(e) = crate::fleet::run_fleet(&nats_cfg, &daemon_config, manager).await {
                    error!(?e, "NATS fleet error");
                }
            });
        }

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
                        let (r, w) = stream.into_split();
                        let reader = FrameReader::Unix(r);
                        let writer = FrameWriter::Unix(w);
                        if let Err(e) = handle_client(reader, writer, manager).await {
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

// ---------------------------------------------------------------------------
// WebSocket server (optional, behind "ws" feature)
// ---------------------------------------------------------------------------

#[cfg(feature = "ws")]
async fn run_ws_server(addr: &str, manager: Arc<SessionManager>, data_dir: &Path) -> Result<()> {
    use axum::extract::{ws::WebSocketUpgrade, Query, State};
    use axum::response::IntoResponse;
    use axum::routing::get;
    use axum::Router;
    use std::collections::HashMap;

    #[derive(Clone)]
    struct WsState {
        manager: Arc<SessionManager>,
        data_dir: PathBuf,
    }

    async fn ws_handler(
        ws: WebSocketUpgrade,
        Query(params): Query<HashMap<String, String>>,
        State(state): State<WsState>,
    ) -> impl IntoResponse {
        // Validate token
        let token = params.get("token").map(|s| s.as_str()).unwrap_or("");
        if !crate::auth::validate_token(&state.data_dir, token) {
            return axum::http::StatusCode::UNAUTHORIZED.into_response();
        }

        ws.on_upgrade(move |socket| async move {
            use futures::StreamExt;
            let (ws_writer, ws_reader) = socket.split();
            let reader = FrameReader::WebSocket(ws_reader);
            let writer = FrameWriter::WebSocket(ws_writer);
            if let Err(e) = handle_client(reader, writer, state.manager).await {
                warn!(?e, "WebSocket client handler error");
            }
        })
    }

    let state = WsState {
        manager,
        data_dir: data_dir.to_path_buf(),
    };

    let app = Router::new()
        .route("/ws", get(ws_handler))
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(addr).await?;
    info!(addr, "WebSocket server listening");
    axum::serve(listener, app).await?;
    Ok(())
}

// ---------------------------------------------------------------------------
// Client handler (transport-agnostic)
// ---------------------------------------------------------------------------

async fn handle_client(
    mut reader: FrameReader,
    mut writer: FrameWriter,
    manager: Arc<SessionManager>,
) -> Result<()> {
    // Read the first frame — must be a control message
    let frame = reader.read_frame().await?;
    let Some(Frame::Control(payload)) = frame else {
        writer
            .send_response(&Response::Error {
                message: "expected control message".into(),
            })
            .await?;
        return Ok(());
    };

    let request: Request = protocol::parse_request(&payload)?;

    match request {
        Request::ListSessions => {
            let sessions = manager.list();
            writer
                .send_response(&Response::SessionList { sessions })
                .await?;
        }

        Request::Launch {
            command,
            working_dir,
        } => {
            let result = manager.launch(command, working_dir);
            match result {
                Ok(id) => writer.send_response(&Response::Launched { id }).await?,
                Err(e) => {
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
                        .await?
                }
            }
        }

        Request::Attach { id } => {
            let attach_result = manager.attach(id);
            match attach_result {
                Ok((mut broadcast_rx, pty_input_tx, mut status_rx)) => {
                    writer.send_response(&Response::Attached { id }).await?;

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
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
                        .await?;
                }
            }
        }

        Request::Kill { id } => {
            let result = manager.kill(id);
            match result {
                Ok(()) => writer.send_response(&Response::Killed { id }).await?,
                Err(e) => {
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
                        .await?
                }
            }
        }

        Request::KillAll => {
            let count = manager.kill_all();
            writer.send_response(&Response::KilledAll { count }).await?;
        }

        Request::Resize { .. } => {
            // Resize only makes sense during attach, but handle gracefully
            writer.send_response(&Response::Resized).await?;
        }

        Request::Detach => {
            writer.send_response(&Response::Detached).await?;
        }

        Request::Logs { id, follow, tail } => {
            let log_path = manager.log_path(id);
            match log_path {
                Ok(path) => {
                    handle_logs(&mut writer, &path, follow, tail).await?;
                }
                Err(e) => {
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
                        .await?;
                }
            }
        }

        Request::SendInput { id, data } => {
            let result = manager.send_input(id, data);
            match result {
                Ok(bytes) => {
                    writer
                        .send_response(&Response::InputSent { id, bytes })
                        .await?
                }
                Err(e) => {
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
                        .await?
                }
            }
        }

        Request::GetStatus { id } => {
            let result = manager.get_status(id);
            match result {
                Ok((info, output_size)) => {
                    writer
                        .send_response(&Response::SessionStatus { info, output_size })
                        .await?
                }
                Err(e) => {
                    writer
                        .send_response(&Response::Error {
                            message: e.to_string(),
                        })
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
    reader: &mut FrameReader,
    writer: &mut FrameWriter,
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
                        writer.send_data(&bytes).await?;
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
            frame = reader.read_frame() => {
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
                                writer.send_response(&Response::Detached).await?;
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
                    if let Err(e) = writer.send_response(&Response::Error {
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
    _reader: &mut FrameReader,
    writer: &mut FrameWriter,
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
                writer
                    .send_response(&Response::WatchUpdate {
                        id,
                        status: "running".to_string(),
                        output: Some(output),
                        done: false,
                    })
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
                        writer.send_response(
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

                writer.send_response(
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
    writer: &mut FrameWriter,
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

    writer
        .send_response(&Response::LogData {
            data: output,
            done: !follow,
        })
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
                    writer
                        .send_response(&Response::LogData {
                            data: String::from_utf8_lossy(new_data).into_owned(),
                            done: false,
                        })
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
