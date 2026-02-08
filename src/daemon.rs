use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Mutex;
use tracing::{error, info, warn};

use crate::protocol::{
    self, Frame, Request, Response, read_frame, send_data, send_response,
};
use crate::session::{SessionManager, SessionStatus};

pub struct Daemon {
    manager: Arc<Mutex<SessionManager>>,
    socket_path: PathBuf,
    pid_path: PathBuf,
}

impl Daemon {
    pub fn new(data_dir: &Path) -> Result<Self> {
        let manager = SessionManager::new(data_dir.to_path_buf())?;
        Ok(Self {
            manager: Arc::new(Mutex::new(manager)),
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

        // Periodic status refresh
        let manager_refresh = self.manager.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(5));
            loop {
                interval.tick().await;
                manager_refresh.lock().await.refresh_statuses();
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
    manager: Arc<Mutex<SessionManager>>,
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
            let sessions = manager.lock().await.list();
            send_response(&mut writer, &Response::SessionList { sessions }).await?;
        }

        Request::Launch { command, working_dir } => {
            let result = manager.lock().await.launch(command, working_dir);
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
            let attach_result = manager.lock().await.attach(id);
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
                    manager.lock().await.detach(id).ok();

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
            let result = manager.lock().await.kill(id);
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
            let count = manager.lock().await.kill_all();
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
            let log_path = manager.lock().await.log_path(id);
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
    manager: &Arc<Mutex<SessionManager>>,
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
                        pty_input_tx.send(bytes).await.ok();
                    }
                    Some(Frame::Control(payload)) => {
                        let req: Request = protocol::parse_request(&payload)?;
                        match req {
                            Request::Detach => {
                                send_response(writer, &Response::Detached).await?;
                                break;
                            }
                            Request::Resize { cols, rows } => {
                                manager.lock().await.resize(session_id, cols, rows).ok();
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
                    send_response(writer, &Response::Error {
                        message: format!("session {status}"),
                    }).await.ok();
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
