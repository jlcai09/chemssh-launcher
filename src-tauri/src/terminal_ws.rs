use std::{io, time::Duration};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use http::header::{CONNECTION, SEC_WEBSOCKET_ACCEPT, SEC_WEBSOCKET_KEY, UPGRADE};
use http::{HeaderValue, Request, Response, StatusCode};
use hyper::body::Incoming;
use hyper_util::rt::TokioIo;
use subtle::ConstantTimeEq;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::chemssh_proxy::{ProxyBody, empty_body, header_str, json_error, query_param};
use crate::commands::terminal::RawTerminalEvent;
use crate::state::AppState;

const WS_GUID: &str = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
const WS_WRITE_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_OUTPUT_BATCH: usize = 16;
const WS_OP_BINARY: u8 = 0x2;
const WS_OP_CLOSE: u8 = 0x8;
const WS_OP_PING: u8 = 0x9;
const WS_OP_PONG: u8 = 0xA;

/// Pure-Rust SHA-1 (RFC 3174) used solely for the RFC 6455 Sec-WebSocket-Accept handshake.
fn sha1_digest(data: &[u8]) -> [u8; 20] {
    let mut h: [u32; 5] = [0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0];
    let bit_len = (data.len() as u64).wrapping_mul(8);
    let mut padded = Vec::with_capacity(((data.len() + 9) + 63) & !63);
    padded.extend_from_slice(data);
    padded.push(0x80);
    while (padded.len() % 64) != 56 {
        padded.push(0);
    }
    padded.extend_from_slice(&bit_len.to_be_bytes());

    for block in padded.chunks_exact(64) {
        let mut w = [0u32; 80];
        for (i, word) in block.chunks_exact(4).enumerate() {
            w[i] = u32::from_be_bytes([word[0], word[1], word[2], word[3]]);
        }
        for i in 16..80 {
            w[i] = (w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16]).rotate_left(1);
        }

        let [mut a, mut b, mut c, mut d, mut e] = h;
        for (i, &wi) in w.iter().enumerate() {
            let (f, k) = match i {
                0..=19 => ((b & c) | ((!b) & d), 0x5A827999u32),
                20..=39 => (b ^ c ^ d, 0x6ED9EBA1u32),
                40..=59 => ((b & c) | (b & d) | (c & d), 0x8F1BBCDCu32),
                _ => (b ^ c ^ d, 0xCA62C1D6u32),
            };
            let temp = a
                .rotate_left(5)
                .wrapping_add(f)
                .wrapping_add(e)
                .wrapping_add(k)
                .wrapping_add(wi);
            e = d;
            d = c;
            c = b.rotate_left(30);
            b = a;
            a = temp;
        }

        h[0] = h[0].wrapping_add(a);
        h[1] = h[1].wrapping_add(b);
        h[2] = h[2].wrapping_add(c);
        h[3] = h[3].wrapping_add(d);
        h[4] = h[4].wrapping_add(e);
    }

    let mut out = [0u8; 20];
    for (chunk, word) in out.chunks_exact_mut(4).zip(h) {
        chunk.copy_from_slice(&word.to_be_bytes());
    }
    out
}

pub(crate) fn compute_ws_accept(client_key: &str) -> String {
    let mut buf = String::with_capacity(client_key.len() + WS_GUID.len());
    buf.push_str(client_key.trim());
    buf.push_str(WS_GUID);
    BASE64.encode(sha1_digest(buf.as_bytes()))
}

pub(crate) fn encode_raw_terminal_frame(ev: &RawTerminalEvent) -> Vec<u8> {
    match ev {
        RawTerminalEvent::Output { seq, data } => {
            let mut out = Vec::with_capacity(9 + data.len());
            out.push(0x00);
            out.extend_from_slice(&seq.to_be_bytes());
            out.extend_from_slice(data);
            out
        }
        RawTerminalEvent::Exit { seq, .. } => {
            let mut out = Vec::with_capacity(9);
            out.push(0x01);
            out.extend_from_slice(&seq.to_be_bytes());
            out
        }
    }
}

async fn write_ws_frame_unflushed<W: AsyncWrite + Unpin>(
    writer: &mut W,
    opcode: u8,
    payload: &[u8],
) -> io::Result<()> {
    let mut header = [0u8; 10];
    header[0] = 0x80 | (opcode & 0x0f);
    let hdr_len = if payload.len() <= 125 {
        header[1] = payload.len() as u8;
        2
    } else if payload.len() <= u16::MAX as usize {
        header[1] = 126;
        header[2..4].copy_from_slice(&(payload.len() as u16).to_be_bytes());
        4
    } else {
        header[1] = 127;
        header[2..10].copy_from_slice(&(payload.len() as u64).to_be_bytes());
        10
    };
    writer.write_all(&header[..hdr_len]).await?;
    if !payload.is_empty() {
        writer.write_all(payload).await?;
    }
    Ok(())
}

pub(crate) async fn write_ws_frame<W: AsyncWrite + Unpin>(
    writer: &mut W,
    opcode: u8,
    payload: &[u8],
) -> io::Result<()> {
    write_ws_frame_unflushed(writer, opcode, payload).await?;
    writer.flush().await
}

/// The terminal socket only accepts WebSocket protocol control frames. Keep
/// partial bytes on the reader so the output branch can interrupt this future.
struct ControlReader {
    frame: [u8; 2 + 4 + 125],
    used: usize,
}

impl ControlReader {
    fn new() -> Self {
        Self {
            frame: [0; 2 + 4 + 125],
            used: 0,
        }
    }

    async fn read<R: AsyncRead + Unpin>(
        &mut self,
        reader: &mut R,
    ) -> io::Result<Option<(u8, Vec<u8>)>> {
        while self.used < 2 {
            let n = reader.read(&mut self.frame[self.used..2]).await?;
            if n == 0 && self.used == 0 {
                return Ok(None);
            }
            if n == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
            self.used += n;
        }

        let opcode = self.frame[0] & 0x0f;
        let len = usize::from(self.frame[1] & 0x7f);
        if self.frame[0] & 0xf0 != 0x80
            || self.frame[1] & 0x80 == 0
            || len > 125
            || !matches!(opcode, WS_OP_CLOSE | WS_OP_PING | WS_OP_PONG)
            || (opcode == WS_OP_CLOSE && len == 1)
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "invalid terminal WebSocket control frame",
            ));
        }

        let end = 6 + len;
        while self.used < end {
            let n = reader.read(&mut self.frame[self.used..end]).await?;
            if n == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
            self.used += n;
        }
        let payload = (0..len)
            .map(|i| self.frame[6 + i] ^ self.frame[2 + (i & 3)])
            .collect();
        self.used = 0;
        Ok(Some((opcode, payload)))
    }
}

async fn write_terminal_batch<W: AsyncWrite + Unpin>(
    writer: &mut W,
    events: impl IntoIterator<Item = RawTerminalEvent>,
) -> io::Result<()> {
    tokio::time::timeout(WS_WRITE_TIMEOUT, async {
        for event in events {
            write_ws_frame_unflushed(writer, WS_OP_BINARY, &encode_raw_terminal_frame(&event))
                .await?;
        }
        writer.flush().await
    })
    .await
    .map_err(|_| io::ErrorKind::TimedOut)?
}

pub async fn handle_terminal_ws(
    mut req: Request<Incoming>,
    state: &AppState,
) -> Response<ProxyBody> {
    let token = query_param(req.uri(), "token").unwrap_or_default();
    if token
        .as_bytes()
        .ct_eq(state.api_token.as_bytes())
        .unwrap_u8()
        != 1
    {
        return json_error(403, "invalid terminal websocket token", None);
    }

    let session_id = query_param(req.uri(), "session").unwrap_or_default();
    if session_id.trim().is_empty() {
        return json_error(400, "session query parameter is required", None);
    }
    let since: u64 = query_param(req.uri(), "since")
        .and_then(|v| v.parse::<u64>().ok())
        .unwrap_or(0);
    let Some(attempt) = query_param(req.uri(), "attempt")
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|&v| v > 0)
    else {
        return json_error(400, "terminal websocket attempt is required", None);
    };

    let Some(active) = state.terminals.get(&session_id) else {
        return json_error(
            404,
            format!("terminal session not found: {session_id}"),
            None,
        );
    };

    let upgrade_hdr = header_str(req.headers(), &UPGRADE).unwrap_or("");
    if !upgrade_hdr.eq_ignore_ascii_case("websocket") {
        return json_error(400, "missing Upgrade: websocket header", None);
    }
    let Some(ws_key) =
        header_str(req.headers(), &SEC_WEBSOCKET_KEY).filter(|s| !s.trim().is_empty())
    else {
        return json_error(400, "missing Sec-WebSocket-Key header", None);
    };
    let accept = compute_ws_accept(ws_key);
    let upgrade_fut = hyper::upgrade::on(&mut req);

    tokio::spawn(async move {
        let Ok(upgraded) = upgrade_fut.await else {
            return;
        };
        let Some(crate::commands::terminal::WsTerminalReplay {
            lost_through,
            latest_seq,
            events,
            mut rx,
            mut dropped,
            _guard,
        }) = active.attach_ws(since, attempt)
        else {
            return; // A delayed older upgrade cannot displace the new subscriber.
        };
        let io = TokioIo::new(upgraded);
        let (mut rd, mut wr) = tokio::io::split(io);

        // Ready marks the end of replay, even if it was empty. Its bounds and
        // initial events were captured under the same lock as subscription.
        let mut ready = [0u8; 17];
        ready[0] = 0x02;
        ready[1..9].copy_from_slice(&lost_through.to_be_bytes());
        ready[9..].copy_from_slice(&latest_seq.to_be_bytes());
        tokio::select! {
            biased;
            _ = &mut dropped => {
                // Do not drain an overflowing queue. A partial frame cannot be
                // resumed, so drop the socket and let the client replay.
            }
            _ = async {
                let replay_has_exit = matches!(events.last(), Some(RawTerminalEvent::Exit { .. }));
                let mut backlog = events.into_iter();
                let mut ready = Some(ready);
                let mut controls = ControlReader::new();
                loop {
                    tokio::select! {
                        biased;
                        frame_res = controls.read(&mut rd) => {
                            match frame_res {
                                Ok(Some((WS_OP_PING, payload))) => {
                                    if !matches!(tokio::time::timeout(
                                        WS_WRITE_TIMEOUT, write_ws_frame(&mut wr, WS_OP_PONG, &payload),
                                    ).await, Ok(Ok(()))) {
                                        break;
                                    }
                                }
                                Ok(Some((WS_OP_PONG, _))) => {}
                                Ok(Some((WS_OP_CLOSE, payload))) => {
                                    let _ = tokio::time::timeout(
                                        WS_WRITE_TIMEOUT, write_ws_frame(&mut wr, WS_OP_CLOSE, &payload),
                                    ).await;
                                    break;
                                }
                                _ => break,
                            }
                        }
                        _ = std::future::ready(()), if backlog.len() > 0 => {
                            if write_terminal_batch(
                                &mut wr, backlog.by_ref().take(MAX_OUTPUT_BATCH),
                            ).await.is_err() {
                                break;
                            }
                        }
                        _ = std::future::ready(()), if backlog.len() == 0 && ready.is_some() => {
                            if !matches!(tokio::time::timeout(
                                WS_WRITE_TIMEOUT, write_ws_frame(&mut wr, WS_OP_BINARY, &ready.take().unwrap()),
                            ).await, Ok(Ok(()))) {
                                break;
                            }
                            if replay_has_exit {
                                let _ = tokio::time::timeout(
                                    WS_WRITE_TIMEOUT, write_ws_frame(&mut wr, WS_OP_CLOSE, &[]),
                                ).await;
                                break;
                            }
                        }
                        ev_opt = rx.recv(), if backlog.len() == 0 && ready.is_none() => {
                            let Some(ev) = ev_opt else { break; };
                            let queued: Vec<_> = std::iter::once(ev).chain(
                                std::iter::from_fn(|| rx.try_recv().ok()).take(MAX_OUTPUT_BATCH - 1)
                            ).collect();
                            let events: Vec<_> = queued.iter().map(|queued| queued.event.clone()).collect();
                            let has_exit = matches!(events.last(), Some(RawTerminalEvent::Exit { .. }));
                            if write_terminal_batch(&mut wr, events).await.is_err() {
                                break;
                            }
                            if has_exit {
                                let _ = tokio::time::timeout(
                                    WS_WRITE_TIMEOUT, write_ws_frame(&mut wr, WS_OP_CLOSE, &[]),
                                ).await;
                                break;
                            }
                        }
                    }
                }
            } => {}
        }
    });

    let mut resp = Response::builder()
        .status(StatusCode::SWITCHING_PROTOCOLS)
        .header(UPGRADE, HeaderValue::from_static("websocket"))
        .header(CONNECTION, HeaderValue::from_static("Upgrade"))
        .body(empty_body())
        .unwrap_or_else(|_| Response::new(empty_body()));
    if let Ok(val) = HeaderValue::from_str(&accept) {
        resp.headers_mut().insert(SEC_WEBSOCKET_ACCEPT, val);
    }
    resp
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chemssh_proxy::{bind_proxy_listener, serve_proxy_listener};
    use core_secret::MemoryStore;
    use std::sync::Arc;
    use tempfile::TempDir;
    use tokio::net::TcpStream;

    fn masked_client_frame(opcode: u8, payload: &[u8]) -> Vec<u8> {
        let mask = [0x37u8, 0xFA, 0x21, 0x3D];
        let mut out = Vec::with_capacity(14 + payload.len());
        out.push(0x80 | (opcode & 0x0f));
        if payload.len() <= 125 {
            out.push(0x80 | (payload.len() as u8));
        } else if payload.len() <= u16::MAX as usize {
            out.push(0x80 | 126);
            out.extend_from_slice(&(payload.len() as u16).to_be_bytes());
        } else {
            out.push(0x80 | 127);
            out.extend_from_slice(&(payload.len() as u64).to_be_bytes());
        }
        out.extend_from_slice(&mask);
        for (i, b) in payload.iter().enumerate() {
            out.push(*b ^ mask[i & 3]);
        }
        out
    }

    async fn read_server_ws_frame<R: AsyncRead + Unpin>(
        reader: &mut R,
    ) -> io::Result<(u8, Vec<u8>)> {
        let mut hdr = [0u8; 2];
        reader.read_exact(&mut hdr).await?;
        let opcode = hdr[0] & 0x0f;
        let len7 = hdr[1] & 0x7f;
        let len: usize = match len7 {
            0..=125 => len7 as usize,
            126 => {
                let mut b = [0u8; 2];
                reader.read_exact(&mut b).await?;
                u16::from_be_bytes(b) as usize
            }
            _ => {
                let mut b = [0u8; 8];
                reader.read_exact(&mut b).await?;
                u64::from_be_bytes(b) as usize
            }
        };
        let mut payload = vec![0u8; len];
        if len > 0 {
            reader.read_exact(&mut payload).await?;
        }
        Ok((opcode, payload))
    }

    #[test]
    fn sha1_matches_rfc6455_accept_key_vector() {
        assert_eq!(
            compute_ws_accept("dGhlIHNhbXBsZSBub25jZQ=="),
            "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
        );
    }

    #[tokio::test]
    async fn ws_frame_roundtrip_unmasks_ping_and_encodes_pong() {
        let (mut client, mut server) = tokio::io::duplex(4096);
        let msg = b"ping-data";
        let raw = masked_client_frame(WS_OP_PING, msg);
        client.write_all(&raw).await.unwrap();
        client.flush().await.unwrap();

        let (op, decoded) = ControlReader::new()
            .read(&mut server)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(op, WS_OP_PING);
        assert_eq!(decoded, msg);

        write_ws_frame(&mut server, WS_OP_PONG, b"pong-data")
            .await
            .unwrap();
        let (srv_op, srv_payload) = read_server_ws_frame(&mut client).await.unwrap();
        assert_eq!(srv_op, WS_OP_PONG);
        assert_eq!(srv_payload, b"pong-data");
    }

    #[tokio::test]
    async fn pending_ping_survives_a_turn_of_output_work() {
        let (mut client, mut server) = tokio::io::duplex(128);
        let ping = masked_client_frame(WS_OP_PING, b"hello");
        let mut controls = ControlReader::new();
        client.write_all(&ping[..3]).await.unwrap();
        tokio::select! {
            biased;
            result = controls.read(&mut server) => panic!("partial ping completed: {result:?}"),
            _ = std::future::ready(()) => {}
        }
        client.write_all(&ping[3..]).await.unwrap();
        assert_eq!(
            controls.read(&mut server).await.unwrap(),
            Some((WS_OP_PING, b"hello".to_vec()))
        );
    }

    async fn connect_terminal_ws(addr: &str, token: &str, since: u64, attempt: u64) -> TcpStream {
        let mut sock = TcpStream::connect(addr).await.unwrap();
        let req = format!(
            "GET /api/terminal-ws?session=term-1&token={token}&since={since}&attempt={attempt} HTTP/1.1\r\nHost: {addr}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
        );
        sock.write_all(req.as_bytes()).await.unwrap();
        let mut head_buf = Vec::new();
        loop {
            let mut b = [0u8; 1];
            sock.read_exact(&mut b).await.unwrap();
            head_buf.push(b[0]);
            if head_buf.ends_with(b"\r\n\r\n") {
                break;
            }
        }
        let head = String::from_utf8_lossy(&head_buf);
        assert!(head.starts_with("HTTP/1.1 101"), "expected 101, got {head}");
        assert!(head.contains("s3pPLMBiTxaQ9kYGzzhZRbK+xOo="));
        sock
    }

    #[tokio::test]
    async fn end_to_end_terminal_ws_rejects_bad_token_and_streams_output() {
        let tmp = TempDir::new().unwrap();
        let state = Arc::new(AppState::new_custom(
            tmp.path().to_path_buf(),
            tmp.path().join("profiles.json"),
            tmp.path().join("settings.json"),
            tmp.path().join("cache"),
            Arc::new(MemoryStore::new()),
        ));
        let (active, _input_rx, _resize_rx) = state.terminals.insert_mock("term-1".into());
        active.push_raw_output(b"early-banner".to_vec());

        let (listener, base_url) = bind_proxy_listener().unwrap();
        let addr = base_url.strip_prefix("http://").unwrap().to_string();
        let server_state = Arc::clone(&state);
        tokio::spawn(async move {
            let _ = serve_proxy_listener(listener, server_state).await;
        });

        // 1. Bad token returns 403 Forbidden.
        {
            let mut bad_sock = TcpStream::connect(&addr).await.unwrap();
            let req = format!(
                "GET /api/terminal-ws?session=term-1&token=wrong HTTP/1.1\r\nHost: {addr}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
            );
            bad_sock.write_all(req.as_bytes()).await.unwrap();
            let mut buf = [0u8; 512];
            let n = bad_sock.read(&mut buf).await.unwrap();
            let head = String::from_utf8_lossy(&buf[..n]);
            assert!(head.starts_with("HTTP/1.1 403"), "unexpected head: {head}");
        }

        // 2. Valid upgrade receives backlog and streams live binary output.
        let mut sock = connect_terminal_ws(&addr, &state.api_token, 0, 1).await;

        let (op1, f1) = read_server_ws_frame(&mut sock).await.unwrap();
        assert_eq!(op1, WS_OP_BINARY);
        assert_eq!(f1[0], 0x00);
        assert_eq!(u64::from_be_bytes(f1[1..9].try_into().unwrap()), 1);
        assert_eq!(&f1[9..], b"early-banner");
        // Ready reports replay bounds after the complete backlog.
        let (op, ready) = read_server_ws_frame(&mut sock).await.unwrap();
        assert_eq!(op, WS_OP_BINARY);
        assert_eq!(
            ready,
            [0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1]
        );

        sock.write_all(&masked_client_frame(WS_OP_PING, b"still here"))
            .await
            .unwrap();
        assert_eq!(
            read_server_ws_frame(&mut sock).await.unwrap(),
            (WS_OP_PONG, b"still here".to_vec())
        );

        // Push live output and exit
        active.push_raw_output(b"file1.txt\r\n".to_vec());
        let (op2, f2) = read_server_ws_frame(&mut sock).await.unwrap();
        assert_eq!(op2, WS_OP_BINARY);
        assert_eq!(f2[0], 0x00);
        assert_eq!(u64::from_be_bytes(f2[1..9].try_into().unwrap()), 2);
        assert_eq!(&f2[9..], b"file1.txt\r\n");

        active.push_raw_exit(Some(0), "exited".into());
        let (op3, f3) = read_server_ws_frame(&mut sock).await.unwrap();
        assert_eq!(op3, WS_OP_BINARY);
        assert_eq!(f3, [0x01, 0, 0, 0, 0, 0, 0, 0, 3]);

        // A reconnect from sequence 2 receives only the retained exit, and
        // must still receive Ready before the server closes that socket.
        let mut resumed = connect_terminal_ws(&addr, &state.api_token, 2, 2).await;
        assert_eq!(
            read_server_ws_frame(&mut resumed).await.unwrap(),
            (WS_OP_BINARY, f3)
        );
        assert_eq!(
            read_server_ws_frame(&mut resumed).await.unwrap(),
            (
                WS_OP_BINARY,
                vec![0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3]
            ),
        );
        assert_eq!(
            read_server_ws_frame(&mut resumed).await.unwrap(),
            (WS_OP_CLOSE, vec![])
        );
    }
}
