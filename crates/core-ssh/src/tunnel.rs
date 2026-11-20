use std::net::SocketAddr;
use std::sync::Arc;

use tokio::io::copy_bidirectional;
use tokio::net::TcpListener;
use tokio::sync::watch;

use crate::client::SshClientHandler;
use crate::SshError;

pub struct LocalTunnel {
    pub local_addr: SocketAddr,
    stop_tx: watch::Sender<bool>,
}

impl LocalTunnel {
    pub async fn start(
        handle: Arc<russh::client::Handle<SshClientHandler>>,
        local_host: &str,
        local_port: u16,
        remote_host: String,
        remote_port: u16,
    ) -> Result<Self, SshError> {
        let bind_addr = if local_host.contains(':') && !local_host.starts_with('[') {
            format!("[{local_host}]:{local_port}")
        } else {
            format!("{local_host}:{local_port}")
        };
        let listener = TcpListener::bind(&bind_addr)
            .await
            .map_err(|e| SshError::Tunnel(format!("listen on {bind_addr}: {e}")))?;
        let local_addr = listener
            .local_addr()
            .map_err(|e| SshError::Tunnel(format!("resolve local tunnel addr: {e}")))?;

        let (stop_tx, mut stop_rx) = watch::channel(false);

        tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = stop_rx.changed() => {
                        if *stop_rx.borrow() {
                            break;
                        }
                    }
                    accepted = listener.accept() => {
                        let Ok((mut tcp_stream, peer_addr)) = accepted else {
                            break;
                        };
                        let handle_clone = Arc::clone(&handle);
                        let target_host = remote_host.clone();
                        tokio::spawn(async move {
                            let origin_ip = peer_addr.ip().to_string();
                            let origin_port = peer_addr.port() as u32;
                            let Ok(channel) = handle_clone
                                .channel_open_direct_tcpip(
                                    target_host,
                                    remote_port as u32,
                                    origin_ip,
                                    origin_port,
                                )
                                .await
                            else {
                                return;
                            };
                            let mut ssh_stream = channel.into_stream();
                            let _ = copy_bidirectional(&mut tcp_stream, &mut ssh_stream).await;
                        });
                    }
                }
            }
        });

        Ok(Self {
            local_addr,
            stop_tx,
        })
    }

    pub fn stop(&self) {
        let _ = self.stop_tx.send(true);
    }
}

impl Drop for LocalTunnel {
    fn drop(&mut self) {
        self.stop();
    }
}

pub struct ReverseProxyChannel {
    pub assigned_port: u16,
    pub proxy_url: String,
    handle: Arc<russh::client::Handle<SshClientHandler>>,
    closed: bool,
}

impl ReverseProxyChannel {
    /// Always binds remote loopback `127.0.0.1:0` and lets the SSH server assign the port.
    pub async fn start(
        handle: Arc<russh::client::Handle<SshClientHandler>>,
    ) -> Result<Self, SshError> {
        let assigned_port = handle
            .tcpip_forward("127.0.0.1", 0)
            .await
            .map_err(|e| SshError::ReverseProxy(format!("request remote loopback forward: {e}")))?;
        if assigned_port == 0 || assigned_port > u16::MAX as u32 {
            return Err(SshError::ReverseProxy(format!(
                "invalid server-assigned proxy port: {assigned_port}"
            )));
        }
        let port = assigned_port as u16;
        Ok(Self {
            assigned_port: port,
            proxy_url: format!("http://127.0.0.1:{port}"),
            handle,
            closed: false,
        })
    }

    pub async fn close(&mut self) {
        if self.closed {
            return;
        }
        self.closed = true;
        let _ = self
            .handle
            .cancel_tcpip_forward("127.0.0.1", self.assigned_port as u32)
            .await;
    }
}
