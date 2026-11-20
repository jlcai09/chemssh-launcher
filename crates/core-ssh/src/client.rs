use std::sync::{Arc, Mutex};
use std::time::Duration;

use core_config::{Profile, AUTH_PASSWORD, AUTH_PRIVATE_KEY};
use russh::client::{self, AuthResult, ChannelOpenHandle, Config, Handle};
use russh::keys::{load_secret_key, PrivateKeyWithHashAlg, PublicKey, PublicKeyOrCertificate};
use russh::ChannelMsg;
use russh_sftp::client::SftpSession;
use tokio::io::copy_bidirectional;
use tokio::net::TcpStream;
use tokio::sync::mpsc;

use crate::hostkey::KnownHostsVerifier;
use crate::shell::{PtyShellSession, TerminalEvent};
use crate::tunnel::{LocalTunnel, ReverseProxyChannel};
use crate::SshError;

#[derive(Default, Clone)]
pub struct SshCredentials {
    pub password: Option<String>,
    pub private_key_passphrase: Option<String>,
}

pub struct SshClientHandler {
    verifier: KnownHostsVerifier,
    last_hostkey_err: Arc<Mutex<Option<SshError>>>,
    reverse_proxy_target: Arc<Mutex<Option<String>>>,
    x11_local_target: Arc<Mutex<Option<String>>>,
}

impl SshClientHandler {
    fn new(
        verifier: KnownHostsVerifier,
        last_hostkey_err: Arc<Mutex<Option<SshError>>>,
        reverse_proxy_target: Arc<Mutex<Option<String>>>,
        x11_local_target: Arc<Mutex<Option<String>>>,
    ) -> Self {
        Self {
            verifier,
            last_hostkey_err,
            reverse_proxy_target,
            x11_local_target,
        }
    }
}

impl client::Handler for SshClientHandler {
    type Error = russh::Error;

    async fn check_server_key(
        &mut self,
        server_public_key: &PublicKeyOrCertificate,
    ) -> Result<bool, Self::Error> {
        let key: PublicKey = match server_public_key {
            PublicKeyOrCertificate::PublicKey { key, .. } => key.clone(),
            PublicKeyOrCertificate::Certificate(cert) => {
                PublicKey::new(cert.public_key().clone(), "")
            }
        };
        match self.verifier.verify(&key) {
            Ok(()) => Ok(true),
            Err(err) => {
                if let Ok(mut guard) = self.last_hostkey_err.lock() {
                    *guard = Some(err);
                }
                Ok(false)
            }
        }
    }

    async fn server_channel_open_forwarded_tcpip(
        &mut self,
        channel: russh::Channel<client::Msg>,
        _connected_address: &str,
        _connected_port: u32,
        _originator_address: &str,
        _originator_port: u32,
        reply: ChannelOpenHandle,
        _session: &mut client::Session,
    ) -> Result<(), Self::Error> {
        let target = self
            .reverse_proxy_target
            .lock()
            .ok()
            .and_then(|g| g.clone());
        let Some(target_addr) = target else {
            return Ok(());
        };
        reply.accept().await;
        tokio::spawn(async move {
            let Ok(mut local_stream) = TcpStream::connect(&target_addr).await else {
                return;
            };
            let mut ssh_stream = channel.into_stream();
            let _ = copy_bidirectional(&mut ssh_stream, &mut local_stream).await;
        });
        Ok(())
    }

    async fn server_channel_open_x11(
        &mut self,
        channel: russh::Channel<client::Msg>,
        _originator_address: &str,
        _originator_port: u32,
        reply: ChannelOpenHandle,
        _session: &mut client::Session,
    ) -> Result<(), Self::Error> {
        let target = self.x11_local_target.lock().ok().and_then(|g| g.clone());
        let Some(target_addr) = target else {
            return Ok(());
        };
        reply.accept().await;
        tokio::spawn(async move {
            let Ok(mut local_stream) = TcpStream::connect(&target_addr).await else {
                return;
            };
            let mut ssh_stream = channel.into_stream();
            let _ = copy_bidirectional(&mut ssh_stream, &mut local_stream).await;
        });
        Ok(())
    }
}

pub struct ExecOutput {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: Option<u32>,
}

pub struct SshConnection {
    handle: Arc<Handle<SshClientHandler>>,
    reverse_proxy_target: Arc<Mutex<Option<String>>>,
    x11_local_target: Arc<Mutex<Option<String>>>,
}

impl SshConnection {
    pub async fn connect(
        profile: &Profile,
        credentials: &SshCredentials,
        verifier: KnownHostsVerifier,
    ) -> Result<Self, SshError> {
        let mut config = Config::default();
        config.inactivity_timeout = Some(Duration::from_secs(300));
        config.keepalive_interval = Some(Duration::from_secs(20));
        config.keepalive_max = 3;

        let last_hostkey_err = Arc::new(Mutex::new(None));
        let reverse_proxy_target = Arc::new(Mutex::new(None));
        let x11_local_target = Arc::new(Mutex::new(None));

        let handler = SshClientHandler::new(
            verifier,
            Arc::clone(&last_hostkey_err),
            Arc::clone(&reverse_proxy_target),
            Arc::clone(&x11_local_target),
        );

        let addr = (profile.ssh_host.as_str(), profile.ssh_port);
        let mut handle = match client::connect(Arc::new(config), addr, handler).await {
            Ok(h) => h,
            Err(err) => {
                if let Ok(mut guard) = last_hostkey_err.lock() {
                    if let Some(hk_err) = guard.take() {
                        return Err(hk_err);
                    }
                }
                return Err(SshError::Connect(err.to_string()));
            }
        };

        let auth_result = match profile.auth_method.as_str() {
            AUTH_PASSWORD => {
                let password = credentials
                    .password
                    .as_deref()
                    .ok_or_else(|| SshError::Auth("password is required".into()))?;
                handle
                    .authenticate_password(&profile.ssh_user, password)
                    .await
                    .map_err(|e| SshError::Auth(format!("password authentication failed: {e}")))?
            }
            AUTH_PRIVATE_KEY => {
                let key_path = profile.private_key_path.trim();
                if key_path.is_empty() {
                    return Err(SshError::Auth("private key path is required".into()));
                }
                let secret_key = load_secret_key(
                    key_path,
                    credentials.private_key_passphrase.as_deref(),
                )
                .map_err(|e| SshError::Auth(format!("failed to load private key: {e}")))?;
                let hash_alg = handle
                    .best_supported_rsa_hash()
                    .await
                    .ok()
                    .flatten()
                    .flatten();
                let key_with_alg = PrivateKeyWithHashAlg::new(Arc::new(secret_key), hash_alg);
                handle
                    .authenticate_publickey(&profile.ssh_user, key_with_alg)
                    .await
                    .map_err(|e| SshError::Auth(format!("public key authentication failed: {e}")))?
            }
            other => {
                return Err(SshError::Auth(format!(
                    "unsupported auth method: {other}"
                )));
            }
        };

        if !matches!(auth_result, AuthResult::Success) {
            return Err(SshError::Auth("authentication rejected by server".into()));
        }

        Ok(Self {
            handle: Arc::new(handle),
            reverse_proxy_target,
            x11_local_target,
        })
    }

    pub fn set_reverse_proxy_target(&self, addr: Option<String>) {
        if let Ok(mut guard) = self.reverse_proxy_target.lock() {
            *guard = addr;
        }
    }

    pub fn set_x11_local_target(&self, addr: Option<String>) {
        if let Ok(mut guard) = self.x11_local_target.lock() {
            *guard = addr;
        }
    }

    pub async fn exec_command(&self, command: &str) -> Result<ExecOutput, SshError> {
        let mut channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Exec(format!("open session channel: {e}")))?;
        channel
            .exec(true, command)
            .await
            .map_err(|e| SshError::Exec(format!("exec command: {e}")))?;

        let mut stdout = Vec::new();
        let mut stderr = Vec::new();
        let mut exit_code = None;

        while let Some(msg) = channel.wait().await {
            match msg {
                ChannelMsg::Data { data } => stdout.extend_from_slice(&data),
                ChannelMsg::ExtendedData { data, ext } => {
                    if ext == 1 {
                        stderr.extend_from_slice(&data);
                    }
                }
                ChannelMsg::ExitStatus { exit_status } => exit_code = Some(exit_status),
                ChannelMsg::Eof | ChannelMsg::Close => break,
                _ => {}
            }
        }

        Ok(ExecOutput {
            stdout: String::from_utf8_lossy(&stdout).into_owned(),
            stderr: String::from_utf8_lossy(&stderr).into_owned(),
            exit_code,
        })
    }

    pub async fn exec_streaming(
        &self,
        command: &str,
        output_tx: mpsc::Sender<String>,
    ) -> Result<Option<u32>, SshError> {
        let mut channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Exec(format!("open session channel: {e}")))?;
        channel
            .exec(true, command)
            .await
            .map_err(|e| SshError::Exec(format!("exec command: {e}")))?;

        let mut exit_code = None;
        while let Some(msg) = channel.wait().await {
            match msg {
                ChannelMsg::Data { data } | ChannelMsg::ExtendedData { data, .. } => {
                    let text = String::from_utf8_lossy(&data).into_owned();
                    let _ = output_tx.send(text).await;
                }
                ChannelMsg::ExitStatus { exit_status } => exit_code = Some(exit_status),
                ChannelMsg::Eof | ChannelMsg::Close => break,
                _ => {}
            }
        }
        Ok(exit_code)
    }

    pub async fn open_sftp(&self) -> Result<SftpSession, SshError> {
        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Sftp(format!("open session channel: {e}")))?;
        channel
            .request_subsystem(true, "sftp")
            .await
            .map_err(|e| SshError::Sftp(format!("request sftp subsystem: {e}")))?;
        SftpSession::new(channel.into_stream())
            .await
            .map_err(|e| SshError::Sftp(format!("init sftp session: {e}")))
    }

    pub async fn start_local_tunnel(
        &self,
        local_host: &str,
        local_port: u16,
        remote_host: String,
        remote_port: u16,
    ) -> Result<LocalTunnel, SshError> {
        LocalTunnel::start(
            Arc::clone(&self.handle),
            local_host,
            local_port,
            remote_host,
            remote_port,
        )
        .await
    }

    pub async fn start_reverse_proxy(
        &self,
        local_proxy_addr: String,
    ) -> Result<ReverseProxyChannel, SshError> {
        self.set_reverse_proxy_target(Some(local_proxy_addr));
        match ReverseProxyChannel::start(Arc::clone(&self.handle)).await {
            Ok(channel) => Ok(channel),
            Err(err) => {
                self.set_reverse_proxy_target(None);
                Err(err)
            }
        }
    }

    pub async fn open_pty_shell(
        &self,
        cols: u32,
        rows: u32,
        term: &str,
        event_tx: mpsc::Sender<TerminalEvent>,
    ) -> Result<PtyShellSession, SshError> {
        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Shell(format!("open shell channel: {e}")))?;
        PtyShellSession::open(channel, cols, rows, term, event_tx).await
    }

    pub async fn disconnect(&self) {
        let _ = self
            .handle
            .disconnect(russh::Disconnect::ByApplication, "closed", "en")
            .await;
    }
}
