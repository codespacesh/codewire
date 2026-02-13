use std::time::Duration;

use anyhow::{bail, Context, Result};
use futures::StreamExt;

use crate::config::NatsConfig;
use crate::fleet::connect_nats;
use crate::protocol::{DaemonInfo, FleetRequest, FleetResponse};

/// Scatter-gather fleet discovery: broadcast Discover, collect responses until timeout.
pub async fn discover_fleet(
    client: &async_nats::Client,
    timeout: Duration,
) -> Result<Vec<DaemonInfo>> {
    let inbox = client.new_inbox();
    let mut sub = client.subscribe(inbox.clone()).await?;

    let req = FleetRequest::Discover;
    let payload = serde_json::to_vec(&req)?;
    client
        .publish_with_reply("cw.fleet.discover", inbox, payload.into())
        .await?;

    let mut daemons = Vec::new();
    let deadline = tokio::time::Instant::now() + timeout;

    loop {
        tokio::select! {
            msg = sub.next() => {
                if let Some(msg) = msg {
                    if let Ok(FleetResponse::DaemonInfo(info)) = serde_json::from_slice(&msg.payload) {
                        daemons.push(info);
                    }
                } else {
                    break;
                }
            }
            _ = tokio::time::sleep_until(deadline) => break,
        }
    }
    Ok(daemons)
}

/// Send a request to a specific daemon via NATS request-reply.
pub async fn fleet_request(
    client: &async_nats::Client,
    daemon_name: &str,
    req: &FleetRequest,
    timeout: Duration,
) -> Result<FleetResponse> {
    let subject = match req {
        FleetRequest::ListSessions => format!("cw.{}.list", daemon_name),
        FleetRequest::Launch { .. } => format!("cw.{}.launch", daemon_name),
        FleetRequest::Kill { .. } => format!("cw.{}.kill", daemon_name),
        FleetRequest::GetStatus { .. } => format!("cw.{}.status", daemon_name),
        FleetRequest::SendInput { .. } => format!("cw.{}.send", daemon_name),
        FleetRequest::Discover => "cw.fleet.discover".to_string(),
    };

    let payload = serde_json::to_vec(req)?;
    let msg = tokio::time::timeout(timeout, client.request(subject, payload.into()))
        .await
        .context("fleet request timed out")?
        .context("NATS request failed")?;

    serde_json::from_slice(&msg.payload).context("parsing fleet response")
}

/// Display fleet discovery results with session details.
pub fn print_fleet_detail(daemons: &[DaemonInfo]) {
    if daemons.is_empty() {
        println!("No nodes discovered.");
        return;
    }

    for d in daemons {
        let uptime = format_uptime(d.uptime_secs);
        let url = d.external_url.as_deref().unwrap_or("-");
        println!("{} (up {}, url: {})", d.name, uptime, url);
        if d.sessions.is_empty() {
            println!("  (no sessions)");
        } else {
            for s in &d.sessions {
                println!(
                    "  [{:>3}] {} ({}) {}",
                    s.id, s.prompt, s.status, s.working_dir
                );
            }
        }
        println!();
    }
}

fn format_uptime(secs: u64) -> String {
    if secs < 60 {
        format!("{}s", secs)
    } else if secs < 3600 {
        format!("{}m", secs / 60)
    } else if secs < 86400 {
        format!("{}h", secs / 3600)
    } else {
        format!("{}d", secs / 86400)
    }
}

/// Parse a fleet target string: "daemon:session_id".
pub fn parse_fleet_target(target: &str) -> Result<(&str, u32)> {
    let (daemon, id_str) = target
        .split_once(':')
        .context("fleet target must be <node>:<session_id>")?;
    let id: u32 = id_str.parse().context("session ID must be a number")?;
    Ok((daemon, id))
}

/// Handle the `cw fleet list` command.
pub async fn handle_fleet_list(
    nats_config: &NatsConfig,
    timeout_secs: u64,
    json: bool,
) -> Result<()> {
    let client = connect_nats(nats_config).await?;
    let daemons = discover_fleet(&client, Duration::from_secs(timeout_secs)).await?;

    if json {
        println!("{}", serde_json::to_string_pretty(&daemons)?);
    } else {
        print_fleet_detail(&daemons);
    }

    Ok(())
}

/// Handle the `cw fleet launch --on <daemon> -- <command>` command.
pub async fn handle_fleet_launch(
    nats_config: &NatsConfig,
    daemon_name: &str,
    command: Vec<String>,
    working_dir: String,
) -> Result<()> {
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::Launch {
        command,
        working_dir,
    };
    let resp = fleet_request(&client, daemon_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::Launched { daemon, id } => {
            println!("Launched session {} on {}", id, daemon);
        }
        FleetResponse::Error { daemon, message } => {
            bail!("Error from {}: {}", daemon, message);
        }
        other => bail!("Unexpected response: {:?}", other),
    }

    Ok(())
}

/// Handle the `cw fleet kill <daemon>:<id>` command.
pub async fn handle_fleet_kill(nats_config: &NatsConfig, target: &str) -> Result<()> {
    let (daemon_name, id) = parse_fleet_target(target)?;
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::Kill { id };
    let resp = fleet_request(&client, daemon_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::Killed { daemon, id } => {
            println!("Killed session {} on {}", id, daemon);
        }
        FleetResponse::Error { daemon, message } => {
            bail!("Error from {}: {}", daemon, message);
        }
        other => bail!("Unexpected response: {:?}", other),
    }

    Ok(())
}

/// Handle the `cw fleet send <node>:<id> <text>` command.
pub async fn handle_fleet_send_input(
    nats_config: &NatsConfig,
    target: &str,
    data: Vec<u8>,
) -> Result<()> {
    let (daemon_name, id) = parse_fleet_target(target)?;
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::SendInput { id, data };
    let resp = fleet_request(&client, daemon_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::InputSent { bytes, .. } => {
            println!("Sent {} bytes to {}:{}", bytes, daemon_name, id);
            Ok(())
        }
        FleetResponse::Error { message, .. } => bail!("{}", message),
        other => bail!("Unexpected response: {:?}", other),
    }
}

/// Handle the `cw fleet attach <daemon>:<id>` command.
///
/// Discovers the daemon's external_url via NATS, then connects via WSS
/// for the actual terminal attach (PTY data never goes over NATS).
pub async fn handle_fleet_attach(
    nats_config: &NatsConfig,
    data_dir: &std::path::Path,
    target: &str,
) -> Result<()> {
    let (daemon_name, session_id) = parse_fleet_target(target)?;

    // Discover daemon to get its external_url
    let client = connect_nats(nats_config).await?;
    let daemons = discover_fleet(&client, Duration::from_secs(2)).await?;

    let daemon = daemons
        .iter()
        .find(|d| d.name == daemon_name)
        .with_context(|| format!("node '{}' not found in fleet", daemon_name))?;

    let external_url = daemon
        .external_url
        .as_ref()
        .with_context(|| format!("node '{}' has no external_url configured", daemon_name))?;

    // Look up auth token from servers.toml
    let servers = crate::config::ServersConfig::load(data_dir)?;
    let token = servers
        .servers
        .get(daemon_name)
        .map(|e| e.token.clone())
        .with_context(|| {
            format!(
                "no auth token for '{}'. Add one with: cw server add {} {} --token <token>",
                daemon_name, daemon_name, external_url
            )
        })?;

    // Connect via WSS for attach (data plane)
    let ws_target = crate::client::Target::Remote {
        url: external_url.clone(),
        token,
    };

    crate::client::attach(&ws_target, Some(session_id), false).await
}
