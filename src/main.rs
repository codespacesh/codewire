mod client;
mod daemon;
mod protocol;
mod session;
mod terminal;

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use tracing_subscriber::EnvFilter;

fn data_dir() -> PathBuf {
    dirs_path().unwrap_or_else(|| PathBuf::from("/tmp/.codewire"))
}

fn dirs_path() -> Option<PathBuf> {
    let home = std::env::var("HOME").ok()?;
    Some(PathBuf::from(home).join(".codewire"))
}

#[derive(Parser)]
#[command(name = "cw", about = "Persistent process server for AI coding agents")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Start the daemon (usually auto-started)
    Daemon,

    /// Launch a new session
    Launch {
        /// The prompt to send to the AI agent
        prompt: String,

        /// Working directory (defaults to current dir)
        #[arg(long, short)]
        dir: Option<String>,

        /// Command to run (defaults to "claude")
        #[arg(long, default_value = "claude")]
        cmd: String,
    },

    /// List all sessions
    List {
        /// Output as JSON
        #[arg(long)]
        json: bool,
    },

    /// Attach to a running session
    Attach {
        /// Session ID
        id: u32,
    },

    /// Kill a session
    Kill {
        /// Session ID (omit with --all to kill all)
        id: Option<u32>,

        /// Kill all running sessions
        #[arg(long)]
        all: bool,
    },

    /// View session output logs
    Logs {
        /// Session ID
        id: u32,

        /// Follow output in real-time
        #[arg(long, short)]
        follow: bool,

        /// Show last N lines
        #[arg(long, short)]
        tail: Option<usize>,
    },
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    let dir = data_dir();

    match cli.command {
        Commands::Daemon => {
            tracing_subscriber::fmt()
                .with_env_filter(EnvFilter::from_default_env().add_directive("codewire=info".parse()?))
                .init();

            let daemon = daemon::Daemon::new(&dir)?;
            daemon.run().await
        }

        Commands::Launch { prompt, dir: work_dir, cmd } => {
            ensure_daemon(&dir).await?;
            let work_dir = work_dir.unwrap_or_else(|| {
                std::env::current_dir()
                    .map(|p| p.display().to_string())
                    .unwrap_or_else(|_| ".".to_string())
            });
            client::launch(&dir, prompt, work_dir, cmd).await
        }

        Commands::List { json } => {
            ensure_daemon(&dir).await?;
            client::list(&dir, json).await
        }

        Commands::Attach { id } => {
            ensure_daemon(&dir).await?;
            client::attach(&dir, id).await
        }

        Commands::Kill { id, all } => {
            ensure_daemon(&dir).await?;
            if all {
                client::kill_all(&dir).await
            } else if let Some(id) = id {
                client::kill(&dir, id).await
            } else {
                bail!("specify a session ID or --all")
            }
        }

        Commands::Logs { id, follow, tail } => {
            ensure_daemon(&dir).await?;
            client::logs(&dir, id, follow, tail).await
        }
    }
}

/// Ensure the daemon is running, starting it if necessary.
async fn ensure_daemon(data_dir: &PathBuf) -> Result<()> {
    let sock = data_dir.join("server.sock");
    let _pid_file = data_dir.join("daemon.pid");

    // Check if daemon is already running
    if sock.exists() {
        // Try connecting
        if tokio::net::UnixStream::connect(&sock).await.is_ok() {
            return Ok(());
        }
        // Stale socket — clean up
        let _ = std::fs::remove_file(&sock);
    }

    // Start daemon in background
    std::fs::create_dir_all(data_dir)?;

    let exe = std::env::current_exe().context("finding current executable")?;
    let child = std::process::Command::new(exe)
        .arg("daemon")
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .context("spawning daemon")?;

    eprintln!("[cw] daemon started (pid {})", child.id());

    // Wait for socket to appear
    for _ in 0..50 {
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        if sock.exists() {
            if tokio::net::UnixStream::connect(&sock).await.is_ok() {
                return Ok(());
            }
        }
    }

    bail!("daemon failed to start (socket not available after 5s)")
}
