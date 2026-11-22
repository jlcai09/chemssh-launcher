use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use core_config::X11Config;
use rand::RngExt;
use subtle::ConstantTimeEq;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::TcpStream;
#[cfg(unix)]
use tokio::net::UnixStream;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::time::timeout;

use crate::SshError;

pub const X11_MAX_CONCURRENT_CONNECTIONS: usize = 32;
const X11_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(5);
const X11_CONNECT_TIMEOUT: Duration = Duration::from_secs(1);
const X11_AUTH_PROTOCOL: &str = "MIT-MAGIC-COOKIE-1";
const X11_MAX_AUTH_LEN: usize = 4096;
const X11_INITIAL_READ_LIMIT: usize = 16 * 1024;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum X11Target {
    Tcp { host: String, port: u16 },
    Unix(PathBuf),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedX11Display {
    pub raw: String,
    pub target: X11Target,
    pub display_num: u32,
    pub screen_num: u32,
    pub host_for_auth: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PreparedX11 {
    pub display: ParsedX11Display,
    pub fake_cookie: [u8; 16],
    pub fake_cookie_hex: String,
    pub real_proto: String,
    pub real_cookie: Vec<u8>,
}

#[derive(Clone)]
pub struct X11ForwarderState {
    inner: Arc<X11ForwarderInner>,
}

struct X11ForwarderInner {
    target: X11Target,
    fake_cookie: [u8; 16],
    real_proto: String,
    real_cookie: Vec<u8>,
    semaphore: Arc<Semaphore>,
}

impl X11ForwarderState {
    pub fn from_prepared(prepared: &PreparedX11) -> Self {
        Self {
            inner: Arc::new(X11ForwarderInner {
                target: prepared.display.target.clone(),
                fake_cookie: prepared.fake_cookie,
                real_proto: prepared.real_proto.clone(),
                real_cookie: prepared.real_cookie.clone(),
                semaphore: Arc::new(Semaphore::new(X11_MAX_CONCURRENT_CONNECTIONS)),
            }),
        }
    }

    pub fn try_acquire_permit(&self) -> Option<OwnedSemaphorePermit> {
        Arc::clone(&self.inner.semaphore).try_acquire_owned().ok()
    }

    pub async fn forward_stream<S>(&self, mut remote_stream: S) -> Result<(), SshError>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        let (rewritten_setup, trailing) = timeout(
            X11_HANDSHAKE_TIMEOUT,
            read_and_rewrite_setup_packet(
                &mut remote_stream,
                &self.inner.fake_cookie,
                &self.inner.real_proto,
                &self.inner.real_cookie,
            ),
        )
        .await
        .map_err(|_| SshError::Protocol("x11 client handshake timed out".into()))??;

        let mut local_stream =
            timeout(X11_HANDSHAKE_TIMEOUT, connect_x11_target(&self.inner.target))
                .await
                .map_err(|_| SshError::Protocol("local x11 server connect timed out".into()))??;

        local_stream
            .write_all(&rewritten_setup)
            .await
            .map_err(SshError::Io)?;
        if !trailing.is_empty() {
            local_stream.write_all(&trailing).await.map_err(SshError::Io)?;
        }
        local_stream.flush().await.map_err(SshError::Io)?;

        match &mut local_stream {
            LocalX11Stream::Tcp(tcp) => {
                let _ = tokio::io::copy_bidirectional(&mut remote_stream, tcp).await;
            }
            #[cfg(unix)]
            LocalX11Stream::Unix(unix) => {
                let _ = tokio::io::copy_bidirectional(&mut remote_stream, unix).await;
            }
        }
        Ok(())
    }
}

enum LocalX11Stream {
    Tcp(TcpStream),
    #[cfg(unix)]
    Unix(UnixStream),
}

impl LocalX11Stream {
    async fn read_exact(&mut self, buf: &mut [u8]) -> std::io::Result<()> {
        match self {
            Self::Tcp(s) => s.read_exact(buf).await.map(|_| ()),
            #[cfg(unix)]
            Self::Unix(s) => s.read_exact(buf).await.map(|_| ()),
        }
    }

    async fn write_all(&mut self, buf: &[u8]) -> std::io::Result<()> {
        match self {
            Self::Tcp(s) => s.write_all(buf).await,
            #[cfg(unix)]
            Self::Unix(s) => s.write_all(buf).await,
        }
    }

    async fn flush(&mut self) -> std::io::Result<()> {
        match self {
            Self::Tcp(s) => s.flush().await,
            #[cfg(unix)]
            Self::Unix(s) => s.flush().await,
        }
    }
}

async fn connect_x11_target(target: &X11Target) -> Result<LocalX11Stream, SshError> {
    match target {
        X11Target::Tcp { host, port } => {
            let connect_host = if host.contains(':') && !host.starts_with('[') {
                format!("[{host}]:{port}")
            } else {
                format!("{host}:{port}")
            };
            let tcp = TcpStream::connect(&connect_host).await.map_err(|err| {
                SshError::Protocol(format!(
                    "failed to connect to local X11 server at {connect_host}: {err}"
                ))
            })?;
            let _ = tcp.set_nodelay(true);
            Ok(LocalX11Stream::Tcp(tcp))
        }
        #[cfg(unix)]
        X11Target::Unix(path) => {
            let unix = UnixStream::connect(path).await.map_err(|err| {
                SshError::Protocol(format!(
                    "failed to connect to local X11 unix socket {}: {err}",
                    path.display()
                ))
            })?;
            Ok(LocalX11Stream::Unix(unix))
        }
        #[cfg(not(unix))]
        X11Target::Unix(path) => Err(SshError::Protocol(format!(
            "unix domain X11 socket {} is not supported on this platform",
            path.display()
        ))),
    }
}

pub fn parse_x11_display(raw_display: &str) -> Result<ParsedX11Display, SshError> {
    let trimmed = raw_display.trim();
    let candidate = if trimmed.is_empty() {
        std::env::var("DISPLAY").unwrap_or_default().trim().to_string()
    } else {
        trimmed.to_string()
    };

    if candidate.is_empty() {
        return Err(SshError::Config(
            "X11 forwarding requires DISPLAY or an explicit X11 display setting".into(),
        ));
    }

    if let Some(rest) = candidate.strip_prefix("unix:") {
        #[cfg(not(unix))]
        {
            let _ = rest;
            return Err(SshError::Config(
                "X11 display must be a loopback host:display[.screen]".into(),
            ));
        }
        #[cfg(unix)]
        {
            if rest.starts_with('/') {
                let (sock_path, display_num, screen_num) = parse_unix_socket_path(rest);
                return Ok(ParsedX11Display {
                    raw: candidate,
                    target: X11Target::Unix(sock_path),
                    display_num,
                    screen_num,
                    host_for_auth: String::new(),
                });
            }
            let clean = rest.strip_prefix(':').unwrap_or(rest);
            let (display_num, screen_num) = parse_display_screen(clean)?;
            return Ok(ParsedX11Display {
                raw: candidate,
                target: X11Target::Unix(PathBuf::from(format!("/tmp/.X11-unix/X{display_num}"))),
                display_num,
                screen_num,
                host_for_auth: String::new(),
            });
        }
    }

    if candidate.starts_with('/') {
        #[cfg(not(unix))]
        return Err(SshError::Config(
            "X11 display must be a loopback host:display[.screen]".into(),
        ));
        #[cfg(unix)]
        {
            let (sock_path, display_num, screen_num) = parse_unix_socket_path(&candidate);
            return Ok(ParsedX11Display {
                raw: candidate,
                target: X11Target::Unix(sock_path),
                display_num,
                screen_num,
                host_for_auth: String::new(),
            });
        }
    }

    let Some(colon_idx) = candidate.rfind(':') else {
        return Err(SshError::Config(
            "X11 display must be a loopback host:display[.screen]".into(),
        ));
    };
    let host_raw = candidate[..colon_idx].trim().to_string();
    let disp_part = candidate[colon_idx + 1..].trim().to_string();
    let (display_num, screen_num) = parse_display_screen(&disp_part)?;

    let port = 6000u32.checked_add(display_num).ok_or_else(|| {
        SshError::Config(format!("invalid X11 display number {display_num}"))
    })?;
    if port > u16::MAX as u32 {
        return Err(SshError::Config(format!(
            "X11 display {display_num} exceeds TCP port range"
        )));
    }

    let host_clean = host_raw
        .strip_prefix('[')
        .and_then(|s| s.strip_suffix(']'))
        .unwrap_or(&host_raw);

    if host_clean.eq_ignore_ascii_case("unix") {
        #[cfg(unix)]
        {
            let sock = PathBuf::from(format!("/tmp/.X11-unix/X{display_num}"));
            return Ok(ParsedX11Display {
                raw: candidate,
                target: X11Target::Unix(sock),
                display_num,
                screen_num,
                host_for_auth: String::new(),
            });
        }
        #[cfg(not(unix))]
        return Err(SshError::Config(
            "X11 display must use a loopback IP or localhost".into(),
        ));
    }

    if host_clean.is_empty() {
        #[cfg(unix)]
        {
            let sock = PathBuf::from(format!("/tmp/.X11-unix/X{display_num}"));
            if sock.exists() {
                return Ok(ParsedX11Display {
                    raw: candidate,
                    target: X11Target::Unix(sock),
                    display_num,
                    screen_num,
                    host_for_auth: String::new(),
                });
            }
        }
        return Ok(ParsedX11Display {
            raw: candidate,
            target: X11Target::Tcp {
                host: "127.0.0.1".into(),
                port: port as u16,
            },
            display_num,
            screen_num,
            host_for_auth: "127.0.0.1".into(),
        });
    }

    let normalized_host = if host_clean.eq_ignore_ascii_case("localhost") {
        "127.0.0.1".to_string()
    } else {
        let ip: std::net::IpAddr = host_clean.parse().map_err(|_| {
            SshError::Config("X11 display must use a loopback IP or localhost".into())
        })?;
        if !ip.is_loopback() {
            return Err(SshError::Config(
                "X11 display must use a loopback IP or localhost".into(),
            ));
        }
        host_clean.to_string()
    };

    Ok(ParsedX11Display {
        raw: candidate,
        target: X11Target::Tcp {
            host: normalized_host.clone(),
            port: port as u16,
        },
        display_num,
        screen_num,
        host_for_auth: normalized_host,
    })
}

fn parse_display_screen(part: &str) -> Result<(u32, u32), SshError> {
    if part.is_empty() {
        return Err(SshError::Config("empty X11 display number".into()));
    }
    if let Some((disp_str, screen_str)) = part.split_once('.') {
        let disp: u32 = disp_str
            .parse()
            .map_err(|_| SshError::Config(format!("invalid X11 display number {disp_str:?}")))?;
        let screen: u32 = screen_str
            .parse()
            .map_err(|_| SshError::Config(format!("invalid X11 screen number {screen_str:?}")))?;
        Ok((disp, screen))
    } else {
        let disp: u32 = part
            .parse()
            .map_err(|_| SshError::Config(format!("invalid X11 display number {part:?}")))?;
        Ok((disp, 0))
    }
}

#[cfg(unix)]
fn parse_unix_socket_path(raw: &str) -> (PathBuf, u32, u32) {
    let direct = PathBuf::from(raw);
    if direct.exists() {
        let disp = direct
            .file_name()
            .and_then(|s| s.to_str())
            .and_then(|s| s.strip_prefix('X'))
            .and_then(|s| s.parse::<u32>().ok())
            .unwrap_or(0);
        return (direct, disp, 0);
    }

    if let Some((base, suffix)) = raw.rsplit_once('.') {
        if let Ok(screen) = suffix.parse::<u32>() {
            let base_path = PathBuf::from(base);
            if base_path.exists() {
                let disp = base_path
                    .file_name()
                    .and_then(|s| s.to_str())
                    .and_then(|s| s.strip_prefix('X'))
                    .and_then(|s| s.parse::<u32>().ok())
                    .unwrap_or(0);
                return (base_path, disp, screen);
            }
        }
    }

    (direct, 0, 0)
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct XauthEntry {
    pub family: u16,
    pub address: Vec<u8>,
    pub number: Vec<u8>,
    pub name: Vec<u8>,
    pub data: Vec<u8>,
}

pub fn parse_xauthority_bytes(bytes: &[u8]) -> Result<Vec<XauthEntry>, SshError> {
    let mut entries = Vec::new();
    let mut cursor = 0usize;

    let read_u16_be = |buf: &[u8], pos: &mut usize| -> Result<Option<u16>, SshError> {
        if *pos >= buf.len() {
            return Ok(None);
        }
        if *pos + 2 > buf.len() {
            return Err(SshError::Protocol("truncated Xauthority record header".into()));
        }
        let val = u16::from_be_bytes([buf[*pos], buf[*pos + 1]]);
        *pos += 2;
        Ok(Some(val))
    };

    let read_slice = |buf: &[u8], pos: &mut usize| -> Result<Vec<u8>, SshError> {
        if *pos + 2 > buf.len() {
            return Err(SshError::Protocol("truncated Xauthority field length".into()));
        }
        let len = u16::from_be_bytes([buf[*pos], buf[*pos + 1]]) as usize;
        *pos += 2;
        if *pos + len > buf.len() {
            return Err(SshError::Protocol("truncated Xauthority field data".into()));
        }
        let out = buf[*pos..*pos + len].to_vec();
        *pos += len;
        Ok(out)
    };

    while let Some(family) = read_u16_be(bytes, &mut cursor)? {
        let address = read_slice(bytes, &mut cursor)?;
        let number = read_slice(bytes, &mut cursor)?;
        let name = read_slice(bytes, &mut cursor)?;
        let data = read_slice(bytes, &mut cursor)?;
        entries.push(XauthEntry {
            family,
            address,
            number,
            name,
            data,
        });
    }

    Ok(entries)
}

pub fn read_xauthority(
    explicit_path: &str,
    display: &ParsedX11Display,
) -> Result<Option<(String, Vec<u8>)>, SshError> {
    let resolved = resolve_xauthority_path(explicit_path);
    let Some(path) = resolved else {
        return Ok(None);
    };

    if !path.exists() {
        if !explicit_path.trim().is_empty() {
            return Err(SshError::Config(format!(
                "configured Xauthority file does not exist: {}",
                path.display()
            )));
        }
        return Ok(None);
    }

    let bytes = std::fs::read(&path).map_err(|err| {
        SshError::Config(format!(
            "failed to read Xauthority file {}: {err}",
            path.display()
        ))
    })?;
    let entries = parse_xauthority_bytes(&bytes)?;
    let disp_num_bytes = display.display_num.to_string().into_bytes();
    let local_hostname = detect_local_hostname();

    let mut fallback: Option<Vec<u8>> = None;
    for entry in entries {
        if entry.name != X11_AUTH_PROTOCOL.as_bytes() {
            continue;
        }
        if !entry.number.is_empty() && entry.number != disp_num_bytes {
            continue;
        }
        if entry.data.is_empty() {
            continue;
        }
        if xauth_entry_matches(&entry, display, &local_hostname) {
            return Ok(Some((X11_AUTH_PROTOCOL.to_string(), entry.data)));
        }
        if fallback.is_none() && xauth_entry_loopback_fallback(&entry, display) {
            fallback = Some(entry.data);
        }
    }

    Ok(fallback.map(|data| (X11_AUTH_PROTOCOL.to_string(), data)))
}

fn xauth_entry_matches(
    entry: &XauthEntry,
    display: &ParsedX11Display,
    local_hostname: &str,
) -> bool {
    const FAMILY_INTERNET: u16 = 0;
    const FAMILY_INTERNET6: u16 = 6;
    const FAMILY_LOCALHOST: u16 = 252;
    const FAMILY_LOCAL: u16 = 256;
    const FAMILY_WILD: u16 = 65535;

    match entry.family {
        FAMILY_WILD | FAMILY_LOCALHOST => true,
        FAMILY_LOCAL => {
            if !local_hostname.is_empty() && entry.address == local_hostname.as_bytes() {
                return true;
            }
            if !display.host_for_auth.is_empty()
                && entry.address == display.host_for_auth.as_bytes()
            {
                return true;
            }
            entry.address == b"localhost" || entry.address.is_empty()
        }
        FAMILY_INTERNET => {
            if let Ok(ipv4) = <[u8; 4]>::try_from(entry.address.as_slice()) {
                let ip = std::net::Ipv4Addr::from(ipv4);
                if matches!(display.target, X11Target::Unix(_)) && ip.is_loopback() {
                    return true;
                }
                if let X11Target::Tcp { host, .. } = &display.target {
                    if is_loopback_host(host) && ip.is_loopback() {
                        return true;
                    }
                    if let Ok(target_ip) = host.parse::<std::net::Ipv4Addr>() {
                        return ip == target_ip;
                    }
                }
            }
            false
        }
        FAMILY_INTERNET6 => {
            if let Ok(ipv6) = <[u8; 16]>::try_from(entry.address.as_slice()) {
                let ip = std::net::Ipv6Addr::from(ipv6);
                if matches!(display.target, X11Target::Unix(_)) && ip.is_loopback() {
                    return true;
                }
                if let X11Target::Tcp { host, .. } = &display.target {
                    if is_loopback_host(host) && ip.is_loopback() {
                        return true;
                    }
                    if let Ok(target_ip) = host.parse::<std::net::Ipv6Addr>() {
                        return ip == target_ip;
                    }
                }
            }
            false
        }
        _ => false,
    }
}

fn xauth_entry_loopback_fallback(entry: &XauthEntry, display: &ParsedX11Display) -> bool {
    let target_is_local = match &display.target {
        X11Target::Unix(_) => true,
        X11Target::Tcp { host, .. } => is_loopback_host(host),
    };
    if !target_is_local {
        return false;
    }
    entry.family == 256 || entry.family == 0 || entry.family == 6
}

fn is_loopback_host(host: &str) -> bool {
    host.eq_ignore_ascii_case("localhost")
        || host == "127.0.0.1"
        || host == "::1"
        || host.parse::<std::net::IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false)
}

fn resolve_xauthority_path(explicit_path: &str) -> Option<PathBuf> {
    let trimmed = explicit_path.trim();
    if !trimmed.is_empty() {
        if let Some(rest) = trimmed.strip_prefix("~/").or_else(|| trimmed.strip_prefix("~\\")) {
            if let Some(home) = dirs_home() {
                return Some(home.join(rest));
            }
        }
        return Some(PathBuf::from(trimmed));
    }

    if let Some(env_path) = std::env::var_os("XAUTHORITY") {
        let p = PathBuf::from(env_path);
        if !p.as_os_str().is_empty() {
            return Some(p);
        }
    }

    dirs_home().map(|h| h.join(".Xauthority"))
}

fn dirs_home() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        std::env::var_os("USERPROFILE")
            .map(PathBuf::from)
            .or_else(|| {
                let drive = std::env::var_os("HOMEDRIVE")?;
                let path = std::env::var_os("HOMEPATH")?;
                let mut combined = drive;
                combined.push(path);
                Some(PathBuf::from(combined))
            })
    }
    #[cfg(not(windows))]
    {
        std::env::var_os("HOME").map(PathBuf::from)
    }
}

fn detect_local_hostname() -> String {
    if let Ok(h) = std::env::var("HOSTNAME") {
        let trimmed = h.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    if let Ok(h) = std::env::var("COMPUTERNAME") {
        let trimmed = h.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    if let Ok(contents) = std::fs::read_to_string("/etc/hostname") {
        let trimmed = contents.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    String::new()
}

pub fn build_x11_setup_packet(proto: &[u8], cookie: &[u8]) -> Result<Vec<u8>, SshError> {
    let proto_len = u16::try_from(proto.len())
        .map_err(|_| SshError::Protocol("X11 auth protocol name too long".into()))?;
    let cookie_len = u16::try_from(cookie.len())
        .map_err(|_| SshError::Protocol("X11 auth cookie too long".into()))?;

    let mut out = Vec::with_capacity(12 + pad4(proto.len()) + pad4(cookie.len()));
    out.push(b'l');
    out.push(0);
    out.extend_from_slice(&11u16.to_le_bytes());
    out.extend_from_slice(&0u16.to_le_bytes());
    out.extend_from_slice(&proto_len.to_le_bytes());
    out.extend_from_slice(&cookie_len.to_le_bytes());
    out.extend_from_slice(&[0u8, 0u8]);
    out.extend_from_slice(proto);
    out.resize(12 + pad4(proto.len()), 0);
    out.extend_from_slice(cookie);
    out.resize(12 + pad4(proto.len()) + pad4(cookie.len()), 0);
    Ok(out)
}

pub async fn verify_x_server(
    target: &X11Target,
    real_proto: &str,
    real_cookie: &[u8],
) -> Result<(), SshError> {
    let setup = build_x11_setup_packet(real_proto.as_bytes(), real_cookie)?;
    timeout(X11_CONNECT_TIMEOUT, async {
        let mut stream = connect_x11_target(target).await?;
        stream.write_all(&setup).await.map_err(|_| {
            SshError::X11("write local X11 authentication handshake failed".into())
        })?;
        stream.flush().await.map_err(|_| {
            SshError::X11("flush local X11 authentication handshake failed".into())
        })?;
        let mut header = [0u8; 8];
        stream.read_exact(&mut header).await.map_err(|_| {
            SshError::X11("local X server did not complete authentication".into())
        })?;
        if header[0] != 1
            || u16::from_le_bytes([header[2], header[3]]) != 11
            || u16::from_le_bytes([header[4], header[5]]) != 0
        {
            return Err(SshError::X11(
                "local X server rejected X11 authentication".into(),
            ));
        }
        Ok(())
    })
    .await
    .map_err(|_| {
        SshError::X11(format!(
            "local X server at {} did not complete handshake within {}s",
            describe_target(target),
            X11_CONNECT_TIMEOUT.as_secs()
        ))
    })?
}

fn describe_target(target: &X11Target) -> String {
    match target {
        X11Target::Tcp { host, port } => format!("{host}:{port}"),
        X11Target::Unix(path) => path.display().to_string(),
    }
}

pub async fn prepare_local_x_server(config: &X11Config) -> Result<PreparedX11, SshError> {
    let normalized = config.normalized();
    normalized
        .validate()
        .map_err(|e| SshError::Config(e.to_string()))?;

    let display = parse_x11_display(&normalized.display)?;
    let auth = read_xauthority(&normalized.xauthority, &display)?;
    let (real_proto, real_cookie) = auth.unwrap_or_default();

    verify_x_server(&display.target, &real_proto, &real_cookie)
        .await
        .map_err(|err| {
            SshError::X11(format!(
                "local X server unavailable or access denied at {}; start the server and check its access policy or Xauthority: {err}",
                describe_target(&display.target)
            ))
        })?;

    let mut fake_cookie = [0u8; 16];
    rand::rng().fill(&mut fake_cookie);
    let fake_cookie_hex = hex::encode(fake_cookie);

    Ok(PreparedX11 {
        display,
        fake_cookie,
        fake_cookie_hex,
        real_proto,
        real_cookie,
    })
}

async fn read_and_rewrite_setup_packet<R: AsyncRead + Unpin>(
    reader: &mut R,
    fake_cookie: &[u8; 16],
    real_proto: &str,
    real_cookie: &[u8],
) -> Result<(Vec<u8>, Vec<u8>), SshError> {
    let mut buf = Vec::with_capacity(256);
    let mut header = [0u8; 12];
    reader.read_exact(&mut header).await.map_err(SshError::Io)?;
    buf.extend_from_slice(&header);

    let big_endian = match header[0] {
        b'B' => true,
        b'l' => false,
        other => {
            return Err(SshError::Protocol(format!(
                "invalid X11 client byte order 0x{other:02x}"
            )));
        }
    };

    let read_u16 = |b: &[u8], offset: usize| -> usize {
        if big_endian {
            u16::from_be_bytes([b[offset], b[offset + 1]]) as usize
        } else {
            u16::from_le_bytes([b[offset], b[offset + 1]]) as usize
        }
    };

    let proto_len = read_u16(&header, 6);
    let data_len = read_u16(&header, 8);
    if proto_len > X11_MAX_AUTH_LEN || data_len > X11_MAX_AUTH_LEN {
        return Err(SshError::Protocol(
            "X11 authorization field exceeds size limit".into(),
        ));
    }

    let padded_proto_len = pad4(proto_len);
    let padded_data_len = pad4(data_len);
    let total_setup_len = 12 + padded_proto_len + padded_data_len;
    if total_setup_len > X11_INITIAL_READ_LIMIT {
        return Err(SshError::Protocol(
            "X11 initial setup packet exceeds size limit".into(),
        ));
    }

    let remaining = total_setup_len - 12;
    if remaining > 0 {
        buf.resize(total_setup_len, 0);
        reader
            .read_exact(&mut buf[12..total_setup_len])
            .await
            .map_err(SshError::Io)?;
    }

    let client_proto = &buf[12..12 + proto_len];
    let data_start = 12 + padded_proto_len;
    let client_data = &buf[data_start..data_start + data_len];

    if client_proto != X11_AUTH_PROTOCOL.as_bytes()
        || client_data.len() != fake_cookie.len()
        || !bool::from(client_data.ct_eq(fake_cookie))
    {
        return Err(SshError::Protocol("invalid X11 authentication cookie".into()));
    }

    let real_proto_bytes = real_proto.as_bytes();
    let real_proto_len_u16 = u16::try_from(real_proto_bytes.len()).map_err(|_| {
        SshError::Protocol("real X11 auth protocol name too long".into())
    })?;
    let real_cookie_len_u16 = u16::try_from(real_cookie.len()).map_err(|_| {
        SshError::Protocol("real X11 auth cookie too long".into())
    })?;

    let mut rewritten =
        Vec::with_capacity(12 + pad4(real_proto_bytes.len()) + pad4(real_cookie.len()));
    rewritten.extend_from_slice(&header[0..6]);
    if big_endian {
        rewritten.extend_from_slice(&real_proto_len_u16.to_be_bytes());
        rewritten.extend_from_slice(&real_cookie_len_u16.to_be_bytes());
    } else {
        rewritten.extend_from_slice(&real_proto_len_u16.to_le_bytes());
        rewritten.extend_from_slice(&real_cookie_len_u16.to_le_bytes());
    }
    rewritten.extend_from_slice(&[0u8, 0u8]);

    rewritten.extend_from_slice(real_proto_bytes);
    rewritten.resize(12 + pad4(real_proto_bytes.len()), 0);
    rewritten.extend_from_slice(real_cookie);
    rewritten.resize(
        12 + pad4(real_proto_bytes.len()) + pad4(real_cookie.len()),
        0,
    );

    Ok((rewritten, Vec::new()))
}

#[inline]
fn pad4(len: usize) -> usize {
    (len + 3) & !3
}

pub fn tcp_target_socket_addr(target: &X11Target) -> Option<SocketAddr> {
    match target {
        X11Target::Tcp { host, port } => {
            let ip = host.parse::<std::net::IpAddr>().ok()?;
            Some(SocketAddr::new(ip, *port))
        }
        X11Target::Unix(_) => None,
    }
}

pub fn is_valid_x11_path(path: &Path) -> bool {
    !path.as_os_str().is_empty()
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt, duplex};
    use tokio::net::TcpListener;

    fn build_x11_setup(byte_order: u8, proto: &[u8], data: &[u8], trailing: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(byte_order);
        out.push(0);
        if byte_order == b'B' {
            out.extend_from_slice(&11u16.to_be_bytes());
            out.extend_from_slice(&0u16.to_be_bytes());
            out.extend_from_slice(&(proto.len() as u16).to_be_bytes());
            out.extend_from_slice(&(data.len() as u16).to_be_bytes());
        } else {
            out.extend_from_slice(&11u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&(proto.len() as u16).to_le_bytes());
            out.extend_from_slice(&(data.len() as u16).to_le_bytes());
        }
        out.extend_from_slice(&[0u8, 0u8]);
        out.extend_from_slice(proto);
        out.resize(12 + pad4(proto.len()), 0);
        out.extend_from_slice(data);
        out.resize(12 + pad4(proto.len()) + pad4(data.len()), 0);
        out.extend_from_slice(trailing);
        out
    }

    fn build_xauth_record(
        family: u16,
        addr: &[u8],
        number: &[u8],
        name: &[u8],
        data: &[u8],
    ) -> Vec<u8> {
        let mut out = Vec::new();
        out.extend_from_slice(&family.to_be_bytes());
        for field in [addr, number, name, data] {
            out.extend_from_slice(&(field.len() as u16).to_be_bytes());
            out.extend_from_slice(field);
        }
        out
    }

    #[test]
    fn parse_x11_display_enforces_loopback_tcp() {
        let tcp = parse_x11_display("localhost:2.1").expect("localhost display");
        assert_eq!(
            tcp.target,
            X11Target::Tcp {
                host: "127.0.0.1".into(),
                port: 6002
            }
        );
        assert_eq!(tcp.display_num, 2);
        assert_eq!(tcp.screen_num, 1);

        let ipv6 = parse_x11_display("[::1]:1").expect("ipv6 display");
        assert_eq!(
            ipv6.target,
            X11Target::Tcp {
                host: "::1".into(),
                port: 6001
            }
        );

        assert!(parse_x11_display("192.0.2.10:0").is_err());
        assert!(parse_x11_display("example.com:0").is_err());
    }

    #[test]
    fn parse_xauthority_finds_matching_cookie() {
        let mut raw =
            build_xauth_record(0, &[127, 0, 0, 1], b"1", b"MIT-MAGIC-COOKIE-1", &[1; 16]);
        raw.extend_from_slice(&build_xauth_record(
            0,
            &[127, 0, 0, 1],
            b"0",
            b"MIT-MAGIC-COOKIE-1",
            &[0xab; 16],
        ));
        let entries = parse_xauthority_bytes(&raw).expect("parse xauth");
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[1].data, vec![0xab; 16]);
    }

    #[tokio::test]
    async fn verify_x_server_checks_x11_greeting() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            for accept_status in [1u8, 0u8] {
                let (mut sock, _) = listener.accept().await.unwrap();
                let mut buf = [0u8; 64];
                let _ = sock.read(&mut buf).await.unwrap();
                let mut reply = [0u8; 8];
                reply[0] = accept_status;
                reply[2..4].copy_from_slice(&11u16.to_le_bytes());
                reply[4..6].copy_from_slice(&0u16.to_le_bytes());
                sock.write_all(&reply).await.unwrap();
            }
        });

        let target = X11Target::Tcp {
            host: "127.0.0.1".into(),
            port,
        };
        verify_x_server(&target, "MIT-MAGIC-COOKIE-1", &[0x55; 16])
            .await
            .expect("accepted");
        let err = verify_x_server(&target, "MIT-MAGIC-COOKIE-1", &[0x55; 16])
            .await
            .unwrap_err();
        assert!(err.to_string().contains("rejected"));
    }

    #[tokio::test]
    async fn x11_cookie_rewrite_accepts_fake_and_rejects_bad_cookie() {
        let fake = [0x11u8; 16];
        let real = [0x22u8; 16];

        let setup = build_x11_setup(b'l', b"MIT-MAGIC-COOKIE-1", &fake, b"HELLO");
        let (mut client, mut server) = duplex(1024);
        tokio::spawn(async move {
            client.write_all(&setup).await.unwrap();
        });

        let (rewritten, _) =
            read_and_rewrite_setup_packet(&mut server, &fake, "MIT-MAGIC-COOKIE-1", &real)
                .await
                .expect("valid cookie");

        let expected = build_x11_setup(b'l', b"MIT-MAGIC-COOKIE-1", &real, b"");
        assert_eq!(rewritten, expected);

        let mut tail = [0u8; 5];
        server.read_exact(&mut tail).await.expect("trailing bytes");
        assert_eq!(&tail, b"HELLO");

        let wrong_cookie = [0x99u8; 16];
        let bad_setup = build_x11_setup(b'B', b"MIT-MAGIC-COOKIE-1", &wrong_cookie, b"");
        let (mut client2, mut server2) = duplex(1024);
        tokio::spawn(async move {
            client2.write_all(&bad_setup).await.unwrap();
        });
        let err =
            read_and_rewrite_setup_packet(&mut server2, &fake, "MIT-MAGIC-COOKIE-1", &real)
                .await
                .unwrap_err();
        assert!(err.to_string().contains("invalid X11 authentication cookie"));
    }

    #[tokio::test]
    async fn x11_forwarder_strips_auth_when_real_cookie_empty() {
        let fake = [0x33u8; 16];
        let setup = build_x11_setup(b'B', b"MIT-MAGIC-COOKIE-1", &fake, b"");
        let (mut client, mut server) = duplex(1024);
        tokio::spawn(async move {
            client.write_all(&setup).await.unwrap();
        });

        let (rewritten, _) = read_and_rewrite_setup_packet(&mut server, &fake, "", &[])
            .await
            .expect("empty real auth");
        assert_eq!(rewritten, build_x11_setup(b'B', b"", b"", b""));
    }
}
