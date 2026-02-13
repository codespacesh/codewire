use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// Frame types on the wire.
const FRAME_CONTROL: u8 = 0x00;
const FRAME_DATA: u8 = 0x01;

/// Maximum frame payload size (16 MB).
const MAX_PAYLOAD: u32 = 16 * 1024 * 1024;

// ---------------------------------------------------------------------------
// Control messages (JSON-encoded)
// ---------------------------------------------------------------------------

fn default_true() -> bool {
    true
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum Request {
    ListSessions,
    Launch {
        command: Vec<String>,
        working_dir: String,
    },
    Attach {
        id: u32,
        #[serde(default = "default_true")]
        include_history: bool,
        #[serde(default)]
        history_lines: Option<usize>,
    },
    Detach,
    Kill {
        id: u32,
    },
    KillAll,
    Resize {
        cols: u16,
        rows: u16,
    },
    Logs {
        id: u32,
        follow: bool,
        tail: Option<usize>,
    },
    SendInput {
        id: u32,
        data: Vec<u8>,
    },
    GetStatus {
        id: u32,
    },
    WatchSession {
        id: u32,
        include_history: bool,
        history_lines: Option<usize>,
    },
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum Response {
    SessionList {
        sessions: Vec<SessionInfo>,
    },
    Launched {
        id: u32,
    },
    Attached {
        id: u32,
    },
    Detached,
    Killed {
        id: u32,
    },
    KilledAll {
        count: usize,
    },
    Resized,
    LogData {
        data: String,
        done: bool,
    },
    InputSent {
        id: u32,
        bytes: usize,
    },
    SessionStatus {
        info: SessionInfo,
        output_size: u64,
    },
    WatchUpdate {
        id: u32,
        status: String,
        output: Option<String>,
        done: bool,
    },
    Error {
        message: String,
    },
    Ok,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionInfo {
    pub id: u32,
    pub prompt: String,
    pub working_dir: String,
    pub created_at: String,
    pub status: String,
    pub attached: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pid: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub output_size_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_output_snippet: Option<String>,
}

// ---------------------------------------------------------------------------
// Fleet protocol (JSON-over-NATS, separate from binary-framed Request/Response)
// ---------------------------------------------------------------------------

/// Daemon metadata advertised via NATS fleet discovery.
#[cfg(feature = "nats")]
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DaemonInfo {
    pub name: String,
    pub external_url: Option<String>,
    pub sessions: Vec<SessionInfo>,
    pub uptime_secs: u64,
}

/// Request sent over NATS to daemons.
#[cfg(feature = "nats")]
#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum FleetRequest {
    Discover,
    ListSessions,
    Launch {
        command: Vec<String>,
        working_dir: String,
    },
    Kill {
        id: u32,
    },
    GetStatus {
        id: u32,
    },
    SendInput {
        id: u32,
        data: Vec<u8>,
    },
}

/// Response from a daemon over NATS.
#[cfg(feature = "nats")]
#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum FleetResponse {
    DaemonInfo(DaemonInfo),
    SessionList {
        daemon: String,
        sessions: Vec<SessionInfo>,
    },
    Launched {
        daemon: String,
        id: u32,
    },
    Killed {
        daemon: String,
        id: u32,
    },
    SessionStatus {
        daemon: String,
        info: SessionInfo,
        output_size: u64,
    },
    InputSent {
        daemon: String,
        id: u32,
        bytes: usize,
    },
    Error {
        daemon: String,
        message: String,
    },
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

#[derive(Debug)]
pub enum Frame {
    Control(Vec<u8>),
    Data(Vec<u8>),
}

impl Frame {
    pub fn control(msg: &impl Serialize) -> Result<Self> {
        let json = serde_json::to_vec(msg)?;
        Ok(Frame::Control(json))
    }
}

// ---------------------------------------------------------------------------
// Wire format: [type: u8][length: u32 BE][payload]
// ---------------------------------------------------------------------------

pub async fn write_frame<W: AsyncWrite + Unpin>(writer: &mut W, frame: &Frame) -> Result<()> {
    let (frame_type, payload) = match frame {
        Frame::Control(data) => (FRAME_CONTROL, data.as_slice()),
        Frame::Data(data) => (FRAME_DATA, data.as_slice()),
    };

    let len = payload.len() as u32;
    let mut header = [0u8; 5];
    header[0] = frame_type;
    header[1..5].copy_from_slice(&len.to_be_bytes());

    writer.write_all(&header).await?;
    writer.write_all(payload).await?;
    writer.flush().await?;
    Ok(())
}

pub async fn read_frame<R: AsyncRead + Unpin>(reader: &mut R) -> Result<Option<Frame>> {
    let mut header = [0u8; 5];
    match reader.read_exact(&mut header).await {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(e) => return Err(e).context("reading frame header"),
    }

    let frame_type = header[0];
    let len = u32::from_be_bytes([header[1], header[2], header[3], header[4]]);

    if len > MAX_PAYLOAD {
        bail!("frame payload too large: {len} bytes");
    }

    let mut payload = vec![0u8; len as usize];
    reader
        .read_exact(&mut payload)
        .await
        .context("reading frame payload")?;

    match frame_type {
        FRAME_CONTROL => Ok(Some(Frame::Control(payload))),
        FRAME_DATA => Ok(Some(Frame::Data(payload))),
        other => bail!("unknown frame type: {other:#x}"),
    }
}

// ---------------------------------------------------------------------------
// Convenience helpers (used by integration tests and MCP server)
// ---------------------------------------------------------------------------

#[allow(dead_code)]
pub async fn send_request<W: AsyncWrite + Unpin>(writer: &mut W, req: &Request) -> Result<()> {
    let frame = Frame::control(req)?;
    write_frame(writer, &frame).await
}

#[allow(dead_code)]
pub async fn send_data<W: AsyncWrite + Unpin>(writer: &mut W, bytes: &[u8]) -> Result<()> {
    let frame = Frame::Data(bytes.to_vec());
    write_frame(writer, &frame).await
}

pub fn parse_request(payload: &[u8]) -> Result<Request> {
    serde_json::from_slice(payload).context("parsing request")
}

pub fn parse_response(payload: &[u8]) -> Result<Response> {
    serde_json::from_slice(payload).context("parsing response")
}
