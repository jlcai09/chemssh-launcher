use std::collections::HashMap;
use std::time::{Duration, Instant};

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use url::Url;

use crate::SessionError;

#[derive(Debug, Clone, Default)]
pub struct HealthOptions {
    pub reject_statuses: HashMap<u16, String>,
}

pub async fn wait_for_url(
    target_url: &str,
    timeout: Duration,
    interval: Duration,
) -> Result<(), SessionError> {
    wait_for_url_with_options(target_url, timeout, interval, &HealthOptions::default()).await
}

pub async fn wait_for_url_with_options(
    target_url: &str,
    timeout: Duration,
    interval: Duration,
    options: &HealthOptions,
) -> Result<(), SessionError> {
    let parsed = Url::parse(target_url)
        .map_err(|e| SessionError::HealthCheck(format!("invalid health URL {target_url}: {e}")))?;
    if parsed.scheme() != "http" {
        return Err(SessionError::HealthCheck(format!(
            "unsupported health URL scheme {:?}",
            parsed.scheme()
        )));
    }
    let host = parsed
        .host_str()
        .ok_or_else(|| SessionError::HealthCheck(format!("missing host in {target_url}")))?
        .to_string();
    let port = parsed.port_or_known_default().unwrap_or(80);
    let path_and_query = match parsed.query() {
        Some(q) => format!("{}?{q}", parsed.path()),
        None => parsed.path().to_string(),
    };
    let req_path = if path_and_query.is_empty() {
        "/".to_string()
    } else {
        path_and_query
    };

    let deadline = Instant::now() + timeout;
    let mut last_err;

    loop {
        let now = Instant::now();
        let remaining = deadline.saturating_duration_since(now);
        let attempt_timeout = remaining.min(Duration::from_secs(3));

        match probe_http_status(&host, port, &req_path, attempt_timeout).await {
            Ok(status) => {
                if let Some(msg) = options.reject_statuses.get(&status) {
                    let reason = if msg.is_empty() {
                        format!("HTTP {status}")
                    } else {
                        msg.clone()
                    };
                    return Err(SessionError::HealthCheck(format!(
                        "health check failed for {target_url}: unexpected HTTP status {status}: {reason}"
                    )));
                }
                if (200..=499).contains(&status) {
                    return Ok(());
                }
                last_err = Some(format!("unexpected HTTP status {status}"));
            }
            Err(err) => {
                last_err = Some(err);
            }
        }

        if Instant::now() >= deadline {
            let detail = last_err.unwrap_or_else(|| "timed out".to_string());
            return Err(SessionError::HealthCheck(format!(
                "health check failed for {target_url}: {detail}"
            )));
        }

        let sleep_for = interval.min(deadline.saturating_duration_since(Instant::now()));
        if !sleep_for.is_zero() {
            tokio::time::sleep(sleep_for).await;
        }
    }
}

async fn probe_http_status(
    host: &str,
    port: u16,
    path: &str,
    timeout: Duration,
) -> Result<u16, String> {
    let connect_addr = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    };

    let fut = async {
        let mut stream = TcpStream::connect(&connect_addr)
            .await
            .map_err(|e| e.to_string())?;
        let request = format!(
            "GET {path} HTTP/1.1\r\nHost: {connect_addr}\r\nConnection: close\r\nUser-Agent: chemssh-launcher/0.4.0\r\n\r\n"
        );
        stream
            .write_all(request.as_bytes())
            .await
            .map_err(|e| e.to_string())?;
        let mut reader = BufReader::new(stream);
        let mut status_line = String::new();
        reader
            .read_line(&mut status_line)
            .await
            .map_err(|e| e.to_string())?;
        parse_status_code(&status_line)
    };

    tokio::time::timeout(timeout, fut)
        .await
        .map_err(|_| "request timed out".to_string())?
}

fn parse_status_code(status_line: &str) -> Result<u16, String> {
    let mut parts = status_line.split_whitespace();
    let version = parts
        .next()
        .ok_or_else(|| "empty HTTP status line".to_string())?;
    if !version.starts_with("HTTP/") {
        return Err(format!("malformed HTTP status line: {status_line:?}"));
    }
    let code_str = parts
        .next()
        .ok_or_else(|| "missing HTTP status code".to_string())?;
    code_str
        .parse::<u16>()
        .map_err(|_| format!("invalid HTTP status code: {code_str:?}"))
}
