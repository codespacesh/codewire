//! Fleet integration tests.
//!
//! Requires: `cargo test --features nats`
//! Also requires nats-server running on localhost:4222.
//! Tests are skipped if NATS is not available.

#![cfg(feature = "nats")]

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use codewire::config::{validate_daemon_name, DaemonConfig, NatsConfig};
use codewire::fleet::connect_nats;
use codewire::fleet_client::{discover_fleet, fleet_request, parse_fleet_target};
use codewire::protocol::FleetRequest;
use codewire::session::SessionManager;

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

// ---------------------------------------------------------------------------
// Unit tests (no NATS required)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Integration tests (require NATS server)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn test_fleet_discover_two_daemons() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir_a = temp_dir("fleet-discover-a");
    let dir_b = temp_dir("fleet-discover-b");

    let (manager_a, _persist_rx_a) = SessionManager::new(dir_a).unwrap();
    let manager_a = Arc::new(manager_a);
    let (manager_b, _persist_rx_b) = SessionManager::new(dir_b).unwrap();
    let manager_b = Arc::new(manager_b);

    let config_a = test_daemon_config("test-daemon-a");
    let config_b = test_daemon_config("test-daemon-b");

    // Start both fleet handlers
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

    // Give them time to subscribe
    tokio::time::sleep(Duration::from_millis(500)).await;

    // Discover
    let daemons = discover_fleet(&client, Duration::from_secs(2)).await.unwrap();

    assert!(
        daemons.len() >= 2,
        "expected at least 2 daemons, got {}",
        daemons.len()
    );

    let names: Vec<&str> = daemons.iter().map(|d| d.name.as_str()).collect();
    assert!(names.contains(&"test-daemon-a"), "missing daemon A");
    assert!(names.contains(&"test-daemon-b"), "missing daemon B");

    // Verify external_url
    let a = daemons.iter().find(|d| d.name == "test-daemon-a").unwrap();
    assert_eq!(
        a.external_url.as_deref(),
        Some("wss://test-daemon-a.test.example.com/ws")
    );

    fleet_a.abort();
    fleet_b.abort();
}

#[tokio::test]
async fn test_fleet_launch_and_list() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir = temp_dir("fleet-launch");
    let (manager, _persist_rx) = SessionManager::new(dir).unwrap();
    let manager = Arc::new(manager);
    let config = test_daemon_config("test-launcher");

    let nats_cfg = nats_config.clone();
    let cfg = config.clone();
    let mgr = manager.clone();
    let fleet_handle = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_cfg, &cfg, mgr).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Launch via fleet
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "echo hello && sleep 10".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "test-launcher", &req, Duration::from_secs(5))
        .await
        .unwrap();

    let session_id = match resp {
        codewire::protocol::FleetResponse::Launched { daemon, id } => {
            assert_eq!(daemon, "test-launcher");
            id
        }
        other => panic!("expected Launched, got: {:?}", other),
    };

    // List via fleet
    let req = FleetRequest::ListSessions;
    let resp = fleet_request(&client, "test-launcher", &req, Duration::from_secs(5))
        .await
        .unwrap();

    match resp {
        codewire::protocol::FleetResponse::SessionList { daemon, sessions } => {
            assert_eq!(daemon, "test-launcher");
            assert!(!sessions.is_empty());
            assert!(sessions.iter().any(|s| s.id == session_id));
        }
        other => panic!("expected SessionList, got: {:?}", other),
    }

    fleet_handle.abort();
}

#[tokio::test]
async fn test_fleet_kill_session() {
    let client = require_nats().await;
    let nats_config = nats_config();

    let dir = temp_dir("fleet-kill");
    let (manager, _persist_rx) = SessionManager::new(dir).unwrap();
    let manager = Arc::new(manager);
    let config = test_daemon_config("test-killer");

    let nats_cfg = nats_config.clone();
    let cfg = config.clone();
    let mgr = manager.clone();
    let fleet_handle = tokio::spawn(async move {
        codewire::fleet::run_fleet(&nats_cfg, &cfg, mgr).await
    });

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Launch a session
    let req = FleetRequest::Launch {
        command: vec!["bash".into(), "-c".into(), "sleep 60".into()],
        working_dir: "/tmp".to_string(),
    };
    let resp = fleet_request(&client, "test-killer", &req, Duration::from_secs(5))
        .await
        .unwrap();
    let session_id = match resp {
        codewire::protocol::FleetResponse::Launched { id, .. } => id,
        other => panic!("expected Launched, got: {:?}", other),
    };

    // Kill it
    let req = FleetRequest::Kill { id: session_id };
    let resp = fleet_request(&client, "test-killer", &req, Duration::from_secs(5))
        .await
        .unwrap();

    match resp {
        codewire::protocol::FleetResponse::Killed { daemon, id } => {
            assert_eq!(daemon, "test-killer");
            assert_eq!(id, session_id);
        }
        other => panic!("expected Killed, got: {:?}", other),
    }

    fleet_handle.abort();
}
