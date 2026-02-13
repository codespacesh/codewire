//! End-to-end integration tests for codewire.
//!
//! These tests start a daemon, launch sessions using `bash -c` instead of `claude`,
//! and verify the full lifecycle: launch, list, attach, detach, kill, and logs.

use std::path::{Path, PathBuf};
use std::time::Duration;

use tokio::net::UnixStream;

use codewire::daemon::Daemon;
use codewire::protocol::{self, read_frame, send_data, send_request, Frame, Request, Response};

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

/// Start a daemon in a background task.
async fn start_test_daemon(data_dir: &Path) -> PathBuf {
    let daemon = Daemon::new(data_dir).unwrap();

    let sock_path = data_dir.join("server.sock");
    tokio::spawn(async move {
        daemon.run().await.unwrap();
    });

    // Wait for socket to appear
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if sock_path.exists() && UnixStream::connect(&sock_path).await.is_ok() {
            return sock_path;
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
            command: vec![
                "bash".into(),
                "-c".into(),
                "echo hello-from-codewire && sleep 5".into(),
            ],
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
            assert!(
                s.prompt.contains("hello-from-codewire"),
                "prompt should contain the command, got: {}",
                s.prompt
            );
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
            command: vec!["bash".into(), "-c".into(), "sleep 60".into()],
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
            command: vec!["bash".into(), "-c".into(), "sleep 60".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 60".into()],
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
            command: vec!["bash".into(), "-c".into(), "echo done".into()],
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
            command: vec![
                "bash".into(),
                "-c".into(),
                "echo LOG_TEST_OUTPUT_12345".into(),
            ],
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
            command: vec![
                "bash".into(),
                "-c".into(),
                "for i in 1 2 3; do echo ATTACH_TEST_$i; sleep 1; done".into(),
            ],
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

    send_request(&mut writer, &Request::Attach { id })
        .await
        .unwrap();

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
            command: vec!["bash".into(), "-c".into(), "cat".into()],
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

    send_request(&mut writer, &Request::Attach { id })
        .await
        .unwrap();

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
                if let Some(Frame::Data(bytes)) = frame.unwrap() {
                    collected.extend_from_slice(&bytes);
                    let text = String::from_utf8_lossy(&collected);
                    if text.contains("INPUT_TEST_LINE") {
                        break;
                    }
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
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
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

    send_request(&mut writer, &Request::Attach { id })
        .await
        .unwrap();

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
            assert_eq!(
                s.status, "running",
                "session should still be running after detach"
            );
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
            assert!(
                message.contains("not found"),
                "error should mention not found: {message}"
            );
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
            command: vec!["bash".into(), "-c".into(), "sleep 10".into()],
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

    send_request(&mut writer, &Request::Attach { id })
        .await
        .unwrap();
    let _ = read_frame(&mut reader).await.unwrap(); // Attached response

    // Send resize — should not error
    send_request(
        &mut writer,
        &Request::Resize {
            cols: 120,
            rows: 40,
        },
    )
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

#[tokio::test]
async fn test_multiple_attachments() {
    let dir = temp_dir("multi-attach");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that outputs periodically
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec![
                "bash".into(),
                "-c".into(),
                "for i in 1 2 3 4 5; do echo MULTI_$i; sleep 1; done".into(),
            ],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Attach first client
    let stream1 = UnixStream::connect(&sock).await.unwrap();
    let (mut reader1, mut writer1) = stream1.into_split();
    send_request(&mut writer1, &Request::Attach { id })
        .await
        .unwrap();
    let _ = read_frame(&mut reader1).await.unwrap(); // Attached response

    // Attach second client (should succeed with new multi-attach support)
    let stream2 = UnixStream::connect(&sock).await.unwrap();
    let (mut reader2, mut writer2) = stream2.into_split();
    send_request(&mut writer2, &Request::Attach { id })
        .await
        .unwrap();
    let frame = read_frame(&mut reader2).await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(
                matches!(resp, Response::Attached { .. }),
                "second attach should succeed"
            );
        }
        _ => panic!("expected control frame"),
    }

    // Both clients should receive output
    let timeout = tokio::time::sleep(Duration::from_secs(3));
    tokio::pin!(timeout);

    let mut output1 = Vec::new();
    let mut output2 = Vec::new();

    loop {
        tokio::select! {
            frame = read_frame(&mut reader1) => {
                if let Ok(Some(Frame::Data(bytes))) = frame {
                    output1.extend_from_slice(&bytes);
                }
            }
            frame = read_frame(&mut reader2) => {
                if let Ok(Some(Frame::Data(bytes))) = frame {
                    output2.extend_from_slice(&bytes);
                }
            }
            _ = &mut timeout => break,
        }
    }

    let text1 = String::from_utf8_lossy(&output1);
    let text2 = String::from_utf8_lossy(&output2);

    assert!(
        text1.contains("MULTI_"),
        "first client should receive output: {text1}"
    );
    assert!(
        text2.contains("MULTI_"),
        "second client should receive output: {text2}"
    );

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_send_input_cross_session() {
    let dir = temp_dir("cross-input");
    let sock = start_test_daemon(&dir).await;

    // Launch an interactive session (cat echoes input)
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "cat".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Send input without attaching
    let test_input = b"CROSS_SESSION_TEST\n".to_vec();
    let resp = request_response(
        &sock,
        &Request::SendInput {
            id,
            data: test_input.clone(),
        },
    )
    .await;

    match resp {
        Response::InputSent { id: sent_id, bytes } => {
            assert_eq!(sent_id, id);
            assert_eq!(bytes, test_input.len());
        }
        other => panic!("expected InputSent, got: {other:?}"),
    }

    // Wait for processing
    tokio::time::sleep(Duration::from_secs(1)).await;

    // Verify output was captured in logs
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
        Response::LogData { data, .. } => {
            assert!(
                data.contains("CROSS_SESSION_TEST"),
                "output should contain our input: {data}"
            );
        }
        other => panic!("expected LogData, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_get_session_status() {
    let dir = temp_dir("status");
    let sock = start_test_daemon(&dir).await;

    // Launch a session
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec![
                "bash".into(),
                "-c".into(),
                "echo STATUS_TEST && sleep 2".into(),
            ],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Get status
    let resp = request_response(&sock, &Request::GetStatus { id }).await;

    match resp {
        Response::SessionStatus { info, output_size } => {
            assert_eq!(info.id, id);
            assert_eq!(info.status, "running");
            assert!(info.pid.is_some(), "PID should be present");
            assert!(output_size > 0, "should have captured some output");
            assert!(
                info.output_size_bytes.is_some(),
                "output_size_bytes should be present"
            );
        }
        other => panic!("expected SessionStatus, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_watch_session() {
    let dir = temp_dir("watch");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that outputs periodically
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec![
                "bash".into(),
                "-c".into(),
                "for i in 1 2 3; do echo WATCH_$i; sleep 1; done".into(),
            ],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Watch the session
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();

    send_request(
        &mut writer,
        &Request::WatchSession {
            id,
            include_history: true,
            history_lines: Some(10),
        },
    )
    .await
    .unwrap();

    let mut collected_output = String::new();
    let mut done = false;
    let timeout = tokio::time::sleep(Duration::from_secs(5));
    tokio::pin!(timeout);

    loop {
        tokio::select! {
            frame = read_frame(&mut reader) => {
                match frame.unwrap() {
                    Some(Frame::Control(payload)) => {
                        let resp = protocol::parse_response(&payload).unwrap();
                        match resp {
                            Response::WatchUpdate { output, done: is_done, .. } => {
                                if let Some(text) = output {
                                    collected_output.push_str(&text);
                                }
                                if is_done {
                                    done = true;
                                    break;
                                }
                            }
                            other => panic!("unexpected response: {other:?}"),
                        }
                    }
                    None => break,
                    _ => {}
                }
            }
            _ = &mut timeout => break,
        }
    }

    assert!(
        collected_output.contains("WATCH_"),
        "should have received watch output: {collected_output}"
    );
    assert!(done, "watch should complete when session ends");
}

#[tokio::test]
async fn test_supervisor_pattern() {
    let dir = temp_dir("supervisor");
    let sock = start_test_daemon(&dir).await;

    // Launch two worker sessions
    let resp1 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "echo WORKER1 && sleep 2".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id1 = match resp1 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    let resp2 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "cat".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id2 = match resp2 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(500)).await;

    // Supervisor: Get status of worker 1
    let resp = request_response(&sock, &Request::GetStatus { id: id1 }).await;
    assert!(matches!(resp, Response::SessionStatus { .. }));

    // Supervisor: Send input to worker 2
    let resp = request_response(
        &sock,
        &Request::SendInput {
            id: id2,
            data: b"SUPERVISOR_MESSAGE\n".to_vec(),
        },
    )
    .await;
    assert!(matches!(resp, Response::InputSent { .. }));

    // Supervisor: List all sessions
    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            assert!(sessions.len() >= 2, "should have at least 2 sessions");
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id: id1 }).await;
    request_response(&sock, &Request::Kill { id: id2 }).await;
}

/// Test concurrent list and attach operations don't block each other
#[tokio::test]
async fn test_concurrent_list_and_attach() {
    let dir = temp_dir("concurrent_list_attach");
    let sock = start_test_daemon(&dir).await;

    // Launch 3 sessions
    let mut ids = Vec::new();
    for _ in 0..3 {
        let resp = request_response(
            &sock,
            &Request::Launch {
                command: vec!["sleep".to_string(), "10".to_string()],
                working_dir: "/tmp".to_string(),
            },
        )
        .await;
        match resp {
            Response::Launched { id } => ids.push(id),
            other => panic!("expected Launched, got: {other:?}"),
        }
    }

    // Concurrently: list sessions, attach to different sessions, get status
    // These should NOT block each other with our DashMap implementation
    let list_task = tokio::spawn({
        let sock = sock.clone();
        async move {
            for _ in 0..10 {
                let _resp = request_response(&sock, &Request::ListSessions).await;
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }
    });

    let attach_tasks: Vec<_> = ids
        .iter()
        .map(|&id| {
            let sock = sock.clone();
            tokio::spawn(async move {
                // Quick attach and detach
                let stream = UnixStream::connect(&sock).await.unwrap();
                let (mut reader, mut writer) = stream.into_split();
                send_request(&mut writer, &Request::Attach { id })
                    .await
                    .unwrap();
                let _resp = read_frame(&mut reader).await.unwrap();
                send_request(&mut writer, &Request::Detach).await.unwrap();
            })
        })
        .collect();

    // All tasks should complete quickly without blocking
    let start = std::time::Instant::now();
    list_task.await.unwrap();
    for task in attach_tasks {
        task.await.unwrap();
    }
    let elapsed = start.elapsed();

    // Should complete in well under 1 second with lock-free architecture
    assert!(
        elapsed.as_secs() < 1,
        "Operations took too long: {:?}",
        elapsed
    );

    // Clean up
    request_response(&sock, &Request::KillAll).await;
}

/// Test that persistence only happens on state changes, not periodically
#[tokio::test]
async fn test_event_driven_persistence() {
    let dir = temp_dir("evt_persist");
    let sock = start_test_daemon(&dir).await;

    let sessions_json = dir.join("sessions.json");

    // Launch a session
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["sleep".to_string(), "5".to_string()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Wait for persistence (debounced to 500ms)
    tokio::time::sleep(Duration::from_millis(800)).await;

    // Get initial mtime
    let mtime1 = std::fs::metadata(&sessions_json)
        .unwrap()
        .modified()
        .unwrap();

    // Wait 2 seconds - no state changes, so no writes expected
    tokio::time::sleep(Duration::from_secs(2)).await;

    let mtime2 = std::fs::metadata(&sessions_json)
        .unwrap()
        .modified()
        .unwrap();

    // mtime should NOT have changed (no periodic writes!)
    assert_eq!(
        mtime1, mtime2,
        "sessions.json was written without state changes"
    );

    // Now make a state change (kill the session)
    request_response(&sock, &Request::Kill { id }).await;

    // Wait for persistence
    tokio::time::sleep(Duration::from_millis(800)).await;

    let mtime3 = std::fs::metadata(&sessions_json)
        .unwrap()
        .modified()
        .unwrap();

    // mtime SHOULD have changed (state change triggered write)
    assert!(
        mtime3 > mtime2,
        "sessions.json was not written on state change"
    );
}

/// Test corrupt sessions.json recovery
#[tokio::test]
async fn test_corrupt_sessions_json_recovery() {
    let dir = temp_dir("corrupt_sessions");
    let sessions_json = dir.join("sessions.json");

    // Write corrupt JSON
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(&sessions_json, "invalid json{[[").unwrap();

    // Start daemon - should recover gracefully
    let sock = start_test_daemon(&dir).await;

    // Should start with empty session list (corrupt file ignored)
    let resp = request_response(&sock, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            assert_eq!(sessions.len(), 0, "should start with no sessions");
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Backup file should exist
    let backup_files: Vec<_> = std::fs::read_dir(&dir)
        .unwrap()
        .filter_map(|e| e.ok())
        .filter(|e| {
            e.file_name()
                .to_string_lossy()
                .starts_with("sessions.json.corrupt")
        })
        .collect();

    assert_eq!(backup_files.len(), 1, "corrupt file should be backed up");

    // Daemon should be functional - launch a new session
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["echo".to_string(), "test".to_string()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;

    assert!(matches!(resp, Response::Launched { .. }));
}

// ---------------------------------------------------------------------------
// Auto-attach tests
// ---------------------------------------------------------------------------

#[tokio::test]
async fn test_auto_attach_single_session() {
    let dir = temp_dir("auto-attach-single");
    let sock = start_test_daemon(&dir).await;

    // Launch a single session
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 10".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Auto-attach (no ID specified - simulated by using ListSessions + Attach in client)
    // This tests the protocol flow that client::attach uses
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    let auto_id = match list_resp {
        Response::SessionList { sessions } => {
            let mut candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            candidates.sort_by(|a, b| a.created_at.cmp(&b.created_at));
            candidates[0].id
        }
        other => panic!("expected SessionList, got: {other:?}"),
    };

    assert_eq!(auto_id, id, "should auto-select the single running session");

    // Attach to the auto-selected session
    let resp = request_response(&sock, &Request::Attach { id: auto_id }).await;
    match resp {
        Response::Attached { id: attached_id } => assert_eq!(attached_id, id),
        other => panic!("expected Attached, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_auto_attach_oldest_session() {
    let dir = temp_dir("auto-attach-oldest");
    let sock = start_test_daemon(&dir).await;

    // Launch three sessions with delays to ensure different timestamps
    let resp1 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id1 = match resp1 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp2 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id2 = match resp2 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp3 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id3 = match resp3 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Auto-select should pick the oldest (first) session
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    let auto_id = match list_resp {
        Response::SessionList { sessions } => {
            let mut candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            candidates.sort_by(|a, b| a.created_at.cmp(&b.created_at));
            candidates[0].id
        }
        other => panic!("expected SessionList, got: {other:?}"),
    };

    assert_eq!(auto_id, id1, "should auto-select the oldest session (id1)");
    assert_ne!(auto_id, id2, "should not select id2");
    assert_ne!(auto_id, id3, "should not select id3");

    // Clean up
    request_response(&sock, &Request::Kill { id: id1 }).await;
    request_response(&sock, &Request::Kill { id: id2 }).await;
    request_response(&sock, &Request::Kill { id: id3 }).await;
}

#[tokio::test]
async fn test_auto_attach_skips_attached() {
    let dir = temp_dir("auto-skip-att");
    let sock = start_test_daemon(&dir).await;

    // Launch two sessions
    let resp1 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id1 = match resp1 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp2 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id2 = match resp2 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Attach to first session (keep connection alive)
    let stream1 = UnixStream::connect(&sock).await.unwrap();
    let (mut reader1, mut writer1) = stream1.into_split();
    send_request(&mut writer1, &Request::Attach { id: id1 })
        .await
        .unwrap();
    let _ = read_frame(&mut reader1).await.unwrap(); // Attached response

    // Small delay to ensure attach state is updated
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Auto-select should skip the attached session and pick id2
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    let auto_id = match list_resp {
        Response::SessionList { sessions } => {
            let mut candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            assert!(
                !candidates.is_empty(),
                "should have at least one unattached session"
            );
            candidates.sort_by(|a, b| a.created_at.cmp(&b.created_at));
            candidates[0].id
        }
        other => panic!("expected SessionList, got: {other:?}"),
    };

    assert_eq!(auto_id, id2, "should skip attached session and select id2");

    // Clean up (detach first)
    send_request(&mut writer1, &Request::Detach).await.unwrap();
    let _ = read_frame(&mut reader1).await.unwrap();
    request_response(&sock, &Request::Kill { id: id1 }).await;
    request_response(&sock, &Request::Kill { id: id2 }).await;
}

#[tokio::test]
async fn test_auto_attach_skips_completed() {
    let dir = temp_dir("auto-skip-done");
    let sock = start_test_daemon(&dir).await;

    // Launch a session that completes quickly
    let resp1 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "echo done".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id1 = match resp1 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Wait for it to complete
    tokio::time::sleep(Duration::from_secs(2)).await;

    // Launch a running session
    let resp2 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id2 = match resp2 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Auto-select should skip completed session
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    let auto_id = match list_resp {
        Response::SessionList { sessions } => {
            let mut candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            assert!(
                !candidates.is_empty(),
                "should have at least one running session"
            );
            candidates.sort_by(|a, b| a.created_at.cmp(&b.created_at));
            candidates[0].id
        }
        other => panic!("expected SessionList, got: {other:?}"),
    };

    assert_eq!(
        auto_id, id2,
        "should skip completed session and select running id2"
    );
    assert_ne!(auto_id, id1, "should not select completed session");

    // Clean up
    request_response(&sock, &Request::Kill { id: id2 }).await;
}

#[tokio::test]
async fn test_auto_attach_no_candidates() {
    let dir = temp_dir("auto-no-cand");
    let sock = start_test_daemon(&dir).await;

    // No sessions at all - test the error case
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    match list_resp {
        Response::SessionList { sessions } => {
            let candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            assert!(
                candidates.is_empty(),
                "should have no candidates when no sessions exist"
            );
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Launch a session and attach to it
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Attach to it
    let stream = UnixStream::connect(&sock).await.unwrap();
    let (mut reader, mut writer) = stream.into_split();
    send_request(&mut writer, &Request::Attach { id })
        .await
        .unwrap();
    let _ = read_frame(&mut reader).await.unwrap();

    tokio::time::sleep(Duration::from_millis(200)).await;

    // Now no unattached sessions available
    let list_resp = request_response(&sock, &Request::ListSessions).await;
    match list_resp {
        Response::SessionList { sessions } => {
            let candidates: Vec<_> = sessions
                .into_iter()
                .filter(|s| s.status == "running" && !s.attached)
                .collect();
            assert!(candidates.is_empty(), "should have no unattached sessions");
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Clean up
    send_request(&mut writer, &Request::Detach).await.unwrap();
    let _ = read_frame(&mut reader).await.unwrap();
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_explicit_attach_still_works() {
    let dir = temp_dir("explicit-attach");
    let sock = start_test_daemon(&dir).await;

    // Launch two sessions
    let resp1 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id1 = match resp1 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp2 = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".to_string(),
        },
    )
    .await;
    let id2 = match resp2 {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(300)).await;

    // Explicitly attach to second session (not the oldest)
    let resp = request_response(&sock, &Request::Attach { id: id2 }).await;
    match resp {
        Response::Attached { id: attached_id } => {
            assert_eq!(
                attached_id, id2,
                "should attach to explicitly specified session"
            );
        }
        other => panic!("expected Attached, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id: id1 }).await;
    request_response(&sock, &Request::Kill { id: id2 }).await;
}

// ---------------------------------------------------------------------------
// WebSocket remote access tests
// ---------------------------------------------------------------------------

/// Find an available TCP port by binding to port 0.
fn find_available_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.local_addr().unwrap().port()
}

/// Start a daemon with WebSocket enabled and return (sock_path, ws_port).
async fn start_ws_test_daemon(data_dir: &Path) -> (PathBuf, u16) {
    let port = find_available_port();

    // Write config with WebSocket enabled
    std::fs::write(
        data_dir.join("config.toml"),
        format!("[daemon]\nlisten = \"127.0.0.1:{}\"\n", port),
    )
    .unwrap();

    let sock_path = start_test_daemon(data_dir).await;

    // Wait for the WebSocket server to be ready
    for _ in 0..50 {
        if tokio::net::TcpStream::connect(format!("127.0.0.1:{}", port))
            .await
            .is_ok()
        {
            return (sock_path, port);
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    panic!("WebSocket server did not start on port {}", port);
}

/// Helper: send a request via WebSocket and read one response.
async fn ws_request_response(port: u16, token: &str, req: &Request) -> Response {
    use codewire::connection::{FrameReader, FrameWriter};
    use futures::StreamExt;
    use tokio_tungstenite::connect_async;

    let url = format!("ws://127.0.0.1:{}/ws?token={}", port, token);
    let (ws, _) = connect_async(&url).await.unwrap();
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

#[tokio::test]
async fn test_ws_list_sessions() {
    let dir = temp_dir("ws-list");
    let (sock, port) = start_ws_test_daemon(&dir).await;

    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Launch a session via Unix socket
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".into(),
        },
    )
    .await;

    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // List sessions via WebSocket
    let resp = ws_request_response(port, &token, &Request::ListSessions).await;
    match resp {
        Response::SessionList { sessions } => {
            assert_eq!(sessions.len(), 1);
            assert_eq!(sessions[0].id, id);
        }
        other => panic!("expected SessionList, got: {other:?}"),
    }

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}

#[tokio::test]
async fn test_ws_launch_and_kill() {
    let dir = temp_dir("ws-launch-kill");
    let (_sock, port) = start_ws_test_daemon(&dir).await;

    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Launch session via WebSocket
    let resp = ws_request_response(
        port,
        &token,
        &Request::Launch {
            command: vec!["bash".into(), "-c".into(), "sleep 30".into()],
            working_dir: "/tmp".into(),
        },
    )
    .await;

    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    // Kill session via WebSocket
    let resp = ws_request_response(port, &token, &Request::Kill { id }).await;
    match resp {
        Response::Killed { id: killed_id } => assert_eq!(killed_id, id),
        other => panic!("expected Killed, got: {other:?}"),
    }
}

#[tokio::test]
async fn test_ws_auth_rejection() {
    let dir = temp_dir("ws-auth-reject");
    let (_sock, port) = start_ws_test_daemon(&dir).await;

    // Try connecting with wrong token — should get HTTP 401 (connection fails)
    let url = format!("ws://127.0.0.1:{}/ws?token=wrong-token", port);
    let result = tokio_tungstenite::connect_async(&url).await;
    assert!(
        result.is_err(),
        "expected connection to fail with bad token"
    );
}

#[tokio::test]
async fn test_ws_attach_and_receive_output() {
    let dir = temp_dir("ws-attach");
    let (sock, port) = start_ws_test_daemon(&dir).await;

    let token = std::fs::read_to_string(dir.join("token")).unwrap();

    // Launch a session that produces output periodically (so we can attach and still see some)
    let resp = request_response(
        &sock,
        &Request::Launch {
            command: vec![
                "bash".into(),
                "-c".into(),
                "for i in 1 2 3; do echo WS_ATTACH_$i; sleep 1; done".into(),
            ],
            working_dir: "/tmp".into(),
        },
    )
    .await;

    let id = match resp {
        Response::Launched { id } => id,
        other => panic!("expected Launched, got: {other:?}"),
    };

    tokio::time::sleep(Duration::from_millis(200)).await;

    // Attach via WebSocket
    use codewire::connection::{FrameReader, FrameWriter};
    use futures::StreamExt;

    let url = format!("ws://127.0.0.1:{}/ws?token={}", port, token);
    let (ws, _) = tokio_tungstenite::connect_async(&url).await.unwrap();
    let (ws_writer, ws_reader) = ws.split();
    let mut reader = FrameReader::WsClient(ws_reader);
    let mut writer = FrameWriter::WsClient(ws_writer);

    writer.send_request(&Request::Attach { id }).await.unwrap();

    // Read attached confirmation
    let frame = reader.read_frame().await.unwrap().unwrap();
    match frame {
        Frame::Control(payload) => {
            let resp = protocol::parse_response(&payload).unwrap();
            assert!(
                matches!(resp, Response::Attached { .. }),
                "expected Attached, got: {resp:?}"
            );
        }
        _ => panic!("expected control frame"),
    }

    // Read output data — should eventually see "WS_ATTACH_3"
    let mut collected = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);

    loop {
        tokio::select! {
            frame = reader.read_frame() => {
                match frame.unwrap() {
                    Some(Frame::Data(bytes)) => {
                        collected.extend_from_slice(&bytes);
                        let output = String::from_utf8_lossy(&collected);
                        if output.contains("WS_ATTACH_3") {
                            break;
                        }
                    }
                    Some(Frame::Control(payload)) => {
                        let resp = protocol::parse_response(&payload).unwrap();
                        match resp {
                            Response::Error { message } if message.contains("completed") => break,
                            _ => {}
                        }
                    }
                    None => panic!("connection closed before receiving output"),
                }
            }
            _ = tokio::time::sleep_until(deadline) => {
                panic!("timeout waiting for output, got: {}", String::from_utf8_lossy(&collected));
            }
        }
    }

    // Detach
    writer.send_request(&Request::Detach).await.unwrap();

    // Clean up
    request_response(&sock, &Request::Kill { id }).await;
}
