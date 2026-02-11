mod client;
mod daemon;
mod protocol;
mod session;
mod terminal;

#[cfg(feature = "mcp")]
mod mcp_server;

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use tracing_subscriber::EnvFilter;

fn data_dir() -> PathBuf {
    dirs_path().unwrap_or_else(|| {
        eprintln!("[cw] ERROR: $HOME environment variable is not set");
        eprintln!("[cw] WARNING: Using insecure fallback directory /tmp/.codewire");
        eprintln!("[cw] WARNING: This directory is world-readable and may be cleared on reboot");
        tracing::error!("HOME environment variable not set, using insecure /tmp fallback");
        PathBuf::from("/tmp/.codewire")
    })
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

    /// Start the daemon (alias for daemon)
    Start,

    /// Stop the daemon
    Stop,

    /// Launch a new session: cw launch [--dir <dir>] -- <command> [args...]
    Launch {
        /// Working directory (defaults to current dir)
        #[arg(long, short)]
        dir: Option<String>,

        /// Command and arguments to run (everything after --)
        #[arg(trailing_var_arg = true, num_args = 1..)]
        command: Vec<String>,
    },

    /// List all sessions
    List {
        /// Output as JSON
        #[arg(long)]
        json: bool,
    },

    /// Attach to a running session
    Attach {
        /// Session ID (omit to auto-select oldest unattached session)
        id: Option<u32>,
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

    /// Send input to a session without attaching
    Send {
        /// Session ID
        id: u32,

        /// Input text to send
        input: Option<String>,

        /// Read input from stdin
        #[arg(long)]
        stdin: bool,

        /// Read input from file
        #[arg(long, short)]
        file: Option<PathBuf>,

        /// Don't add newline at end
        #[arg(long, short)]
        no_newline: bool,
    },

    /// Watch a session in real-time (monitor without attaching)
    Watch {
        /// Session ID
        id: u32,

        /// Show last N lines of history
        #[arg(long, short)]
        tail: Option<usize>,

        /// Don't show history, only new output
        #[arg(long)]
        no_history: bool,

        /// Auto-exit after N seconds
        #[arg(long)]
        timeout: Option<u64>,
    },

    /// Get detailed session status
    Status {
        /// Session ID
        id: u32,

        /// Output as JSON
        #[arg(long)]
        json: bool,
    },

    /// Start MCP (Model Context Protocol) server
    #[cfg(feature = "mcp")]
    McpServer,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    let dir = data_dir();

    match cli.command {
        Commands::Daemon | Commands::Start => {
            tracing_subscriber::fmt()
                .with_env_filter(EnvFilter::from_default_env().add_directive("codewire=info".parse()?))
                .init();

            let daemon = daemon::Daemon::new(&dir)?;
            daemon.run().await
        }

        Commands::Stop => {
            let pid_file = dir.join("daemon.pid");
            if !pid_file.exists() {
                eprintln!("Daemon is not running (no PID file found)");
                return Ok(());
            }

            let pid_str = std::fs::read_to_string(&pid_file)
                .context("reading daemon PID file")?;
            let pid: i32 = pid_str.trim().parse()
                .context("parsing daemon PID")?;

            // Send SIGTERM to daemon
            use nix::sys::signal::{kill, Signal};
            use nix::unistd::Pid;

            match kill(Pid::from_raw(pid), Signal::SIGTERM) {
                Ok(()) => {
                    eprintln!("Daemon stopped (PID {})", pid);
                    // Clean up PID file
                    let _ = std::fs::remove_file(&pid_file);
                    Ok(())
                }
                Err(nix::errno::Errno::ESRCH) => {
                    eprintln!("Daemon is not running (stale PID file)");
                    let _ = std::fs::remove_file(&pid_file);
                    Ok(())
                }
                Err(e) => {
                    bail!("Failed to stop daemon: {}", e)
                }
            }
        }

        Commands::Launch { dir: work_dir, command } => {
            ensure_daemon(&dir).await?;
            let work_dir = work_dir.unwrap_or_else(|| {
                std::env::current_dir()
                    .map(|p| p.display().to_string())
                    .unwrap_or_else(|_| ".".to_string())
            });
            client::launch(&dir, command, work_dir).await
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
                bail!("Must specify either a session ID or --all to kill all sessions.\nUsage: cw kill <ID> or cw kill --all")
            }
        }

        Commands::Logs { id, follow, tail } => {
            ensure_daemon(&dir).await?;
            client::logs(&dir, id, follow, tail).await
        }

        Commands::Send {
            id,
            input,
            stdin,
            file,
            no_newline,
        } => {
            ensure_daemon(&dir).await?;
            client::send_input(&dir, id, input, stdin, file, no_newline).await
        }

        Commands::Watch {
            id,
            tail,
            no_history,
            timeout,
        } => {
            ensure_daemon(&dir).await?;
            client::watch_session(&dir, id, tail, no_history, timeout).await
        }

        Commands::Status { id, json } => {
            ensure_daemon(&dir).await?;
            client::get_status(&dir, id, json).await
        }

        #[cfg(feature = "mcp")]
        Commands::McpServer => {
            ensure_daemon(&dir).await?;
            mcp_server::run_mcp_server(dir).await
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
