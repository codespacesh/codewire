mod auth;
mod client;
mod config;
mod connection;
mod daemon;
mod protocol;
mod session;
mod terminal;

#[cfg(feature = "nats")]
mod fleet;
#[cfg(feature = "nats")]
mod fleet_client;

#[cfg(feature = "mcp")]
mod mcp_server;

use std::path::PathBuf;

use anyhow::{bail, Context, Result};
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
    /// Connect to a remote server (name from servers.toml or ws://host:port)
    #[arg(long, short, global = true)]
    server: Option<String>,

    /// Auth token for remote server (overrides saved token)
    #[arg(long, global = true)]
    token: Option<String>,

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

    /// Fleet discovery and remote management via NATS
    #[cfg(feature = "nats")]
    Fleet {
        #[command(subcommand)]
        action: FleetAction,
    },

    /// Manage remote server connections
    Server {
        #[command(subcommand)]
        action: ServerAction,
    },
}

#[cfg(feature = "nats")]
#[derive(Subcommand)]
enum FleetAction {
    /// List all nodes and their sessions across the fleet
    List {
        /// Discovery timeout in seconds
        #[arg(long, default_value = "2")]
        timeout: u64,
        /// Output as JSON
        #[arg(long)]
        json: bool,
    },
    /// Attach to a remote session: cw fleet attach <node>:<session_id>
    Attach {
        /// Target in <node>:<session_id> format
        target: String,
    },
    /// Launch a session on a specific node
    Launch {
        /// Node name to launch on
        #[arg(long)]
        on: String,
        /// Working directory on the remote node
        #[arg(long, short)]
        dir: Option<String>,
        /// Command and arguments
        #[arg(trailing_var_arg = true, num_args = 1..)]
        command: Vec<String>,
    },
    /// Kill a remote session: cw fleet kill <node>:<session_id>
    Kill {
        /// Target in <node>:<session_id> format
        target: String,
    },
    /// Send input to a remote session: cw fleet send <node>:<session_id> <text>
    Send {
        /// Target in <node>:<session_id> format
        target: String,
        /// Text to send
        input: String,
    },
}

#[derive(Subcommand)]
enum ServerAction {
    /// Add a remote server
    Add {
        /// Server name (for later reference)
        name: String,
        /// WebSocket URL (e.g. ws://host:9100)
        url: String,
        /// Auth token
        #[arg(long)]
        token: String,
    },
    /// Remove a saved server
    Remove {
        /// Server name
        name: String,
    },
    /// List saved servers
    List,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    let dir = data_dir();
    let target = resolve_target(&cli, &dir)?;
    let is_local = matches!(target, client::Target::Local(_));

    match cli.command {
        Commands::Daemon | Commands::Start => {
            tracing_subscriber::fmt()
                .with_env_filter(
                    EnvFilter::from_default_env().add_directive("codewire=info".parse()?),
                )
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

            let pid_str = std::fs::read_to_string(&pid_file).context("reading daemon PID file")?;
            let pid: i32 = pid_str.trim().parse().context("parsing daemon PID")?;

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

        Commands::Launch {
            dir: work_dir,
            command,
        } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            let work_dir = work_dir.unwrap_or_else(|| {
                std::env::current_dir()
                    .map(|p| p.display().to_string())
                    .unwrap_or_else(|_| ".".to_string())
            });
            client::launch(&target, command, work_dir).await
        }

        Commands::List { json } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::list(&target, json).await
        }

        Commands::Attach { id } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::attach(&target, id).await
        }

        Commands::Kill { id, all } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            if all {
                client::kill_all(&target).await
            } else if let Some(id) = id {
                client::kill(&target, id).await
            } else {
                bail!("Must specify either a session ID or --all to kill all sessions.\nUsage: cw kill <ID> or cw kill --all")
            }
        }

        Commands::Logs { id, follow, tail } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::logs(&target, id, follow, tail).await
        }

        Commands::Send {
            id,
            input,
            stdin,
            file,
            no_newline,
        } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::send_input(&target, id, input, stdin, file, no_newline).await
        }

        Commands::Watch {
            id,
            tail,
            no_history,
            timeout,
        } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::watch_session(&target, id, tail, no_history, timeout).await
        }

        Commands::Status { id, json } => {
            if is_local {
                ensure_daemon(&dir).await?;
            }
            client::get_status(&target, id, json).await
        }

        #[cfg(feature = "mcp")]
        Commands::McpServer => {
            ensure_daemon(&dir).await?;
            mcp_server::run_mcp_server(dir).await
        }

        #[cfg(feature = "nats")]
        Commands::Fleet { action } => handle_fleet_action(action, &dir).await,

        Commands::Server { action } => handle_server_action(action, &dir),
    }
}

#[cfg(feature = "nats")]
async fn handle_fleet_action(action: FleetAction, data_dir: &std::path::Path) -> Result<()> {
    let config = config::Config::load(data_dir)?;
    let nats_config = config.nats.as_ref().ok_or_else(|| {
        anyhow::anyhow!(
            "NATS not configured. Set CODEWIRE_NATS_URL or add [nats] to ~/.codewire/config.toml"
        )
    })?;

    match action {
        FleetAction::List { timeout, json } => {
            fleet_client::handle_fleet_list(nats_config, timeout, json).await
        }
        FleetAction::Attach { target } => {
            fleet_client::handle_fleet_attach(nats_config, data_dir, &target).await
        }
        FleetAction::Launch { on, dir, command } => {
            let working_dir = dir.unwrap_or_else(|| ".".to_string());
            fleet_client::handle_fleet_launch(nats_config, &on, command, working_dir).await
        }
        FleetAction::Kill { target } => fleet_client::handle_fleet_kill(nats_config, &target).await,
        FleetAction::Send { target, input } => {
            fleet_client::handle_fleet_send_input(nats_config, &target, input.into_bytes()).await
        }
    }
}

/// Resolve connection target from CLI args.
fn resolve_target(cli: &Cli, data_dir: &std::path::Path) -> Result<client::Target> {
    match &cli.server {
        None => Ok(client::Target::Local(data_dir.to_path_buf())),
        Some(server) => {
            // Check if it's a saved server name
            let servers = config::ServersConfig::load(data_dir)?;
            if let Some(entry) = servers.servers.get(server) {
                let token = cli.token.clone().unwrap_or_else(|| entry.token.clone());
                Ok(client::Target::Remote {
                    url: entry.url.clone(),
                    token,
                })
            } else {
                // Treat as URL or host:port
                let token = cli
                    .token
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("--token required for ad-hoc server"))?;
                let url = if server.starts_with("ws://") || server.starts_with("wss://") {
                    server.clone()
                } else {
                    format!("ws://{}", server)
                };
                Ok(client::Target::Remote { url, token })
            }
        }
    }
}

/// Handle server management subcommands.
fn handle_server_action(action: ServerAction, data_dir: &std::path::Path) -> Result<()> {
    match action {
        ServerAction::Add { name, url, token } => {
            let mut servers = config::ServersConfig::load(data_dir)?;
            servers
                .servers
                .insert(name.clone(), config::ServerEntry { url, token });
            servers.save(data_dir)?;
            println!("Server '{}' saved.", name);
            Ok(())
        }
        ServerAction::Remove { name } => {
            let mut servers = config::ServersConfig::load(data_dir)?;
            if servers.servers.remove(&name).is_some() {
                servers.save(data_dir)?;
                println!("Server '{}' removed.", name);
            } else {
                println!("Server '{}' not found.", name);
            }
            Ok(())
        }
        ServerAction::List => {
            let servers = config::ServersConfig::load(data_dir)?;
            if servers.servers.is_empty() {
                println!("No saved servers.");
            } else {
                println!("{:<15} URL", "NAME");
                println!("{}", "-".repeat(50));
                for (name, entry) in &servers.servers {
                    println!("{:<15} {}", name, entry.url);
                }
            }
            Ok(())
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
        if sock.exists() && tokio::net::UnixStream::connect(&sock).await.is_ok() {
            return Ok(());
        }
    }

    bail!("daemon failed to start (socket not available after 5s)")
}
