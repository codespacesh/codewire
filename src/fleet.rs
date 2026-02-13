use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result};
use futures::StreamExt;
use tracing::{info, warn};

use crate::config::{DaemonConfig, NatsConfig};
use crate::protocol::{DaemonInfo, FleetRequest, FleetResponse};
use crate::session::SessionManager;

/// Connect to NATS using the provided config.
pub async fn connect_nats(config: &NatsConfig) -> Result<async_nats::Client> {
    let mut opts = async_nats::ConnectOptions::new().event_callback(|event| async move {
        match event {
            async_nats::Event::Connected => info!("NATS: connected"),
            async_nats::Event::Disconnected => warn!("NATS: disconnected"),
            async_nats::Event::LameDuckMode => warn!("NATS: lame duck mode"),
            _ => {}
        }
    });

    if let Some(ref token) = config.token {
        opts = opts.token(token.clone());
    }
    if let Some(ref creds) = config.creds_file {
        opts = opts
            .credentials_file(creds)
            .await
            .context("loading NATS credentials file")?;
    }

    opts.connect(&config.url)
        .await
        .context("connecting to NATS")
}

/// Run the fleet integration (called from Daemon::run).
pub async fn run_fleet(
    nats_config: &NatsConfig,
    daemon_config: &DaemonConfig,
    manager: Arc<SessionManager>,
) -> Result<()> {
    let client = connect_nats(nats_config).await?;
    let start_time = Instant::now();
    let name = &daemon_config.name;

    info!(name, "NATS fleet registered");

    // Subscribe to fleet discovery and direct requests
    let mut discover_sub = client.subscribe("cw.fleet.discover").await?;
    let mut direct_sub = client.subscribe(format!("cw.{}.>", name)).await?;

    // Spawn heartbeat
    let hb_client = client.clone();
    let hb_config = daemon_config.clone();
    let hb_manager = manager.clone();
    tokio::spawn(async move {
        heartbeat_loop(hb_client, &hb_config, &hb_manager, start_time).await;
    });

    // Main message loop
    loop {
        tokio::select! {
            Some(msg) = discover_sub.next() => {
                handle_message(msg, daemon_config, &manager, start_time, &client).await;
            }
            Some(msg) = direct_sub.next() => {
                handle_message(msg, daemon_config, &manager, start_time, &client).await;
            }
        }
    }
}

async fn handle_message(
    msg: async_nats::Message,
    daemon_config: &DaemonConfig,
    manager: &Arc<SessionManager>,
    start_time: Instant,
    client: &async_nats::Client,
) {
    let Ok(req) = serde_json::from_slice::<FleetRequest>(&msg.payload) else {
        warn!("invalid fleet request payload");
        return;
    };

    let name = &daemon_config.name;
    let response = match req {
        FleetRequest::Discover | FleetRequest::ListSessions => {
            let sessions = manager.list();
            if matches!(req, FleetRequest::Discover) {
                FleetResponse::DaemonInfo(DaemonInfo {
                    name: name.clone(),
                    external_url: daemon_config.external_url.clone(),
                    sessions,
                    uptime_secs: start_time.elapsed().as_secs(),
                })
            } else {
                FleetResponse::SessionList {
                    daemon: name.clone(),
                    sessions,
                }
            }
        }
        FleetRequest::Launch {
            command,
            working_dir,
        } => match manager.launch(command, working_dir) {
            Ok(id) => FleetResponse::Launched {
                daemon: name.clone(),
                id,
            },
            Err(e) => FleetResponse::Error {
                daemon: name.clone(),
                message: e.to_string(),
            },
        },
        FleetRequest::Kill { id } => match manager.kill(id) {
            Ok(()) => FleetResponse::Killed {
                daemon: name.clone(),
                id,
            },
            Err(e) => FleetResponse::Error {
                daemon: name.clone(),
                message: e.to_string(),
            },
        },
        FleetRequest::GetStatus { id } => match manager.get_status(id) {
            Ok((info, output_size)) => FleetResponse::SessionStatus {
                daemon: name.clone(),
                info,
                output_size,
            },
            Err(e) => FleetResponse::Error {
                daemon: name.clone(),
                message: e.to_string(),
            },
        },
    };

    if let Some(reply) = msg.reply {
        if let Ok(payload) = serde_json::to_vec(&response) {
            let _ = client.publish(reply, payload.into()).await;
        }
    }
}

async fn heartbeat_loop(
    client: async_nats::Client,
    daemon_config: &DaemonConfig,
    manager: &Arc<SessionManager>,
    start_time: Instant,
) {
    let mut interval = tokio::time::interval(std::time::Duration::from_secs(30));
    loop {
        interval.tick().await;
        let info = DaemonInfo {
            name: daemon_config.name.clone(),
            external_url: daemon_config.external_url.clone(),
            sessions: manager.list(),
            uptime_secs: start_time.elapsed().as_secs(),
        };
        if let Ok(payload) = serde_json::to_vec(&info) {
            let _ = client.publish("cw.fleet.heartbeat", payload.into()).await;
        }
    }
}
