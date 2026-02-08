use std::collections::HashMap;
use std::io::Read;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU32, Ordering};

use anyhow::{Context, Result, bail};
use chrono::{DateTime, Utc};
use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, mpsc, watch};
use tracing::{error, info};

use crate::protocol::SessionInfo;

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
    /// PTY master — used for resize.
    master: Box<dyn MasterPty + Send>,
    /// Whether a client is currently attached.
    pub attached: bool,
    /// Broadcast sender: PTY output goes here, clients subscribe.
    pub broadcast_tx: broadcast::Sender<Vec<u8>>,
    /// Send input data to the PTY via this channel.
    pub pty_input_tx: mpsc::Sender<Vec<u8>>,
    /// Watch channel for session status changes.
    pub status_tx: watch::Sender<SessionStatus>,
    pub status_rx: watch::Receiver<SessionStatus>,
}

// ---------------------------------------------------------------------------
// Session Manager
// ---------------------------------------------------------------------------

static NEXT_ID: AtomicU32 = AtomicU32::new(1);

pub struct SessionManager {
    sessions: HashMap<u32, Session>,
    data_dir: PathBuf,
    /// Command to run. Defaults to "claude".
    command: String,
    /// Extra args prepended before the prompt. Defaults to ["--dangerously-skip-permissions", "-p"].
    command_args: Vec<String>,
}

impl SessionManager {
    pub fn new(data_dir: PathBuf) -> Result<Self> {
        std::fs::create_dir_all(&data_dir).context("creating data dir")?;

        // Restore next ID from persisted sessions
        let meta_path = data_dir.join("sessions.json");
        if meta_path.exists() {
            let data = std::fs::read_to_string(&meta_path)?;
            let metas: Vec<SessionMeta> = serde_json::from_str(&data).unwrap_or_default();
            let max_id = metas.iter().map(|m| m.id).max().unwrap_or(0);
            NEXT_ID.store(max_id + 1, Ordering::SeqCst);
        }

        Ok(Self {
            sessions: HashMap::new(),
            data_dir,
            command: "claude".to_string(),
            command_args: vec![
                "--dangerously-skip-permissions".to_string(),
                "-p".to_string(),
            ],
        })
    }

    /// Override the command and args (for testing).
    pub fn set_command(&mut self, command: String, args: Vec<String>) {
        self.command = command;
        self.command_args = args;
    }

    /// Launch a new session running `claude` with the given prompt.
    pub fn launch(&mut self, prompt: String, working_dir: String) -> Result<u32> {
        let id = NEXT_ID.fetch_add(1, Ordering::SeqCst);
        let work_dir = PathBuf::from(&working_dir);

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

        // Spawn process
        let mut cmd = CommandBuilder::new(&self.command);
        for arg in &self.command_args {
            cmd.arg(arg);
        }
        cmd.arg(&prompt);
        if work_dir.exists() {
            cmd.cwd(&work_dir);
        }

        let mut child = pair.slave.spawn_command(cmd).context("spawning claude")?;
        let pid = child.process_id();
        drop(pair.slave);

        // Extract reader and writer from PTY master
        let mut master_reader = pair
            .master
            .try_clone_reader()
            .context("cloning PTY reader")?;
        let mut master_writer = pair
            .master
            .take_writer()
            .context("taking PTY writer")?;

        // Channels
        let (broadcast_tx, _) = broadcast::channel::<Vec<u8>>(4096);
        let (pty_input_tx, mut pty_input_rx) = mpsc::channel::<Vec<u8>>(256);
        let (status_tx, status_rx) = watch::channel(SessionStatus::Running);

        let meta = SessionMeta {
            id,
            prompt,
            working_dir: work_dir,
            created_at: Utc::now(),
            status: SessionStatus::Running,
            pid,
        };

        let session = Session {
            meta: meta.clone(),
            master: pair.master,
            attached: false,
            broadcast_tx: broadcast_tx.clone(),
            pty_input_tx: pty_input_tx.clone(),
            status_tx: status_tx.clone(),
            status_rx: status_rx.clone(),
        };

        self.sessions.insert(id, session);

        // --- Background task: read PTY output, tee to log + broadcast ---
        let broadcast_tx_clone = broadcast_tx.clone();
        let status_rx_reader = status_rx.clone();
        std::thread::spawn(move || {
            let mut log_file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&log_path)
                .ok();

            let mut buf = [0u8; 4096];
            loop {
                if *status_rx_reader.borrow() != SessionStatus::Running {
                    break;
                }

                match master_reader.read(&mut buf) {
                    Ok(0) => break,
                    Ok(n) => {
                        let data = buf[..n].to_vec();
                        if let Some(ref mut f) = log_file {
                            let _ = std::io::Write::write_all(f, &data);
                            let _ = std::io::Write::flush(f);
                        }
                        // Broadcast — ok if no receivers
                        let _ = broadcast_tx_clone.send(data);
                    }
                    Err(e) => {
                        // EIO means slave closed
                        if e.raw_os_error() == Some(5) {
                            break;
                        }
                        error!(id, ?e, "PTY read error");
                        break;
                    }
                }
            }
            info!(id, "output reader exited");
        });

        // --- Background task: forward client input to PTY ---
        std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                while let Some(data) = pty_input_rx.recv().await {
                    if std::io::Write::write_all(&mut master_writer, &data).is_err() {
                        break;
                    }
                    let _ = std::io::Write::flush(&mut master_writer);
                }
            });
            info!(id, "input writer exited");
        });

        // --- Background task: wait for child process exit ---
        let status_tx_waiter = status_tx.clone();
        std::thread::spawn(move || {
            match child.wait() {
                Ok(exit) => {
                    let code = exit.exit_code() as i32;
                    info!(id, code, "session process exited");
                    let _ = status_tx_waiter.send(SessionStatus::Completed(code));
                }
                Err(e) => {
                    error!(id, ?e, "waiting for child");
                    let _ = status_tx_waiter.send(SessionStatus::Completed(-1));
                }
            }
        });

        info!(id, "session launched");
        self.persist_meta();
        Ok(id)
    }

    pub fn list(&self) -> Vec<SessionInfo> {
        let mut sessions: Vec<SessionInfo> = self
            .sessions
            .values()
            .map(|s| {
                let status = s.status_rx.borrow().clone();
                SessionInfo {
                    id: s.meta.id,
                    prompt: s.meta.prompt.clone(),
                    working_dir: s.meta.working_dir.display().to_string(),
                    created_at: s.meta.created_at.to_rfc3339(),
                    status: status.to_string(),
                    attached: s.attached,
                }
            })
            .collect();
        sessions.sort_by_key(|s| s.id);
        sessions
    }

    /// Attach a client. Returns (broadcast_rx, pty_input_tx, status_rx).
    pub fn attach(
        &mut self,
        id: u32,
    ) -> Result<(
        broadcast::Receiver<Vec<u8>>,
        mpsc::Sender<Vec<u8>>,
        watch::Receiver<SessionStatus>,
    )> {
        let session = self
            .sessions
            .get_mut(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;

        if *session.status_rx.borrow() != SessionStatus::Running {
            bail!("session {id} is not running");
        }

        if session.attached {
            bail!("session {id} already has a client attached");
        }

        session.attached = true;
        let rx = session.broadcast_tx.subscribe();
        let tx = session.pty_input_tx.clone();
        let status = session.status_rx.clone();

        Ok((rx, tx, status))
    }

    pub fn detach(&mut self, id: u32) -> Result<()> {
        let session = self
            .sessions
            .get_mut(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;
        session.attached = false;
        Ok(())
    }

    pub fn resize(&mut self, id: u32, cols: u16, rows: u16) -> Result<()> {
        let session = self
            .sessions
            .get_mut(&id)
            .ok_or_else(|| anyhow::anyhow!("session {id} not found"))?;
        session
            .master
            .resize(PtySize {
                rows,
                cols,
                pixel_width: 0,
                pixel_height: 0,
            })
            .context("resizing PTY")?;
        Ok(())
    }

    pub fn kill(&mut self, id: u32) -> Result<()> {
        let session = self
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
        self.persist_meta();
        Ok(())
    }

    pub fn kill_all(&mut self) -> usize {
        let ids: Vec<u32> = self
            .sessions
            .iter()
            .filter(|(_, s)| *s.status_rx.borrow() == SessionStatus::Running)
            .map(|(id, _)| *id)
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
    pub fn refresh_statuses(&mut self) {
        for session in self.sessions.values_mut() {
            let current = session.status_rx.borrow().clone();
            if session.meta.status != current {
                session.meta.status = current;
            }
        }
        self.persist_meta();
    }

    fn persist_meta(&self) {
        let metas: Vec<&SessionMeta> = self.sessions.values().map(|s| &s.meta).collect();
        let path = self.data_dir.join("sessions.json");
        if let Ok(json) = serde_json::to_string_pretty(&metas) {
            let _ = std::fs::write(path, json);
        }
    }
}
