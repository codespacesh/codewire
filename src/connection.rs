use anyhow::Result;

use crate::protocol::{self, Frame, Response};

// ---------------------------------------------------------------------------
// Frame reader — reads Frames from various transports
// ---------------------------------------------------------------------------

pub enum FrameReader {
    Unix(tokio::net::unix::OwnedReadHalf),

    #[cfg(feature = "ws")]
    WebSocket(futures::stream::SplitStream<axum::extract::ws::WebSocket>),

    #[cfg(feature = "ws")]
    WsClient(
        futures::stream::SplitStream<
            tokio_tungstenite::WebSocketStream<
                tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
            >,
        >,
    ),
}

impl FrameReader {
    pub async fn read_frame(&mut self) -> Result<Option<Frame>> {
        match self {
            FrameReader::Unix(r) => protocol::read_frame(r).await,

            #[cfg(feature = "ws")]
            FrameReader::WebSocket(r) => read_frame_from_axum_ws(r).await,

            #[cfg(feature = "ws")]
            FrameReader::WsClient(r) => read_frame_from_tungstenite_ws(r).await,
        }
    }
}

#[cfg(feature = "ws")]
async fn read_frame_from_axum_ws(
    r: &mut futures::stream::SplitStream<axum::extract::ws::WebSocket>,
) -> Result<Option<Frame>> {
    use futures::StreamExt;
    loop {
        match r.next().await {
            Some(Ok(axum::extract::ws::Message::Text(json))) => {
                return Ok(Some(Frame::Control(json.as_bytes().to_vec())));
            }
            Some(Ok(axum::extract::ws::Message::Binary(bytes))) => {
                return Ok(Some(Frame::Data(bytes.to_vec())));
            }
            Some(Ok(axum::extract::ws::Message::Close(_))) | None => {
                return Ok(None);
            }
            Some(Ok(_)) => continue, // skip Ping/Pong
            Some(Err(e)) => return Err(e.into()),
        }
    }
}

#[cfg(feature = "ws")]
async fn read_frame_from_tungstenite_ws(
    r: &mut futures::stream::SplitStream<
        tokio_tungstenite::WebSocketStream<
            tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
        >,
    >,
) -> Result<Option<Frame>> {
    use futures::StreamExt;
    use tokio_tungstenite::tungstenite::Message;
    loop {
        match r.next().await {
            Some(Ok(Message::Text(json))) => {
                return Ok(Some(Frame::Control(json.as_bytes().to_vec())));
            }
            Some(Ok(Message::Binary(bytes))) => {
                return Ok(Some(Frame::Data(bytes.into())));
            }
            Some(Ok(Message::Close(_))) | None => {
                return Ok(None);
            }
            Some(Ok(_)) => continue,
            Some(Err(e)) => return Err(e.into()),
        }
    }
}

// ---------------------------------------------------------------------------
// Frame writer — writes Frames to various transports
// ---------------------------------------------------------------------------

pub enum FrameWriter {
    Unix(tokio::net::unix::OwnedWriteHalf),

    #[cfg(feature = "ws")]
    WebSocket(futures::stream::SplitSink<axum::extract::ws::WebSocket, axum::extract::ws::Message>),

    #[cfg(feature = "ws")]
    WsClient(
        futures::stream::SplitSink<
            tokio_tungstenite::WebSocketStream<
                tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
            >,
            tokio_tungstenite::tungstenite::Message,
        >,
    ),
}

impl FrameWriter {
    pub async fn write_frame(&mut self, frame: &Frame) -> Result<()> {
        match self {
            FrameWriter::Unix(w) => protocol::write_frame(w, frame).await,

            #[cfg(feature = "ws")]
            FrameWriter::WebSocket(w) => {
                use futures::SinkExt;
                let msg = match frame {
                    Frame::Control(data) => axum::extract::ws::Message::Text(
                        String::from_utf8_lossy(data).into_owned().into(),
                    ),
                    Frame::Data(data) => axum::extract::ws::Message::Binary(data.clone().into()),
                };
                w.send(msg).await.map_err(Into::into)
            }

            #[cfg(feature = "ws")]
            FrameWriter::WsClient(w) => {
                use futures::SinkExt;
                use tokio_tungstenite::tungstenite::Message;
                let msg = match frame {
                    Frame::Control(data) => {
                        Message::Text(String::from_utf8_lossy(data).into_owned().into())
                    }
                    Frame::Data(data) => Message::Binary(data.clone().into()),
                };
                w.send(msg).await.map_err(Into::into)
            }
        }
    }

    pub async fn send_response(&mut self, resp: &Response) -> Result<()> {
        let frame = Frame::control(resp)?;
        self.write_frame(&frame).await
    }

    pub async fn send_data(&mut self, data: &[u8]) -> Result<()> {
        self.write_frame(&Frame::Data(data.to_vec())).await
    }

    pub async fn send_request(&mut self, req: &crate::protocol::Request) -> Result<()> {
        let frame = Frame::control(req)?;
        self.write_frame(&frame).await
    }
}
