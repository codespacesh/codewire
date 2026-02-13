//! Fleet integration tests.
//!
//! Requires: `cargo test --features nats`
//! Also requires: `docker compose up -d` (NATS on localhost:4222, Caddy on localhost:9443)
//! Tests are skipped if NATS is not available.

#![cfg(feature = "nats")]

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use codewire::config::{validate_daemon_name, DaemonConfig, NatsConfig};
use codewire::connection::{FrameReader, FrameWriter};
use codewire::daemon::Daemon;
use codewire::fleet::connect_nats;
use codewire::fleet_client::{discover_fleet, fleet_request, parse_fleet_target};
use codewire::protocol::{self, FleetRequest, FleetResponse, Frame, Request, Response};
use codewire::session::SessionManager;
use futures::StreamExt;
use tokio::net::UnixStream;

fn nats_config() -> NatsConfig {
    NatsConfig {
        url: std::env::var("TEST_NATS_URL")
            .unwrap_or_else(|_| "nats://127.0.0.1:4222".to_string()),
        token: None,
        creds_file: None,
    }
}

/// Try to connect to NATS; skip test if unavailable.
async fn require_nats() -> async_nats::Client {
    let config = nats_config();
    match connect_nats(&config).await {
        Ok(client) => client,
        Err(_) => {
            eprintln!("NATS not available, skipping test");
            std::process::exit(0);
        }
    }
}

fn temp_dir(name: &str) -> PathBuf {
    let dir = std::env::temp_dir()
        .join("codewire-fleet-test")
        .join(name)
        .join(format!("{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn test_daemon_config(name: &str) -> DaemonConfig {
    DaemonConfig {
        name: name.to_string(),
        listen: None,
        external_url: Some(format!("wss://{}.test.example.com/ws", name)),
    }
}

/// Find an available TCP port.
fn find_available_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.local_addr().unwrap().port()
}

/// Start a full daemon (Unix socket + WS + NATS fleet) and return (sock_path, ws_port).
async fn start_fleet_daemon(data_dir: &Path, daemon_name: &str) -> (PathBuf, u16) {
    let port = find_available_port();
    let nats = nats_config();

    // Write config with WS + NATS + fleet
    let config = format!(
        r#"[daemon]
name = "{daemon_name}"
listen = "127.0.0.1:{port}"
external_url = "ws://127.0.0.1:{port}/ws"

[nats]
url = "{}"
"#,
        nats.url,
    );
    std::fs::write(data_dir.join("config.toml"), config).unwrap();

    let daemon = Daemon::new(data_dir).unwrap();
    let sock_path = data_dir.join("server.sock");
    tokio::spawn(async move {
        daemon.run().await.unwrap();
    });

    // Wait for Unix socket
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if sock_path.exists() && UnixStream::connect(&sock_path).await.is_ok() {
            break;
        }
    }

    // Wait for WS to be ready
    for _ in 0..50 {
        if tokio::net::TcpStream::connect(format!("127.0.0.1:{}", port))
            .await
            .is_ok()
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }

    // Wait for NATS subscription to be established
    tokio::time::sleep(Duration::from_millis(500)).await;

    (sock_path, port)
}

/// Send a request via Unix socket and read one response.
async fn unix_request(sock_path: &PathBuf, req: &Request) -> Response {
    let stream = UnixStream::connect(sock_path).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();
    codewire::protocol::send_request(&mut writer, req)
        .await
        .unwrap();
    let frame = codewire::protocol::read_frame(&mut reader)
        .await
        .unwrap()
        .unwrap();
    match frame {
        Frame::Control(payload) => protocol::parse_response(&payload).unwrap(),
        _ => panic!("expected control frame"),
    }
}

/// Connect via WS and send a request, read one response.
async fn ws_request(port: u16, token: &str, req: &Request) -> Response {
    let url = format!("ws://127.0.0.1:{}/ws?token={}", port, token);
    let (ws, _) = tokio_tungstenite::connect_async(&url).await.unwrap();
    let (ws_writer, ws_reader) = ws.split();
    let mut reader = FrameReader::WsClient(ws_reader);
    let mut writer = FrameWriter::WsClient(ws_writer);

    writer.send_request(req).await.unwrap();
    let frame = reader.read_frame().await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => protocol::parse_response(&payload).unwrap(),
        _ => panic!("expected control frame"),
    }
}

// ===========================================================================
// Unit tests (no NATS required)
// ===========================================================================

#[test]
fn test_validate_daemon_name_valid() {
    assert!(validate_daemon_name("my-daemon").is_ok());
    assert!(validate_daemon_name("daemon_1").is_ok());
    assert!(validate_daemon_name("gpu-box").is_ok());
    assert!(validate_daemon_name("a").is_ok());
}

#[test]
fn test_validate_daemon_name_invalid() {
    assert!(validate_daemon_name("").is_err());
    assert!(validate_daemon_name("my.daemon").is_err());
    assert!(validate_daemon_name("my daemon").is_err());
    assert!(validate_daemon_name("my*daemon").is_err());
    assert!(validate_daemon_name("my>daemon").is_err());
}

#[test]
fn test_parse_fleet_target_valid() {
    let (daemon, id) = parse_fleet_target("gpu-box:42").unwrap();
    assert_eq!(daemon, "gpu-box");
    assert_eq!(id, 42);
}

#[test]
fn test_parse_fleet_target_invalid() {
    assert!(parse_fleet_target("no-colon").is_err());
    assert!(parse_fleet_target("daemon:abc").is_err());
}

// ===========================================================================
// NATS fleet tests (standalone fleet module, no full daemon)
// ===========================================================================

#[tokio::test]
async fn test_fleet_discover_two_daemons() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir_a = temp_dir("fleet-disc-a");
    let dir_b = temp_dir("fleet-disc-b");

    let (manager_a, _rx_a) = SessionManager::new(dir_a).unwrap();
    let manager_a = Arc::new(manager_a);
    let (manager_b, _rx_b) = SessionManager::new(dir_b).unwrap();
    let manager_b = Arc::new(manager_b);

    let config_a = test_daemon_config("e2e-disc-a");
    let config_b = test_daemon_config("e2e-disc-b");

    let nats_a = nats_config.clone();
    let cfg_a = config_a.clone();
    let mgr_a = manager_a.clone();
    let fleet_a = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_a, &cfg_a, mgr_a).await
    });

    let nats_b = nats_config.clone();
    let cfg_b = config_b.clone();
    let mgr_b = manager_b.clone();
    let fleet_b = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_b, &cfg_b, mgr_b).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    let daemons = discover_fleet(&client, Duration::from_secs(2))
        .await
        .unwrap();

    let names: Vec<&str> = daemons.iter().map(|d| d.name.as_str()).collect();
    assert!(names.contains(&"e2e-disc-a"), "missing daemon A: {:?}", names);
    assert!(names.contains(&"e2e-disc-b"), "missing daemon B: {:?}", names);

    let a = daemons.iter().find(|d| d.name == "e2e-disc-a").unwrap();
    assert_eq!(
        a.external_url.as_deref(),
        Some("wss://e2e-disc-a.test.example.com/ws")
    );

    fleet_a.abort();
    fleet_b.abort();
}

#[tokio::test]
async fn test_fleet_launch_and_list() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir = temp_dir("fleet-launch");
    let (manager, _rx) = SessionManager::new(dir).unwrap();
    let manager = Arc::new(manager);
    let config = test_daemon_config("e2e-launcher");

    let nats_cfg = nats_config.clone();
    let cfg = config.clone();
    let mgr = manager.clone();
    let handle = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_cfg, &cfg, mgr).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Launch via NATS
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "echo hello && sleep 10".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "e2e-launcher", &req, Duration::from_secs(5))
        .await
        .unwrap();

    let session_id = match resp {
        FleetResponse::Launched { daemon, id } => {
            assert_eq!(daemon, "e2e-launcher");
            id
        }
        other => panic!("expected Launched, got: {:?}", other),
    };

    // List via NATS
    let resp = fleet_request(
        &client,
        "e2e-launcher",
        &FleetRequest::ListSessions,
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    match resp {
        FleetResponse::SessionList { daemon, sessions } => {
            assert_eq!(daemon, "e2e-launcher");
            assert!(sessions.iter().any(|s| s.id == session_id));
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    handle.abort();
}

#[tokio::test]
async fn test_fleet_kill_session() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir = temp_dir("fleet-kill");
    let (manager, _rx) = SessionManager::new(dir).unwrap();
    let manager = Arc::new(manager);
    let config = test_daemon_config("e2e-killer");

    let nats_cfg = nats_config.clone();
    let cfg = config.clone();
    let mgr = manager.clone();
    let handle = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_cfg, &cfg, mgr).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Launch
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "sleep 60".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "e2e-killer", &req, Duration::from_secs(5))
        .await
        .unwrap();
    let id = match resp {
        FleetResponse::Launched { id, .. } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    // Kill via NATS
    let resp = fleet_request(
        &client,
        "e2e-killer",
        &FleetRequest::Kill { id },
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    match resp {
        FleetResponse::Killed { daemon, id: killed } => {
            assert_eq!(daemon, "e2e-killer");
            assert_eq!(killed, id);
        }
        other => panic!("expected Killed, got: {:?}", other),
    }

    handle.abort();
}

#[tokio::test]
async fn test_fleet_get_status() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir = temp_dir("fleet-status");
    let (manager, _rx) = SessionManager::new(dir).unwrap();
    let manager = Arc::new(manager);
    let config = test_daemon_config("e2e-status");

    let nats_cfg = nats_config.clone();
    let cfg = config.clone();
    let mgr = manager.clone();
    let handle = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_cfg, &cfg, mgr).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Launch
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "echo status-test && sleep 30".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "e2e-status", &req, Duration::from_secs(5))
        .await
        .unwrap();
    let id = match resp {
        FleetResponse::Launched { id, .. } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // GetStatus via NATS
    let resp = fleet_request(
        &client,
        "e2e-status",
        &FleetRequest::GetStatus { id },
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    match resp {
        FleetResponse::SessionStatus { daemon, info, .. } => {
            assert_eq!(daemon, "e2e-status");
            assert_eq!(info.id, id);
            assert_eq!(info.status, "running");
        }
        other => panic!("expected SessionStatus, got: {:?}", other),
    }

    handle.abort();
}

// ===========================================================================
// E2E tests: Full daemon with WS + NATS (docker compose required)
// ===========================================================================

/// Full e2e: start a daemon with WS+NATS, discover via NATS, launch via NATS,
/// list via WS, verify sessions match across both planes.
#[tokio::test]
async fn test_e2e_nats_discover_ws_list() {
    let _client = require_nats().await;

    let dir = temp_dir("e2e-nats-ws");
    let (sock, port) = start_fleet_daemon(&dir, "e2e-full").await;
    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Discover via NATS
    let client = connect_nats(&nats_config()).await.unwrap();
    let daemons = discover_fleet(&client, Duration::from_secs(2))
        .await
        .unwrap();
    let our_daemon = daemons.iter().find(|d| d.name == "e2e-full");
    assert!(our_daemon.is_some(), "daemon not found via NATS discover");
    assert_eq!(
        our_daemon.unwrap().external_url.as_deref(),
        Some(format!("ws://127.0.0.1:{}/ws", port).as_str())
    );

    // Launch a session via NATS
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "echo nats-launched && sleep 30".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "e2e-full", &req, Duration::from_secs(5))
        .await
        .unwrap();
    let nats_id = match resp {
        FleetResponse::Launched { id, .. } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    // Verify session visible via WS
    let ws_resp = ws_request(port, &token, &Request::ListSessions).await;
    match ws_resp {
        Response::SessionList { sessions } => {
            assert!(
                sessions.iter().any(|s| s.id == nats_id),
                "NATS-launched session {} not visible via WS",
                nats_id
            );
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    // Also visible via Unix socket
    let unix_resp = unix_request(&sock, &Request::ListSessions).await;
    match unix_resp {
        Response::SessionList { sessions } => {
            assert!(sessions.iter().any(|s| s.id == nats_id));
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    // Kill via NATS
    let resp = fleet_request(
        &client,
        "e2e-full",
        &FleetRequest::Kill { id: nats_id },
        Duration::from_secs(5),
    )
    .await
    .unwrap();
    assert!(matches!(resp, FleetResponse::Killed { .. }));
}

/// Full e2e: launch via WS, discover via NATS, session appears in fleet discover.
#[tokio::test]
async fn test_e2e_ws_launch_nats_discover() {
    let _client = require_nats().await;

    let dir = temp_dir("e2e-ws-nats");
    let (_sock, port) = start_fleet_daemon(&dir, "e2e-cross").await;
    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Launch via WS
    let ws_resp = ws_request(
        port,
        &token,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "echo ws-launched && sleep 30".into()],
            working_dir: "/tmp".into(),
        },
    )
    .await;
    let ws_id = match ws_resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    tokio::time::sleep(Duration::from_millis(200)).await;

    // Session should appear in NATS discover
    let client = connect_nats(&nats_config()).await.unwrap();
    let daemons = discover_fleet(&client, Duration::from_secs(2))
        .await
        .unwrap();
    let daemon = daemons.iter().find(|d| d.name == "e2e-cross").unwrap();
    assert!(
        daemon.sessions.iter().any(|s| s.id == ws_id),
        "WS-launched session {} not visible in NATS discover",
        ws_id
    );

    // Also verify via NATS ListSessions
    let resp = fleet_request(
        &client,
        "e2e-cross",
        &FleetRequest::ListSessions,
        Duration::from_secs(5),
    )
    .await
    .unwrap();
    match resp {
        FleetResponse::SessionList { sessions, .. } => {
            assert!(sessions.iter().any(|s| s.id == ws_id));
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    // Clean up
    ws_request(port, &token, &Request::Kill { id: ws_id }).await;
}

/// Full e2e: two daemons, verify independent fleet discovery.
#[tokio::test]
async fn test_e2e_multi_daemon_fleet() {
    let _client = require_nats().await;

    let dir_a = temp_dir("e2e-multi-a");
    let dir_b = temp_dir("e2e-multi-b");
    let (_sock_a, port_a) = start_fleet_daemon(&dir_a, "e2e-alpha").await;
    let (_sock_b, port_b) = start_fleet_daemon(&dir_b, "e2e-beta").await;
    let token_a = std::fs::read_to_string(dir_a.join("token")).unwrap();
    let token_b = std::fs::read_to_string(dir_b.join("token")).unwrap();

    // Launch on each daemon via WS
    let resp_a = ws_request(
        port_a,
        &token_a,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".into(),
        },
    )
    .await;
    let id_a = match resp_a {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    let resp_b = ws_request(
        port_b,
        &token_b,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".into(),
        },
    )
    .await;
    let id_b = match resp_b {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Fleet discover should find both
    let client = connect_nats(&nats_config()).await.unwrap();
    let daemons = discover_fleet(&client, Duration::from_secs(2))
        .await
        .unwrap();

    let alpha = daemons.iter().find(|d| d.name == "e2e-alpha");
    let beta = daemons.iter().find(|d| d.name == "e2e-beta");
    assert!(alpha.is_some(), "alpha not found in fleet");
    assert!(beta.is_some(), "beta not found in fleet");
    assert!(alpha.unwrap().sessions.iter().any(|s| s.id == id_a));
    assert!(beta.unwrap().sessions.iter().any(|s| s.id == id_b));

    // Kill alpha's session via NATS, verify beta unaffected
    fleet_request(
        &client,
        "e2e-alpha",
        &FleetRequest::Kill { id: id_a },
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    // Beta's session still alive via WS
    let resp = ws_request(port_b, &token_b, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            assert!(sessions.iter().any(|s| s.id == id_b && s.status == "running"));
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    // Clean up
    ws_request(port_b, &token_b, &Request::Kill { id: id_b }).await;
}

/// Full e2e: attach via WS to a NATS-launched session, verify output arrives.
#[tokio::test]
async fn test_e2e_nats_launch_ws_attach() {
    let _client = require_nats().await;

    let dir = temp_dir("e2e-attach");
    let (_sock, port) = start_fleet_daemon(&dir, "e2e-attach").await;
    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Launch a session that waits, then produces output (so attach happens first)
    let client = connect_nats(&nats_config()).await.unwrap();
    let req = FleetRequest::Launch {
        command: vec![
            "bash".into(),
            "-c".into(),
            "sleep 1; for i in 1 2 3; do echo line-$i; sleep 0.1; done; sleep 30".into(),
        ],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "e2e-attach", &req, Duration::from_secs(5))
        .await
        .unwrap();
    let id = match resp {
        FleetResponse::Launched { id, .. } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    tokio::time::sleep(Duration::from_millis(200)).await;

    // Attach via WS (before the output starts)
    let url = format!("ws://127.0.0.1:{}/ws?token={}", port, token);
    let (ws, _) = tokio_tungstenite::connect_async(&url).await.unwrap();
    let (ws_writer, ws_reader) = ws.split();
    let mut reader = FrameReader::WsClient(ws_reader);
    let mut writer = FrameWriter::WsClient(ws_writer);

    writer.send_request(&Request::Attach { id }).await.unwrap();

    // Read Attached response
    let frame = reader.read_frame().await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp: Response = protocol::parse_response(&payload).unwrap();
            assert!(matches!(resp, Response::Attached { .. }));
        }
        _ => panic!("expected control frame"),
    }

    // Read data frames (PTY output — arrives after 1s delay)
    let mut output = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    loop {
        tokio::select! {
            frame = reader.read_frame() => {
                match frame.unwrap() {
                    Some(Frame::Data(data)) => {
                        output.extend_from_slice(&data);
                        let text = String::from_utf8_lossy(&output);
                        if text.contains("line-3") {
                            break;
                        }
                    }
                    Some(Frame::Control(_)) => {}
                    None => break,
                }
            }
            _ = tokio::time::sleep_until(deadline) => {
                panic!("timed out waiting for output, got: {}", String::from_utf8_lossy(&output));
            }
        }
    }

    let text = String::from_utf8_lossy(&output);
    assert!(text.contains("line-1"), "missing line-1 in: {}", text);
    assert!(text.contains("line-2"), "missing line-2 in: {}", text);
    assert!(text.contains("line-3"), "missing line-3 in: {}", text);

    // Detach
    writer.send_request(&Request::Detach).await.unwrap();

    // Kill via NATS
    fleet_request(
        &client,
        "e2e-attach",
        &FleetRequest::Kill { id },
        Duration::from_secs(5),
    )
    .await
    .unwrap();
}

/// Full e2e: error response for nonexistent session via NATS.
#[tokio::test]
async fn test_e2e_fleet_error_response() {
    let _client = require_nats().await;

    let dir = temp_dir("e2e-err");
    let (_sock, _port) = start_fleet_daemon(&dir, "e2e-errors").await;

    let client = connect_nats(&nats_config()).await.unwrap();

    // Kill nonexistent session
    let resp = fleet_request(
        &client,
        "e2e-errors",
        &FleetRequest::Kill { id: 99999 },
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    match resp {
        FleetResponse::Error { daemon, message } => {
            assert_eq!(daemon, "e2e-errors");
            assert!(message.contains("99999") || message.to_lowercase().contains("not found"));
        }
        other => panic!("expected Error, got: {:?}", other),
    }

    // GetStatus for nonexistent session
    let resp = fleet_request(
        &client,
        "e2e-errors",
        &FleetRequest::GetStatus { id: 99999 },
        Duration::from_secs(5),
    )
    .await
    .unwrap();

    assert!(matches!(resp, FleetResponse::Error { .. }));
}
