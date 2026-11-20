pub mod health;
pub mod port;

use thiserror::Error;

pub use health::{wait_for_url, wait_for_url_with_options, HealthOptions};
pub use port::{check_port_available, is_port_available, suggest_free_ports};

#[derive(Debug, Error)]
pub enum SessionError {
    #[error("local port occupied: {host}:{port} is already in use")]
    PortOccupied { host: String, port: u16 },
    #[error("{0}")]
    HealthCheck(String),
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::net::TcpListener;
    use std::time::Duration;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    #[test]
    fn port_availability_and_suggestions() {
        let occupied = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = occupied.local_addr().unwrap().port();

        assert!(!is_port_available("127.0.0.1", port));
        assert!(check_port_available("127.0.0.1", port).is_err());

        let suggestions = suggest_free_ports("127.0.0.1", port, 3);
        assert_eq!(suggestions.len(), 3);
        for suggested in suggestions {
            assert!(suggested > port);
            assert!(is_port_available("127.0.0.1", suggested));
        }
    }

    async fn spawn_mock_http(status_line: &'static str) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            loop {
                let Ok((mut socket, _)) = listener.accept().await else {
                    break;
                };
                tokio::spawn(async move {
                    let mut buf = [0u8; 512];
                    let _ = socket.read(&mut buf).await;
                    let resp = format!("{status_line}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
                    let _ = socket.write_all(resp.as_bytes()).await;
                });
            }
        });
        format!("http://127.0.0.1:{port}/")
    }

    #[tokio::test]
    async fn health_check_accepts_2xx_and_4xx() {
        let url_ok = spawn_mock_http("HTTP/1.1 200 OK").await;
        wait_for_url(&url_ok, Duration::from_secs(2), Duration::from_millis(20))
            .await
            .unwrap();

        let url_401 = spawn_mock_http("HTTP/1.1 401 Unauthorized").await;
        wait_for_url(&url_401, Duration::from_secs(2), Duration::from_millis(20))
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn health_check_rejects_5xx_and_explicit_rejected_status() {
        let url_502 = spawn_mock_http("HTTP/1.1 502 Bad Gateway").await;
        assert!(
            wait_for_url(&url_502, Duration::from_millis(120), Duration::from_millis(20))
                .await
                .is_err()
        );

        let url_403 = spawn_mock_http("HTTP/1.1 403 Forbidden").await;
        let mut reject = HashMap::new();
        reject.insert(403, "blocked by token".to_string());
        let err = wait_for_url_with_options(
            &url_403,
            Duration::from_secs(1),
            Duration::from_millis(20),
            &HealthOptions {
                reject_statuses: reject,
            },
        )
        .await
        .unwrap_err();
        assert!(err.to_string().contains("blocked by token"));
    }
}
