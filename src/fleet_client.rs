use std::time::Duration;

use anyhow::{bail, Context, Result};
use futures::StreamExt;

use crate::config::NatsConfig;
use crate::fleet::connect_nats;
use crate::protocol::{NodeInfo, FleetRequest, FleetResponse};

/// Scatter-gather fleet discovery: broadcast Discover, collect responses until timeout.
pub async fn discover_fleet(
    client: &async_nats::Client,
    timeout: Duration,
) -> Result<Vec<NodeInfo>> {
    let inbox = client.new_inbox();
    let mut sub = client.subscribe(inbox.clone()).await?;

    let req = FleetRequest::Discover;
    let payload = serde_json::to_vec(&req)?;
    client
        .publish_with_reply("cw.fleet.discover", inbox, payload.into())
        .await?;

    let mut nodes = Vec::new();
    let deadline = tokio::time::Instant::now() + timeout;

    loop {
        tokio::select! {
            msg = sub.next() => {
                if let Some(msg) = msg {
                    if let Ok(FleetResponse::NodeInfo(info)) = serde_json::from_slice(&msg.payload) {
                        nodes.push(info);
                    }
                } else {
                    break;
                }
            }
            _ = tokio::time::sleep_until(deadline) => break,
        }
    }
    Ok(nodes)
}

/// Send a request to a specific node via NATS request-reply.
pub async fn fleet_request(
    client: &async_nats::Client,
    node_name: &str,
    req: &FleetRequest,
    timeout: Duration,
) -> Result<FleetResponse> {
    let subject = match req {
        FleetRequest::ListSessions => format!("cw.{}.list", node_name),
        FleetRequest::Launch { .. } => format!("cw.{}.launch", node_name),
        FleetRequest::Kill { .. } => format!("cw.{}.kill", node_name),
        FleetRequest::GetStatus { .. } => format!("cw.{}.status", node_name),
        FleetRequest::SendInput { .. } => format!("cw.{}.send", node_name),
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
pub fn print_fleet_detail(nodes: &[NodeInfo]) {
    if nodes.is_empty() {
        println!("No nodes discovered.");
        return;
    }

    for d in nodes {
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

/// Parse a fleet target string: "node:session_id".
pub fn parse_fleet_target(target: &str) -> Result<(&str, u32)> {
    let (node, id_str) = target
        .split_once(':')
        .context("fleet target must be <node>:<session_id>")?;
    let id: u32 = id_str.parse().context("session ID must be a number")?;
    Ok((node, id))
}

/// Handle the `cw fleet list` command.
pub async fn handle_fleet_list(
    nats_config: &NatsConfig,
    timeout_secs: u64,
    json: bool,
) -> Result<()> {
    let client = connect_nats(nats_config).await?;
    let nodes = discover_fleet(&client, Duration::from_secs(timeout_secs)).await?;

    if json {
        println!("{}", serde_json::to_string_pretty(&nodes)?);
    } else {
        print_fleet_detail(&nodes);
    }

    Ok(())
}

/// Handle the `cw fleet launch --on <node> -- <command>` command.
pub async fn handle_fleet_launch(
    nats_config: &NatsConfig,
    node_name: &str,
    command: Vec<String>,
    working_dir: String,
) -> Result<()> {
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::Launch {
        command,
        working_dir,
    };
    let resp = fleet_request(&client, node_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::Launched { node, id } => {
            println!("Launched session {} on {}", id, node);
        }
        FleetResponse::Error { node, message } => {
            bail!("Error from {}: {}", node, message);
        }
        other => bail!("Unexpected response: {:?}", other),
    }

    Ok(())
}

/// Handle the `cw fleet kill <node>:<id>` command.
pub async fn handle_fleet_kill(nats_config: &NatsConfig, target: &str) -> Result<()> {
    let (node_name, id) = parse_fleet_target(target)?;
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::Kill { id };
    let resp = fleet_request(&client, node_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::Killed { node, id } => {
            println!("Killed session {} on {}", id, node);
        }
        FleetResponse::Error { node, message } => {
            bail!("Error from {}: {}", node, message);
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
    let (node_name, id) = parse_fleet_target(target)?;
    let client = connect_nats(nats_config).await?;
    let req = FleetRequest::SendInput { id, data };
    let resp = fleet_request(&client, node_name, &req, Duration::from_secs(10)).await?;

    match resp {
        FleetResponse::InputSent { bytes, .. } => {
            println!("Sent {} bytes to {}:{}", bytes, node_name, id);
            Ok(())
        }
        FleetResponse::Error { message, .. } => bail!("{}", message),
        other => bail!("Unexpected response: {:?}", other),
    }
}

/// Handle the `cw fleet attach <node>:<id>` command.
///
/// Discovers the node's external_url via NATS, then connects via WSS
/// for the actual terminal attach (PTY data never goes over NATS).
pub async fn handle_fleet_attach(
    nats_config: &NatsConfig,
    data_dir: &std::path::Path,
    target: &str,
) -> Result<()> {
    let (node_name, session_id) = parse_fleet_target(target)?;

    // Discover node to get its external_url
    let client = connect_nats(nats_config).await?;
    let nodes = discover_fleet(&client, Duration::from_secs(2)).await?;

    let node = nodes
        .iter()
        .find(|d| d.name == node_name)
        .with_context(|| format!("node '{}' not found in fleet", node_name))?;

    let external_url = node
        .external_url
        .as_ref()
        .with_context(|| format!("node '{}' has no external_url configured", node_name))?;

    // Look up auth token from servers.toml
    let servers = crate::config::ServersConfig::load(data_dir)?;
    let token = servers
        .servers
        .get(node_name)
        .map(|e| e.token.clone())
        .with_context(|| {
            format!(
                "no auth token for '{}'. Add one with: cw server add {} {} --token <token>",
                node_name, node_name, external_url
            )
        })?;

    // Connect via WSS for attach (data plane)
    let ws_target = crate::client::Target::Remote {
        url: external_url.clone(),
        token,
    };

    crate::client::attach(&ws_target, Some(session_id), false).await
}
