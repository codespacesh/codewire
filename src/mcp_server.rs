use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashMap;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

use crate::protocol::{Request, Response};

// ---------------------------------------------------------------------------
// MCP Protocol Types (JSON-RPC 2.0)
// ---------------------------------------------------------------------------

#[derive(Debug, Serialize, Deserialize)]
struct JsonRpcRequest {
    jsonrpc: String,
    id: Option<Value>,
    method: String,
    params: Option<Value>,
}

#[derive(Debug, Serialize)]
struct JsonRpcResponse {
    jsonrpc: String,
    id: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<JsonRpcError>,
}

#[derive(Debug, Serialize)]
struct JsonRpcError {
    code: i32,
    message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    data: Option<Value>,
}

#[derive(Debug, Serialize)]
struct Tool {
    name: String,
    description: String,
    #[serde(rename = "inputSchema")]
    input_schema: Value,
}

// ---------------------------------------------------------------------------
// MCP Server
// ---------------------------------------------------------------------------

pub async fn run_mcp_server(data_dir: std::path::PathBuf) -> Result<()> {
    let stdin = tokio::io::stdin();
    let mut stdout = tokio::io::stdout();
    let mut reader = BufReader::new(stdin);
    let mut line = String::new();

    // Server info
    let server_info = json!({
        "name": "codewire",
        "version": env!("CARGO_PKG_VERSION"),
    });

    loop {
        line.clear();
        let n = reader.read_line(&mut line).await?;
        if n == 0 {
            break; // EOF
        }

        let request: JsonRpcRequest = match serde_json::from_str(&line) {
            Ok(req) => req,
            Err(e) => {
                eprintln!("[mcp] invalid JSON-RPC: {}", e);
                continue;
            }
        };

        let response = match request.method.as_str() {
            "initialize" => JsonRpcResponse {
                jsonrpc: "2.0".to_string(),
                id: request.id,
                result: Some(json!({
                    "protocolVersion": "2024-11-05",
                    "capabilities": {
                        "tools": {}
                    },
                    "serverInfo": server_info
                })),
                error: None,
            },

            "tools/list" => {
                let tools = get_tools();
                JsonRpcResponse {
                    jsonrpc: "2.0".to_string(),
                    id: request.id,
                    result: Some(json!({ "tools": tools })),
                    error: None,
                }
            }

            "tools/call" => {
                let result = handle_tool_call(&data_dir, request.params).await;
                match result {
                    Ok(value) => JsonRpcResponse {
                        jsonrpc: "2.0".to_string(),
                        id: request.id,
                        result: Some(json!({ "content": [{ "type": "text", "text": value }] })),
                        error: None,
                    },
                    Err(e) => JsonRpcResponse {
                        jsonrpc: "2.0".to_string(),
                        id: request.id,
                        result: None,
                        error: Some(JsonRpcError {
                            code: -32603,
                            message: e.to_string(),
                            data: None,
                        }),
                    },
                }
            }

            _ => JsonRpcResponse {
                jsonrpc: "2.0".to_string(),
                id: request.id,
                result: None,
                error: Some(JsonRpcError {
                    code: -32601,
                    message: format!("method not found: {}", request.method),
                    data: None,
                }),
            },
        };

        let response_str = serde_json::to_string(&response)?;
        stdout.write_all(response_str.as_bytes()).await?;
        stdout.write_all(b"\n").await?;
        stdout.flush().await?;
    }

    Ok(())
}

fn get_tools() -> Vec<Tool> {
    vec![
        Tool {
            name: "codewire_list_sessions".to_string(),
            description: "List all CodeWire sessions with their status".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "status_filter": {
                        "type": "string",
                        "description": "Filter by status: 'all', 'running', or 'completed'",
                        "enum": ["all", "running", "completed"]
                    }
                }
            }),
        },
        Tool {
            name: "codewire_read_session_output".to_string(),
            description: "Read output from a session (snapshot, not live)".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "session_id": {
                        "type": "integer",
                        "description": "The session ID to read from"
                    },
                    "tail": {
                        "type": "integer",
                        "description": "Number of lines to show from end (optional)"
                    },
                    "max_chars": {
                        "type": "integer",
                        "description": "Maximum characters to return (default: 50000)"
                    }
                },
                "required": ["session_id"]
            }),
        },
        Tool {
            name: "codewire_send_input".to_string(),
            description: "Send input to a session without attaching".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "session_id": {
                        "type": "integer",
                        "description": "The session ID to send input to"
                    },
                    "input": {
                        "type": "string",
                        "description": "The input text to send"
                    },
                    "auto_newline": {
                        "type": "boolean",
                        "description": "Automatically add newline (default: true)"
                    }
                },
                "required": ["session_id", "input"]
            }),
        },
        Tool {
            name: "codewire_watch_session".to_string(),
            description: "Monitor a session in real-time (time-bounded)".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "session_id": {
                        "type": "integer",
                        "description": "The session ID to watch"
                    },
                    "include_history": {
                        "type": "boolean",
                        "description": "Include recent history (default: true)"
                    },
                    "history_lines": {
                        "type": "integer",
                        "description": "Number of history lines to include (default: 50)"
                    },
                    "max_duration_seconds": {
                        "type": "integer",
                        "description": "Maximum watch duration in seconds (default: 30)"
                    }
                },
                "required": ["session_id"]
            }),
        },
        Tool {
            name: "codewire_get_session_status".to_string(),
            description: "Get detailed status information for a session".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "session_id": {
                        "type": "integer",
                        "description": "The session ID to query"
                    }
                },
                "required": ["session_id"]
            }),
        },
        Tool {
            name: "codewire_launch_session".to_string(),
            description: "Launch a new CodeWire session".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "command": {
                        "type": "array",
                        "items": { "type": "string" },
                        "description": "Command and arguments to run"
                    },
                    "working_dir": {
                        "type": "string",
                        "description": "Working directory (defaults to current dir)"
                    }
                },
                "required": ["command"]
            }),
        },
        Tool {
            name: "codewire_kill_session".to_string(),
            description: "Terminate a running session".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "session_id": {
                        "type": "integer",
                        "description": "The session ID to kill"
                    }
                },
                "required": ["session_id"]
            }),
        },
    ]
}

async fn handle_tool_call(data_dir: &std::path::Path, params: Option<Value>) -> Result<String> {
    let params = params.ok_or_else(|| anyhow::anyhow!("missing params"))?;
    let name = params["name"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("missing tool name"))?;
    let arguments: HashMap<String, Value> =
        serde_json::from_value(params["arguments"].clone()).unwrap_or_default();

    match name {
        "codewire_list_sessions" => {
            let sessions = node_request(data_dir, &Request::ListSessions).await?;
            match sessions {
                Response::SessionList { sessions } => {
                    let filter = arguments
                        .get("status_filter")
                        .and_then(|v| v.as_str())
                        .unwrap_or("all");

                    let filtered: Vec<_> = sessions
                        .into_iter()
                        .filter(|s| match filter {
                            "running" => s.status.contains("running"),
                            "completed" => s.status.contains("completed"),
                            _ => true,
                        })
                        .collect();

                    Ok(serde_json::to_string_pretty(&filtered)?)
                }
                _ => Ok("Unexpected response".to_string()),
            }
        }

        "codewire_read_session_output" => {
            let session_id = arguments
                .get("session_id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow::anyhow!("missing session_id"))?
                as u32;
            let tail = arguments
                .get("tail")
                .and_then(|v| v.as_u64())
                .map(|n| n as usize);
            let max_chars = arguments
                .get("max_chars")
                .and_then(|v| v.as_u64())
                .unwrap_or(50000) as usize;

            let resp = node_request(
                data_dir,
                &Request::Logs {
                    id: session_id,
                    follow: false,
                    tail,
                },
            )
            .await?;

            match resp {
                Response::LogData { data, .. } => {
                    let truncated = if data.len() > max_chars {
                        format!("{}... [truncated]", &data[..max_chars])
                    } else {
                        data
                    };
                    Ok(truncated)
                }
                Response::Error { message } => Ok(format!("Error: {}", message)),
                _ => Ok("Unexpected response".to_string()),
            }
        }

        "codewire_send_input" => {
            let session_id = arguments
                .get("session_id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow::anyhow!("missing session_id"))?
                as u32;
            let input = arguments
                .get("input")
                .and_then(|v| v.as_str())
                .ok_or_else(|| anyhow::anyhow!("missing input"))?;
            let auto_newline = arguments
                .get("auto_newline")
                .and_then(|v| v.as_bool())
                .unwrap_or(true);

            let mut data = input.as_bytes().to_vec();
            if auto_newline && !data.ends_with(b"\n") {
                data.push(b'\n');
            }

            let resp = node_request(
                data_dir,
                &Request::SendInput {
                    id: session_id,
                    data,
                },
            )
            .await?;

            match resp {
                Response::InputSent { id, bytes } => {
                    Ok(format!("Sent {} bytes to session {}", bytes, id))
                }
                Response::Error { message } => Ok(format!("Error: {}", message)),
                _ => Ok("Unexpected response".to_string()),
            }
        }

        "codewire_watch_session" => {
            let session_id = arguments
                .get("session_id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow::anyhow!("missing session_id"))?
                as u32;
            let include_history = arguments
                .get("include_history")
                .and_then(|v| v.as_bool())
                .unwrap_or(true);
            let history_lines = arguments
                .get("history_lines")
                .and_then(|v| v.as_u64())
                .map(|n| n as usize);
            let max_duration = arguments
                .get("max_duration_seconds")
                .and_then(|v| v.as_u64())
                .unwrap_or(30);

            // Connect and watch
            let output = watch_session_timed(
                data_dir,
                session_id,
                include_history,
                history_lines,
                max_duration,
            )
            .await?;

            Ok(output)
        }

        "codewire_get_session_status" => {
            let session_id = arguments
                .get("session_id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow::anyhow!("missing session_id"))?
                as u32;

            let resp = node_request(data_dir, &Request::GetStatus { id: session_id }).await?;

            match resp {
                Response::SessionStatus { info, output_size } => {
                    let mut obj = serde_json::to_value(&info)?;
                    if let Some(o) = obj.as_object_mut() {
                        o.insert("output_size".to_string(), json!(output_size));
                    }
                    Ok(serde_json::to_string_pretty(&obj)?)
                }
                Response::Error { message } => Ok(format!("Error: {}", message)),
                _ => Ok("Unexpected response".to_string()),
            }
        }

        "codewire_launch_session" => {
            let command: Vec<String> = arguments
                .get("command")
                .and_then(|v| v.as_array())
                .ok_or_else(|| anyhow::anyhow!("missing command"))?
                .iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect();

            let working_dir = arguments
                .get("working_dir")
                .and_then(|v| v.as_str())
                .map(String::from)
                .unwrap_or_else(|| {
                    std::env::current_dir()
                        .map(|p| p.display().to_string())
                        .unwrap_or_else(|_| ".".to_string())
                });

            let resp = node_request(
                data_dir,
                &Request::Launch {
                    command,
                    working_dir,
                },
            )
            .await?;

            match resp {
                Response::Launched { id } => Ok(format!("Launched session {}", id)),
                Response::Error { message } => Ok(format!("Error: {}", message)),
                _ => Ok("Unexpected response".to_string()),
            }
        }

        "codewire_kill_session" => {
            let session_id = arguments
                .get("session_id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow::anyhow!("missing session_id"))?
                as u32;

            let resp = node_request(data_dir, &Request::Kill { id: session_id }).await?;

            match resp {
                Response::Killed { id } => Ok(format!("Killed session {}", id)),
                Response::Error { message } => Ok(format!("Error: {}", message)),
                _ => Ok("Unexpected response".to_string()),
            }
        }

        _ => Err(anyhow::anyhow!("unknown tool: {}", name)),
    }
}

async fn node_request(data_dir: &std::path::Path, req: &Request) -> Result<Response> {
    let sock = data_dir.join("server.sock");
    let stream = tokio::net::UnixStream::connect(&sock)
        .await
        .with_context(|| format!("connecting to {}", sock.display()))?;

    let (mut reader, mut writer) = stream.into_split();

    crate::protocol::send_request(&mut writer, req).await?;

    let frame = crate::protocol::read_frame(&mut reader)
        .await?
        .context("unexpected EOF from node")?;

    match frame {
        crate::protocol::Frame::Control(payload) => crate::protocol::parse_response(&payload),
        crate::protocol::Frame::Data(_) => Err(anyhow::anyhow!("unexpected data frame")),
    }
}

async fn watch_session_timed(
    data_dir: &std::path::Path,
    session_id: u32,
    include_history: bool,
    history_lines: Option<usize>,
    max_duration_seconds: u64,
) -> Result<String> {
    let sock = data_dir.join("server.sock");
    let stream = tokio::net::UnixStream::connect(&sock)
        .await
        .with_context(|| format!("connecting to {}", sock.display()))?;

    let (mut reader, mut writer) = stream.into_split();

    crate::protocol::send_request(
        &mut writer,
        &Request::WatchSession {
            id: session_id,
            include_history,
            history_lines,
        },
    )
    .await?;

    let mut output = String::new();
    let timeout = tokio::time::sleep(tokio::time::Duration::from_secs(max_duration_seconds));
    tokio::pin!(timeout);

    loop {
        tokio::select! {
            frame = crate::protocol::read_frame(&mut reader) => {
                match frame? {
                    Some(crate::protocol::Frame::Control(payload)) => {
                        let resp = crate::protocol::parse_response(&payload)?;
                        match resp {
                            Response::WatchUpdate { status, output: out, done, .. } => {
                                if let Some(text) = out {
                                    output.push_str(&text);
                                }
                                if done {
                                    output.push_str(&format!("\n[Session {}]\n", status));
                                    break;
                                }
                            }
                            Response::Error { message } => {
                                return Err(anyhow::anyhow!("watch error: {}", message));
                            }
                            _ => {}
                        }
                    }
                    None => break,
                    _ => {}
                }
            }

            _ = &mut timeout => {
                output.push_str("\n[Watch timeout]\n");
                break;
            }
        }
    }

    if output.len() > 100000 {
        output.truncate(100000);
        output.push_str("\n... [output truncated to 100KB]");
    }

    Ok(output)
}
