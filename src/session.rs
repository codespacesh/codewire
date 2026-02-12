use std::io::Read;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU32, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use dashmap::DashMap;
use portable_pty::{native_pty_system, CommandBuilder, MasterPty, PtySize};
use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, mpsc, watch};
use tracing::{error, info, warn};

use crate::protocol::SessionInfo;

/// Channels returned by [`SessionManager::attach`].
pub type AttachChannels = (
    broadcast::Receiver<Vec<u8>>,
    mpsc::Sender<Vec<u8>>,
    watch::Receiver<SessionStatus>,
);

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum SessionStatus {
    Running,
    Completed(i32),
    Killed,
}

impl std::fmt::Display for SessionStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SessionStatus::Running => write!(f, "running"),
            SessionStatus::Completed(code) => write!(f, "completed ({code})"),
            SessionStatus::Killed => write!(f, "killed"),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionMeta {
    pub id: u32,
    pub prompt: String,
    pub working_dir: PathBuf,
    pub created_at: DateTime<Utc>,
    pub status: SessionStatus,
    pub pid: Option<u32>,
}

/// A live session with its PTY handles and communication channels.
pub struct Session {
    pub meta: SessionMeta,
    /// PTY master — used for resize. Wrapped in Mutex because MasterPty is !Sync
    master: Arc<Mutex<Box<dyn MasterPty + Send>>>,
    /// Number of clients currently attached.
    pub attached_count: Arc<AtomicUsize>,
    /// Broadcast sender: PTY output goes here, clients subscribe.
    pub broadcast_tx: broadcast::Sender<Vec<u8>>,
    /// Send input data to the PTY via this channel.
    pub pty_input_tx: mpsc::Sender<Vec<u8>>,
    /// Watch channel for session status changes.
    pub status_tx: watch::Sender<SessionStatus>,
    pub status_rx: watch::Receiver<SessionStatus>,
    /// Path to output log file
    log_path: PathBuf,
}

// ---------------------------------------------------------------------------
// Session Manager
// ---------------------------------------------------------------------------

static NEXT_ID: AtomicU32 = AtomicU32::new(1);

pub struct SessionManager {
    sessions: Arc<DashMap<u32, Session>>,
    data_dir: PathBuf,
    persist_tx: mpsc::UnboundedSender<()>,
}

impl SessionManager {
    pub fn new(data_dir: PathBuf) -> Result<(Self, mpsc::UnboundedReceiver<()>)> {
        std::fs::create_dir_all(&data_dir).context("creating data dir")?;

        // Restore next ID from persisted sessions
        let meta_path = data_dir.join("sessions.json");
        if meta_path.exists() {
            let data = std::fs::read_to_string(&meta_path)?;
            let metas: Vec<SessionMeta> = match serde_json::from_str(&data) {
                Ok(m) => m,
                Err(e) => {
                    // Backup corrupt file
                    let timestamp = Utc::now().format("%Y%m%d_%H%M%S");
                    let backup_path =
                        meta_path.with_extension(format!("json.corrupt.{}", timestamp));
                    if let Err(backup_err) = std::fs::copy(&meta_path, &backup_path) {
                        error!(?backup_err, "Failed to backup corrupt sessions.json");
                    } else {
                        info!(?backup_path, "Backed up corrupt sessions.json");
                    }

                    error!(
                        ?e,
                        "Corrupt sessions.json file - starting with empty session list"
                    );
                    eprintln!("[cw] ERROR: sessions.json is corrupted and could not be parsed");
                    eprintln!("[cw] A backup has been saved to: {}", backup_path.display());
                    eprintln!("[cw] Starting with empty session list");
                    Vec::new()
                }
            };
            let max_id = metas.iter().map(|m| m.id).max().unwrap_or(0);
            NEXT_ID.store(max_id + 1, Ordering::SeqCst);
        }

        let (persist_tx, persist_rx) = mpsc::unbounded_channel();

        Ok((
            Self {
                sessions: Arc::new(DashMap::new()),
                data_dir,
                persist_tx,
            },
            persist_rx,
        ))
    }

    /// Launch a new session with the given command.
    pub fn launch(&self, command: Vec<String>, working_dir: String) -> Result<u32> {
        if command.is_empty() {
            bail!("command must not be empty");
        }

        // Validate command exists (check if it's in PATH or is an absolute path)
        let cmd_path = PathBuf::from(&command[0]);
        if !cmd_path.is_absolute() {
            // Check if command exists in PATH
            if let Ok(path_var) = std::env::var("PATH") {
                let found = path_var
                    .split(':')
                    .any(|dir| PathBuf::from(dir).join(&command[0]).exists());
                if !found {
                    bail!("Command '{}' not found in PATH", command[0]);
                }
            }
        } else if !cmd_path.exists() {
            bail!("Command '{}' does not exist", command[0]);
        }

        let id = NEXT_ID.fetch_add(1, Ordering::SeqCst);
        let work_dir = PathBuf::from(&working_dir);

        // Validate working directory exists
        if !work_dir.exists() {
            bail!("Working directory '{}' does not exist", working_dir);
        }
        if !work_dir.is_dir() {
            bail!("Working directory '{}' is not a directory", working_dir);
        }

        // Ensure session log directory
        let log_dir = self.data_dir.join("sessions").join(id.to_string());
        std::fs::create_dir_all(&log_dir)?;
        let log_path = log_dir.join("output.log");

        // Open PTY
        let pty_system = native_pty_system();
        let pair = pty_system
            .openpty(PtySize {
                rows: 24,
                cols: 80,
                pixel_width: 0,
                pixel_height: 0,
            })
            .context("opening PTY")?;

        let display_command = command.join(" ");

        let mut cmd = CommandBuilder::new(&command[0]);
        for arg in &command[1..] {
            cmd.arg(arg);
        }
        if work_dir.exists() {
            cmd.cwd(&work_dir);
        }

        let mut child = pair.slave.spawn_command(cmd).context("spawning process")?;
        let pid = child.process_id();
        drop(pair.slave);

        // Extract reader and writer from PTY master
        let mut master_reader = pair
            .master
            .try_clone_reader()
            .context("cloning PTY reader")?;
        let mut master_writer = pair.master.take_writer().context("taking PTY writer")?;

        // Channels
        let (broadcast_tx, _) = broadcast::channel::<Vec<u8>>(4096);
        let (pty_input_tx, mut pty_input_rx) = mpsc::channel::<Vec<u8>>(256);
        let (status_tx, status_rx) = watch::channel(SessionStatus::Running);

        let meta = SessionMeta {
            id,
            prompt: display_command,
            working_dir: work_dir,
            created_at: Utc::now(),
            status: SessionStatus::Running,
            pid,
        };

        let session = Session {
            meta: meta.clone(),
            master: Arc::new(Mutex::new(pair.master)),
            attached_count: Arc::new(AtomicUsize::new(0)),
            broadcast_tx: broadcast_tx.clone(),
            pty_input_tx: pty_input_tx.clone(),
            status_tx: status_tx.clone(),
            status_rx: status_rx.clone(),
            log_path: log_path.clone(),
        };

        self.sessions.insert(id, session);

        // --- Background task: read PTY output, tee to log + broadcast ---
        let broadcast_tx_clone = broadcast_tx.clone();
        let status_rx_reader = status_rx.clone();
        let log_path_async = log_path.clone();
        tokio::spawn(async move {
            use tokio::io::AsyncWriteExt;

            let mut log_file = match tokio::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&log_path_async)
                .await
            {
                Ok(f) => Some(f),
                Err(e) => {
                    error!(
                        session_id = id,
                        ?e,
                        ?log_path_async,
                        "Failed to open session log file"
                    );
                    None
                }
            };

            // Spawn the blocking PTY reader in a dedicated task
            // Use a channel to receive data from the blocking reader
            let (pty_output_tx, mut pty_output_rx) = mpsc::unbounded_channel::<Vec<u8>>();

            tokio::task::spawn_blocking(move || {
                let mut buf = [0u8; 4096];
                loop {
                    if *status_rx_reader.borrow() != SessionStatus::Running {
                        break;
                    }

                    match master_reader.read(&mut buf) {
                        Ok(0) => break,
                        Ok(n) => {
                            let data = buf[..n].to_vec();
                            // Send to async task; if channel closed, reader exited
                            if pty_output_tx.send(data).is_err() {
                                break;
                            }
                        }
                        Err(e) => {
                            // EIO (errno 5) means slave closed
                            if e.raw_os_error() == Some(nix::libc::EIO) {
                                break;
                            }
                            error!(id, ?e, "PTY read error");
                            break;
                        }
                    }
                }
                info!(id, "output reader exited");
            });

            // Async task: receive data, write to log file (async), broadcast
            while let Some(data) = pty_output_rx.recv().await {
                if let Some(ref mut f) = log_file {
                    if let Err(e) = f.write_all(&data).await {
                        error!(session_id = id, ?e, "Failed to write to log file");
                    }
                    let _ = f.flush().await;
                }
                // Broadcast — ok if no receivers
                let _ = broadcast_tx_clone.send(data);
            }
        });

        // --- Background task: forward client input to PTY ---
        tokio::task::spawn_blocking(move || {
            use std::io::Write;
            while let Some(data) = pty_input_rx.blocking_recv() {
                if let Err(e) = master_writer.write_all(&data) {
                    error!(id, ?e, "PTY write error");
                    break;
                }
                let _ = master_writer.flush();
            }
            info!(id, "input writer exited");
        });

        // --- Background task: wait for child process exit ---
        let status_tx_waiter = status_tx.clone();
        tokio::task::spawn_blocking(move || match child.wait() {
            Ok(exit) => {
                let code = exit.exit_code() as i32;
                info!(id, code, "session process exited");
                let _ = status_tx_waiter.send(SessionStatus::Completed(code));
            }
            Err(e) => {
                error!(id, ?e, "waiting for child");
                let _ = status_tx_waiter.send(SessionStatus::Completed(-1));
            }
        });

        info!(id, "session launched");
        let _ = self.persist_tx.send(());
        Ok(id)
    }

    pub fn list(&self) -> Vec<SessionInfo> {
        let mut sessions: Vec<SessionInfo> = self
            .sessions
            .iter()
            .map(|entry| {
                let s = entry.value();
                let status = s.status_rx.borrow().clone();
                let attached = s.attached_count.load(Ordering::SeqCst) > 0;
                SessionInfo {
                    id: s.meta.id,
                    prompt: s.meta.prompt.clone(),
                    working_dir: s.meta.working_dir.display().to_string(),
                    created_at: s.meta.created_at.to_rfc3339(),
                    status: status.to_string(),
                    attached,
                    pid: s.meta.pid,
                    output_size_bytes: None,
                    last_output_snippet: None,
                }
            })
            .collect();
        sessions.sort_by_key(|s| s.id);
        sessions
    }

    /// Attach a client. Returns (broadcast_rx, pty_input_tx, status_rx).
    pub fn attach(&self, id: u32) -> Result<AttachChannels> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        if *session.status_rx.borrow() != SessionStatus::Running {
            bail!("session {id} is not running");
        }

        // Allow multiple attachments
        session.attached_count.fetch_add(1, Ordering::SeqCst);
        let rx = session.broadcast_tx.subscribe();
        let tx = session.pty_input_tx.clone();
        let status = session.status_rx.clone();

        Ok((rx, tx, status))
    }

    pub fn detach(&self, id: u32) -> Result<()> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;
        session.attached_count.fetch_sub(1, Ordering::SeqCst);
        Ok(())
    }

    pub fn resize(&self, id: u32, cols: u16, rows: u16) -> Result<()> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;
        session
            .master
            .lock()
            .unwrap()
            .resize(PtySize {
                rows,
                cols,
                pixel_width: 0,
                pixel_height: 0,
            })
            .context("resizing PTY")?;
        Ok(())
    }

    pub fn kill(&self, id: u32) -> Result<()> {
        let mut session = self
            .sessions
            .get_mut(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        let _ = session.status_tx.send(SessionStatus::Killed);

        if let Some(pid) = session.meta.pid {
            let _ = nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(pid as i32),
                nix::sys::signal::Signal::SIGTERM,
            );
        }

        session.meta.status = SessionStatus::Killed;
        let _ = self.persist_tx.send(());
        Ok(())
    }

    pub fn kill_all(&self) -> usize {
        let ids: Vec<u32> = self
            .sessions
            .iter()
            .filter(|entry| *entry.value().status_rx.borrow() == SessionStatus::Running)
            .map(|entry| *entry.key())
            .collect();
        let count = ids.len();
        for id in ids {
            let _ = self.kill(id);
        }
        count
    }

    pub fn log_path(&self, id: u32) -> Result<PathBuf> {
        if !self.sessions.contains_key(&id) {
            bail!("session {id} not found");
        }
        Ok(self
            .data_dir
            .join("sessions")
            .join(id.to_string())
            .join("output.log"))
    }

    /// Update session statuses from their watch channels.
    pub fn refresh_statuses(&self) {
        let mut changed = false;
        for mut entry in self.sessions.iter_mut() {
            let session = entry.value_mut();
            let current = session.status_rx.borrow().clone();
            if session.meta.status != current {
                session.meta.status = current;
                changed = true;
            }
        }
        if changed {
            let _ = self.persist_tx.send(());
        }
    }

    pub fn persist_meta(&self) {
        let metas: Vec<SessionMeta> = self
            .sessions
            .iter()
            .map(|entry| entry.value().meta.clone())
            .collect();
        let path = self.data_dir.join("sessions.json");
        match serde_json::to_string_pretty(&metas) {
            Ok(json) => {
                if let Err(e) = std::fs::write(&path, json) {
                    error!(?e, ?path, "Failed to persist session metadata");
                }
            }
            Err(e) => {
                error!(?e, "Failed to serialize session metadata to JSON");
            }
        }
    }

    /// Send input to a session without attaching
    pub fn send_input(&self, id: u32, data: Vec<u8>) -> Result<usize> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        let bytes = data.len();
        // Use try_send to avoid blocking if channel is full
        session
            .pty_input_tx
            .try_send(data)
            .context("sending input to session")?;
        Ok(bytes)
    }

    /// Get detailed status for a session
    pub fn get_status(&self, id: u32) -> Result<(SessionInfo, u64)> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        let status = session.status_rx.borrow().clone();
        let attached = session.attached_count.load(Ordering::SeqCst) > 0;

        // Get output size from log file
        let output_size = if session.log_path.exists() {
            std::fs::metadata(&session.log_path)
                .map(|m| m.len())
                .unwrap_or(0)
        } else {
            0
        };

        // Get last few lines of output as snippet
        let snippet = if session.log_path.exists() {
            match std::fs::read_to_string(&session.log_path) {
                Ok(content) => {
                    let lines: Vec<&str> = content.lines().rev().take(5).collect();
                    if lines.is_empty() {
                        None
                    } else {
                        Some(lines.into_iter().rev().collect::<Vec<_>>().join("\n"))
                    }
                }
                Err(e) => {
                    warn!(session_id = id, ?e, "Failed to read log file for snippet");
                    None
                }
            }
        } else {
            None
        };

        let info = SessionInfo {
            id: session.meta.id,
            prompt: session.meta.prompt.clone(),
            working_dir: session.meta.working_dir.display().to_string(),
            created_at: session.meta.created_at.to_rfc3339(),
            status: status.to_string(),
            attached,
            pid: session.meta.pid,
            output_size_bytes: Some(output_size),
            last_output_snippet: snippet,
        };

        Ok((info, output_size))
    }

    /// Subscribe to a session's output without attaching
    pub fn subscribe_output(&self, id: u32) -> Result<broadcast::Receiver<Vec<u8>>> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        Ok(session.broadcast_tx.subscribe())
    }

    /// Get a session's status watcher
    pub fn subscribe_status(&self, id: u32) -> Result<watch::Receiver<SessionStatus>> {
        let session = self
            .sessions
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        Ok(session.status_rx.clone())
    }
}
