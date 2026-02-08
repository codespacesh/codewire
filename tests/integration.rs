//! End-to-end integration tests for codewire.
//!
//! These tests start a daemon, launch sessions using `bash -c` instead of `claude`,
//! and verify the full lifecycle: launch, list, attach, detach, kill, and logs.

use std::path::PathBuf;
use std::time::Duration;

use tokio::net::UnixStream;

use codewire::daemon::Daemon;
use codewire::protocol::{self, Frame, Request, Response, read_frame, send_data, send_request};

/// Create a temp dir for a test and return its path.
fn temp_dir(name: &str) -> PathBuf {
    let dir = std::env::temp_dir()
        .join("codewire-test")
        .join(name)
        .join(format!("{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Helper: send a request, read one response.
async fn request_response(sock_path: &PathBuf, req: &Request) -> Response {
    let stream = UnixStream::connect(sock_path).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();
    send_request(&mut writer, req).await.unwrap();
    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => protocol::parse_response(&payload).unwrap(),
        _ => panic!("expected control frame"),
    }
}

/// Start a daemon in a background task, configured to use bash instead of claude.
async fn start_test_daemon(data_dir: &PathBuf) -> PathBuf {
    let daemon = Daemon::new(data_dir).unwrap();
    daemon
        .set_command("bash".to_string(), vec!["-c".to_string()])
        .await;

    let sock_path = data_dir.join("server.sock");
    tokio::spawn(async move {
        daemon.run().await.unwrap();
    });

    // Wait for socket to appear
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if sock_path.exists() {
            if UnixStream::connect(&sock_path).await.is_ok() {
                return sock_path;
            }
        }
    }
    panic!("daemon failed to start");
}

#[tokio::test]
async fn test_launch_and_list() {
    let dir = temp_dir("launch-list");
    let sock = start_test_daemon(&dir).await;

    // Launch a session
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "echo hello-from-codewire && sleep 5".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;

    let session_id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Give the process a moment to start
    tokio::time::sleep(Duration::from_millis(500)).await;

    // List sessions
    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            assert!(!sessions.is_empty(), "should have at least one session");
            let found = sessions.iter().find(|s| s.id == session_id);
            assert!(found.is_some(), "launched session should appear in list");
            let s = found.unwrap();
            assert_eq!(s.status, "running");
            assert!(s.prompt.contains("hello-from-codewire"));
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_kill_session() {
    let dir = temp_dir("kill");
    let sock = start_test_daemon(&dir).await;

    // Launch
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "sleep 60".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Kill
    let resp = request_response(&sock, &Request::Kill { id }).await;
    match resp {
        Response::Killed { id: killed_id } => assert_eq!(killed_id, id),
        other => panic!("expected Killed, got: {other:?}"),
    }

    // Wait for status to update
    tokio::time::sleep(Duration::from_secs(1)).await;

    // Verify it's no longer running
    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            let s = sessions.iter().find(|s| s.id == id).unwrap();
            assert!(
                s.status.contains("killed") || s.status.contains("completed"),
                "status should be killed or completed (from SIGTERM), got: {}",
                s.status
            );
            assert_ne!(s.status, "running", "should not still be running");
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_kill_all() {
    let dir = temp_dir("kill-all");
    let sock = start_test_daemon(&dir).await;

    // Launch two sessions
    request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "sleep 60".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "sleep 60".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;

    tokio::time::sleep(Duration::from_millis(300)).await;

    let resp = request_response(&sock, &Request::KillAll).await;
    match resp {
        Response::KilledAll { count } => assert_eq!(count, 2),
        other => panic!("expected KilledAll, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_session_completes_naturally() {
    let dir = temp_dir("complete");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that exits quickly
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "echo done".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Wait for it to complete
    tokio::time::sleep(Duration::from_secs(2)).await;

    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            let s = sessions.iter().find(|s| s.id == id).unwrap();
            assert!(
                s.status.contains("completed"),
                "status should be completed, got: {}",
                s.status
            );
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_logs() {
    let dir = temp_dir("logs");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that outputs something
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "echo LOG_TEST_OUTPUT_12345".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Wait for output to be captured
    tokio::time::sleep(Duration::from_secs(2)).await;

    // Read logs (non-follow mode)
    let resp = request_response(
        &sock,
        &Request::Logs {
            id,
            follow: false,
            tail: None,
        },
    )
    .await;

    match resp {
        Response::LogData { data, done } => {
            assert!(done, "non-follow should be done=true");
            assert!(
                data.contains("LOG_TEST_OUTPUT_12345"),
                "log should contain our output, got: {data}"
            );
        }
        other => panic!("expected LogData, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_attach_and_receive_output() {
    let dir = temp_dir("attach");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that outputs periodically
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "for i in 1 2 3; do echo ATTACH_TEST_$i; sleep 1; done".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Attach
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, &Request::Attach { id }).await.unwrap();

    // Read attach confirmation
    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    let resp = match frame {
        Frame::Control(payload) => protocol::parse_response(&payload).unwrap(),
        _ => panic!("expected control frame"),
    };
    match resp {
        Response::Attached { id: attached_id } => assert_eq!(attached_id, id),
        other => panic!("expected Attached, got: {other:?}"),
    }

    // Read some data frames
    let mut collected_output = Vec::new();
    let timeout = tokio::time::sleep(Duration::from_secs(5));
    tokio::pin!(timeout);

    loop {
        tokio::select! {
            frame = read_frame(&mut reader) => {
                match frame.unwrap() {
                    Some(Frame::Data(bytes)) => {
                        collected_output.extend_from_slice(&bytes);
                        let text = String::from_utf8_lossy(&collected_output);
                        if text.contains("ATTACH_TEST_3") {
                            break;
                        }
                    }
                    Some(Frame::Control(payload)) => {
                        // Session might end
                        let resp = protocol::parse_response(&payload).unwrap();
                        match resp {
                            Response::Error { message } if message.contains("completed") => break,
                            _ => {}
                        }
                    }
                    None => break,
                }
            }
            _ = &mut timeout => {
                let text = String::from_utf8_lossy(&collected_output);
                // It's ok if we got at least some output
                assert!(
                    text.contains("ATTACH_TEST_"),
                    "should have received some output, got: {text}"
                );
                break;
            }
        }
    }

    let output = String::from_utf8_lossy(&collected_output);
    assert!(
        output.contains("ATTACH_TEST_"),
        "attached client should receive PTY output, got: {output}"
    );
}

#[tokio::test]
async fn test_attach_send_input() {
    let dir = temp_dir("input");
    let sock = start_test_daemon(&dir).await;

    // Launch an interactive bash session (cat will echo stdin to stdout)
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "cat".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Attach
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, &Request::Attach { id }).await.unwrap();

    // Read attach confirmation
    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(matches!(resp, Response::Attached { .. }));
        }
        _ => panic!("expected control frame"),
    }

    // Send input
    send_data(&mut writer, b"INPUT_TEST_LINE\n").await.unwrap();

    // Read output — cat should echo it back
    let mut collected = Vec::new();
    let timeout = tokio::time::sleep(Duration::from_secs(3));
    tokio::pin!(timeout);

    loop {
        tokio::select! {
            frame = read_frame(&mut reader) => {
                match frame.unwrap() {
                    Some(Frame::Data(bytes)) => {
                        collected.extend_from_slice(&bytes);
                        let text = String::from_utf8_lossy(&collected);
                        if text.contains("INPUT_TEST_LINE") {
                            break;
                        }
                    }
                    _ => {}
                }
            }
            _ = &mut timeout => {
                break;
            }
        }
    }

    let output = String::from_utf8_lossy(&collected);
    assert!(
        output.contains("INPUT_TEST_LINE"),
        "should receive echoed input, got: {output}"
    );

    // Kill the session to clean up (cat doesn't exit on its own)
    let resp = request_response(&sock, &Request::Kill { id }).await;
    assert!(matches!(resp, Response::Killed { .. }));
}

#[tokio::test]
async fn test_detach_from_attach() {
    let dir = temp_dir("detach");
    let sock = start_test_daemon(&dir).await;

    // Launch a long-running session
    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "sleep 30".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Attach
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, &Request::Attach { id }).await.unwrap();

    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(matches!(resp, Response::Attached { .. }));
        }
        _ => panic!("expected control frame"),
    }

    // Send detach request
    send_request(&mut writer, &Request::Detach).await.unwrap();

    // Should receive Detached response
    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(
                matches!(resp, Response::Detached),
                "expected Detached, got: {resp:?}"
            );
        }
        _ => panic!("expected control frame"),
    }

    // Session should still be running
    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            let s = sessions.iter().find(|s| s.id == id).unwrap();
            assert_eq!(s.status, "running", "session should still be running after detach");
            assert!(!s.attached, "session should not be attached");
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_attach_nonexistent_session() {
    let dir = temp_dir("attach-noexist");
    let sock = start_test_daemon(&dir).await;

    let resp = request_response(&sock, &Request::Attach { id: 9999 }).await;
    match resp {
        Response::Error { message } => {
            assert!(message.contains("not found"), "error should mention not found: {message}");
        }
        other => panic!("expected Error, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_resize_during_attach() {
    let dir = temp_dir("resize");
    let sock = start_test_daemon(&dir).await;

    let resp = request_response(
        &sock,
        &Request::Launch {
            cmd: "bash".to_string(),
            prompt: "sleep 10".to_string(),
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Attach
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();

    send_request(&mut writer, &Request::Attach { id }).await.unwrap();
    let _ = read_frame(&mut reader).await.unwrap(); // Attached response

    // Send resize — should not error
    send_request(&mut writer, &Request::Resize { cols: 120, rows: 40 })
        .await
        .unwrap();

    // Small delay to process
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Detach cleanly
    send_request(&mut writer, &Request::Detach).await.unwrap();
    let frame = read_frame(&mut reader).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(matches!(resp, Response::Detached));
        }
        _ => panic!("expected Detached"),
    }

    request_response(&sock, &Request::Kill { id }).await;
}
