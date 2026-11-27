use std::net::{IpAddr, SocketAddr};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use core_config::{EnvPair, Profile, AUTH_PASSWORD, AUTH_PRIVATE_KEY};
use russh::client::{self, AuthResult, ChannelOpenHandle, Config, Handle, KeyboardInteractiveAuthResponse};
use russh::keys::{
    load_secret_key, HashAlg, PrivateKeyWithHashAlg, PublicKey, PublicKeyOrCertificate,
};
use russh::{ChannelMsg, ChannelOpenFailure, MethodKind, Sig};
use russh_sftp::client::{RawSftpSession, SftpSession};
use serde::{Deserialize, Serialize};
use tokio::io::{copy_bidirectional, AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, oneshot};

use crate::hostkey::KnownHostsVerifier;
use crate::remote_command::{
    assemble_check_port_command, assemble_kill_pid_command, assemble_remote_command,
};
use crate::shell::{
    prepare_pty_channel, PreparedPtyChannel, PtyShellSession, ShellOpenOptions, TerminalEvent,
};
use crate::shell_integration::{
    build_query_cwd_command, build_shell_integration_setup_script,
    cleanup_shell_integration_command, parse_query_cwd_output, parse_shell_integration_output,
    proxy_login_shell_command, ShellIntegration,
};
use crate::tunnel::{LocalTunnel, ReverseProxyChannel};
use crate::x11::{PreparedX11, X11ForwarderState};
use crate::SshError;

const CHECK_PORT_TIMEOUT: Duration = Duration::from_secs(15);
const IDENTITY_FETCH_TIMEOUT: Duration = Duration::from_secs(5);
const SHELL_SETUP_TIMEOUT: Duration = Duration::from_secs(8);
const SHELL_CWD_TIMEOUT: Duration = Duration::from_secs(2);
const REMOTE_KILL_TIMEOUT: Duration = Duration::from_secs(10);
const IDENTITY_RESPONSE_MAX_BYTES: usize = 64 * 1024;
const TCP_CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
const HAPPY_EYEBALLS_FALLBACK_DELAY: Duration = Duration::from_millis(250);
const SSH_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(20);
const RSA_HASH_EXT_INFO_TIMEOUT: Duration = Duration::from_millis(250);
const MAX_KEYBOARD_INTERACTIVE_ROUNDS: usize = 8;

#[derive(Default, Clone)]
pub struct SshCredentials {
    pub password: Option<String>,
    pub private_key_passphrase: Option<String>,
}

pub struct SshClientHandler {
    verifier: KnownHostsVerifier,
    last_hostkey_err: Arc<Mutex<Option<SshError>>>,
    reverse_proxy_target: Arc<Mutex<Option<String>>>,
    x11_forwarder: Arc<Mutex<Option<X11ForwarderState>>>,
}

impl SshClientHandler {
    fn new(
        verifier: KnownHostsVerifier,
        last_hostkey_err: Arc<Mutex<Option<SshError>>>,
        reverse_proxy_target: Arc<Mutex<Option<String>>>,
        x11_forwarder: Arc<Mutex<Option<X11ForwarderState>>>,
    ) -> Self {
        Self {
            verifier,
            last_hostkey_err,
            reverse_proxy_target,
            x11_forwarder,
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
            reply
                .reject(ChannelOpenFailure::AdministrativelyProhibited)
                .await;
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
        let forwarder = self.x11_forwarder.lock().ok().and_then(|g| g.clone());
        let Some(forwarder) = forwarder else {
            reply
                .reject(ChannelOpenFailure::AdministrativelyProhibited)
                .await;
            return Ok(());
        };
        let Some(permit) = forwarder.try_acquire_permit() else {
            reply
                .reject(ChannelOpenFailure::AdministrativelyProhibited)
                .await;
            return Ok(());
        };

        reply.accept().await;
        tokio::spawn(async move {
            let _permit = permit;
            let ssh_stream = channel.into_stream();
            let _ = forwarder.forward_stream(ssh_stream).await;
        });
        Ok(())
    }
}

#[derive(Debug, Clone)]
pub struct ExecOutput {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: Option<u32>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CheckPortResult {
    pub reusable: bool,
    pub output: String,
    pub exit_code: Option<u32>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct ChemSshIdentity {
    #[serde(default)]
    pub app: String,
    #[serde(default)]
    pub service: String,
    #[serde(default)]
    pub project_version: String,
    #[serde(default)]
    pub pid: u32,
    #[serde(default)]
    pub scheduler: String,
    #[serde(default)]
    pub workspace_root: String,
    #[serde(default)]
    pub host: String,
    #[serde(default)]
    pub port: u16,
}

enum RemoteProcessControl {
    Signal(Sig),
    Stop,
}

#[derive(Clone)]
pub struct RemoteProcessHandle {
    control_tx: mpsc::UnboundedSender<RemoteProcessControl>,
}

impl RemoteProcessHandle {
    pub fn signal(&self, sig: Sig) -> Result<(), SshError> {
        self.control_tx
            .send(RemoteProcessControl::Signal(sig))
            .map_err(|_| SshError::Exec("remote process channel closed".into()))
    }

    pub fn stop(&self) {
        let _ = self.control_tx.send(RemoteProcessControl::Stop);
    }
}

pub struct OpenedPtyShell {
    pub shell: PtyShellSession,
    pub events: mpsc::UnboundedReceiver<TerminalEvent>,
    pub integration: Option<ShellIntegration>,
    pub x11_warning: Option<String>,
}

#[derive(Clone)]
pub struct CachedRawSftp {
    pub session: Arc<RawSftpSession>,
    pub has_posix_rename: bool,
    verified_parents: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
}

impl CachedRawSftp {
    pub fn is_parent_verified(&self, parent: &str) -> bool {
        self.verified_parents
            .lock()
            .ok()
            .is_some_and(|set| set.contains(parent))
    }

    pub fn mark_parent_verified(&self, parent: &str) {
        if let Ok(mut set) = self.verified_parents.lock() {
            if set.len() >= 128 {
                set.clear();
            }
            set.insert(parent.to_owned());
        }
    }
}

pub struct SshConnection {
    handle: Arc<Handle<SshClientHandler>>,
    reverse_proxy_target: Arc<Mutex<Option<String>>>,
    x11_forwarder: Arc<Mutex<Option<X11ForwarderState>>>,
    raw_sftp: Arc<tokio::sync::Mutex<Option<CachedRawSftp>>>,
}

impl SshConnection {
    pub async fn connect(
        profile: &Profile,
        credentials: &SshCredentials,
        verifier: KnownHostsVerifier,
    ) -> Result<Self, SshError> {
        let mut config = Config::default();
        config.inactivity_timeout = Some(Duration::from_secs(300));
        config.keepalive_interval = Some(Duration::from_secs(10));
        config.keepalive_max = 3;
        config.nodelay = true;
        if let Some(preferred_keys) = verifier.preferred_key_algorithms() {
            config.preferred.key = std::borrow::Cow::Owned(preferred_keys);
        }

        let last_hostkey_err = Arc::new(Mutex::new(None));
        let reverse_proxy_target = Arc::new(Mutex::new(None));
        let x11_forwarder = Arc::new(Mutex::new(None));

        let handler = SshClientHandler::new(
            verifier,
            Arc::clone(&last_hostkey_err),
            Arc::clone(&reverse_proxy_target),
            Arc::clone(&x11_forwarder),
        );

        let stream = connect_tcp_happy_eyeballs(profile.ssh_host.trim(), profile.ssh_port).await?;
        let handshake_res = tokio::time::timeout(
            SSH_HANDSHAKE_TIMEOUT,
            client::connect_stream(Arc::new(config), stream, handler),
        )
        .await;

        let mut handle = match handshake_res {
            Ok(Ok(h)) => h,
            Ok(Err(err)) => {
                if let Ok(mut guard) = last_hostkey_err.lock() {
                    if let Some(hk_err) = guard.take() {
                        return Err(hk_err);
                    }
                }
                return Err(SshError::Connect(err.to_string()));
            }
            Err(_) => {
                if let Ok(mut guard) = last_hostkey_err.lock() {
                    if let Some(hk_err) = guard.take() {
                        return Err(hk_err);
                    }
                }
                return Err(SshError::Connect(format!(
                    "SSH handshake timed out after {}s",
                    SSH_HANDSHAKE_TIMEOUT.as_secs()
                )));
            }
        };

        let auth_result = match profile.auth_method.as_str() {
            AUTH_PASSWORD => {
                let password = credentials
                    .password
                    .as_deref()
                    .ok_or_else(|| SshError::Auth("password is required".into()))?;
                let first = handle
                    .authenticate_password(&profile.ssh_user, password)
                    .await
                    .map_err(|e| SshError::Auth(format!("password authentication failed: {e}")))?;
                match first {
                    AuthResult::Success => AuthResult::Success,
                    AuthResult::Failure {
                        ref remaining_methods,
                        ..
                    } if remaining_methods.is_empty()
                        || remaining_methods.contains(&MethodKind::KeyboardInteractive) =>
                    {
                        authenticate_keyboard_interactive_password(
                            &mut handle,
                            &profile.ssh_user,
                            password,
                        )
                        .await?
                    }
                    other => other,
                }
            }
            AUTH_PRIVATE_KEY => {
                let key_path = profile.private_key_path.trim();
                if key_path.is_empty() {
                    return Err(SshError::Auth("private key path is required".into()));
                }
                let resolved_path = expand_tilde_path(key_path);
                let effective_passphrase = if profile.has_private_key_passphrase {
                    credentials.private_key_passphrase.as_deref()
                } else {
                    credentials
                        .private_key_passphrase
                        .as_deref()
                        .filter(|s| !s.is_empty())
                };
                let secret_key = load_secret_key(&resolved_path, effective_passphrase)
                    .map_err(|e| SshError::Auth(format!("failed to load private key: {e}")))?;
                let key_arc = Arc::new(secret_key);
                if key_arc.algorithm().is_rsa() {
                    authenticate_rsa_publickey_with_fallback(
                        &mut handle,
                        &profile.ssh_user,
                        key_arc,
                    )
                    .await?
                } else {
                    handle
                        .authenticate_publickey(
                            &profile.ssh_user,
                            PrivateKeyWithHashAlg::new(key_arc, None),
                        )
                        .await
                        .map_err(|e| {
                            SshError::Auth(format!("public key authentication failed: {e}"))
                        })?
                }
            }
            other => {
                return Err(SshError::Auth(format!(
                    "unsupported auth method: {other}"
                )));
            }
        };

        if let AuthResult::Failure {
            remaining_methods,
            partial_success,
        } = auth_result
        {
            return Err(SshError::Auth(format_auth_rejection(
                &remaining_methods,
                partial_success,
            )));
        }

        Ok(Self {
            handle: Arc::new(handle),
            reverse_proxy_target,
            x11_forwarder,
            raw_sftp: Arc::new(tokio::sync::Mutex::new(None)),
        })
    }

    #[cfg(test)]
    pub(crate) fn from_raw_handle(handle: Handle<SshClientHandler>) -> Self {
        Self {
            handle: Arc::new(handle),
            reverse_proxy_target: Arc::new(Mutex::new(None)),
            x11_forwarder: Arc::new(Mutex::new(None)),
            raw_sftp: Arc::new(tokio::sync::Mutex::new(None)),
        }
    }

    pub fn set_reverse_proxy_target(&self, addr: Option<String>) {
        if let Ok(mut guard) = self.reverse_proxy_target.lock() {
            *guard = addr;
        }
    }

    pub fn set_x11_forwarder(&self, forwarder: Option<X11ForwarderState>) {
        if let Ok(mut guard) = self.x11_forwarder.lock() {
            *guard = forwarder;
        }
    }

    pub async fn exec_command(&self, command: &str) -> Result<ExecOutput, SshError> {
        self.exec_command_timeout(command, Duration::from_secs(30))
            .await
    }

    pub async fn exec_command_timeout(
        &self,
        command: &str,
        timeout_dur: Duration,
    ) -> Result<ExecOutput, SshError> {
        tokio::time::timeout(timeout_dur, async {
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
                    ChannelMsg::Failure => {
                        return Err(SshError::Exec("remote command execution rejected".into()));
                    }
                    ChannelMsg::Close => break,
                    _ => {}
                }
            }

            Ok(ExecOutput {
                stdout: String::from_utf8_lossy(&stdout).into_owned(),
                stderr: String::from_utf8_lossy(&stderr).into_owned(),
                exit_code,
            })
        })
        .await
        .map_err(|_| SshError::Timeout(timeout_dur))?
    }

    pub async fn exec_with_stdin(
        &self,
        command: &str,
        stdin_bytes: &[u8],
        timeout_dur: Duration,
    ) -> Result<ExecOutput, SshError> {
        tokio::time::timeout(timeout_dur, async {
            let channel = self
                .handle
                .channel_open_session()
                .await
                .map_err(|e| SshError::Exec(format!("open session channel: {e}")))?;
            let (mut read_half, write_half) = channel.split();
            write_half
                .exec(true, command.as_bytes().to_vec())
                .await
                .map_err(|e| SshError::Exec(format!("exec command: {e}")))?;

            if !stdin_bytes.is_empty() {
                write_half
                    .data_bytes(stdin_bytes.to_vec())
                    .await
                    .map_err(|e| SshError::Exec(format!("write command stdin: {e}")))?;
            }
            write_half
                .eof()
                .await
                .map_err(|e| SshError::Exec(format!("send command eof: {e}")))?;

            let mut stdout = Vec::new();
            let mut stderr = Vec::new();
            let mut exit_code = None;

            while let Some(msg) = read_half.wait().await {
                match msg {
                    ChannelMsg::Data { data } => stdout.extend_from_slice(&data),
                    ChannelMsg::ExtendedData { data, ext } => {
                        if ext == 1 {
                            stderr.extend_from_slice(&data);
                        }
                    }
                    ChannelMsg::ExitStatus { exit_status } => exit_code = Some(exit_status),
                    ChannelMsg::Failure => {
                        return Err(SshError::Exec("remote command execution rejected".into()));
                    }
                    ChannelMsg::Close => break,
                    _ => {}
                }
            }

            Ok(ExecOutput {
                stdout: String::from_utf8_lossy(&stdout).into_owned(),
                stderr: String::from_utf8_lossy(&stderr).into_owned(),
                exit_code,
            })
        })
        .await
        .map_err(|_| SshError::Timeout(timeout_dur))?
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
                ChannelMsg::Close => break,
                _ => {}
            }
        }
        Ok(exit_code)
    }

    /// Runs `command`, feeds `stdin_bytes` (then EOF) and streams stdout/stderr chunks to
    /// `output_tx` until the command exits. Used for long-running scripts such as the installer.
    pub async fn exec_streaming_stdin(
        &self,
        command: &str,
        stdin_bytes: &[u8],
        output_tx: mpsc::Sender<String>,
    ) -> Result<Option<u32>, SshError> {
        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Exec(format!("open session channel: {e}")))?;
        let (mut read_half, write_half) = channel.split();
        write_half
            .exec(true, command.as_bytes().to_vec())
            .await
            .map_err(|e| SshError::Exec(format!("exec command: {e}")))?;
        if !stdin_bytes.is_empty() {
            write_half
                .data_bytes(stdin_bytes.to_vec())
                .await
                .map_err(|e| SshError::Exec(format!("write command stdin: {e}")))?;
        }
        write_half
            .eof()
            .await
            .map_err(|e| SshError::Exec(format!("send command eof: {e}")))?;

        let mut exit_code = None;
        while let Some(msg) = read_half.wait().await {
            match msg {
                ChannelMsg::Data { data } | ChannelMsg::ExtendedData { data, .. } => {
                    let text = String::from_utf8_lossy(&data).into_owned();
                    let _ = output_tx.send(text).await;
                }
                ChannelMsg::ExitStatus { exit_status } => exit_code = Some(exit_status),
                ChannelMsg::Failure => {
                    return Err(SshError::Exec("remote command execution rejected".into()));
                }
                ChannelMsg::Close => break,
                _ => {}
            }
        }
        Ok(exit_code)
    }

    pub async fn start_remote_process(
        &self,
        command: &str,
    ) -> Result<
        (
            RemoteProcessHandle,
            mpsc::UnboundedReceiver<String>,
            oneshot::Receiver<Result<Option<u32>, String>>,
        ),
        SshError,
    > {
        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Exec(format!("open remote command channel: {e}")))?;
        let (mut read_half, write_half) = channel.split();
        write_half
            .exec(true, command.as_bytes().to_vec())
            .await
            .map_err(|e| SshError::Exec(format!("start remote command: {e}")))?;

        let (output_tx, output_rx) = mpsc::unbounded_channel();
        let (exit_tx, exit_rx) = oneshot::channel();
        let (control_tx, mut control_rx) = mpsc::unbounded_channel::<RemoteProcessControl>();

        tokio::spawn(async move {
            while let Some(ctrl) = control_rx.recv().await {
                match ctrl {
                    RemoteProcessControl::Signal(sig) => {
                        let _ = write_half.signal(sig).await;
                    }
                    RemoteProcessControl::Stop => {
                        let _ = write_half.signal(Sig::TERM).await;
                        let _ = write_half.eof().await;
                        let _ = write_half.close().await;
                        break;
                    }
                }
            }
        });

        tokio::spawn(async move {
            let mut exit_code = None;
            let mut exit_signal: Option<String> = None;

            while let Some(msg) = read_half.wait().await {
                match msg {
                    ChannelMsg::Data { data } | ChannelMsg::ExtendedData { data, .. } => {
                        let text = String::from_utf8_lossy(&data).into_owned();
                        if !text.is_empty() {
                            let _ = output_tx.send(text);
                        }
                    }
                    ChannelMsg::ExitStatus { exit_status } => exit_code = Some(exit_status),
                    ChannelMsg::ExitSignal {
                        signal_name,
                        error_message,
                        ..
                    } => {
                        exit_signal = Some(if error_message.trim().is_empty() {
                            format!("{signal_name:?}")
                        } else {
                            format!("{signal_name:?}: {error_message}")
                        });
                    }
                    ChannelMsg::Failure => {
                        let _ = exit_tx.send(Err("remote command execution rejected".into()));
                        return;
                    }
                    ChannelMsg::Close => break,
                    _ => {}
                }
            }

            let res = if let Some(sig) = exit_signal {
                Err(format!("terminated by signal {sig}"))
            } else {
                Ok(exit_code)
            };
            let _ = exit_tx.send(res);
        });

        Ok((RemoteProcessHandle { control_tx }, output_rx, exit_rx))
    }

    pub async fn run_check_port_command(
        &self,
        profile: &Profile,
        proxy_env: &[EnvPair],
    ) -> Result<CheckPortResult, SshError> {
        let cmd = assemble_check_port_command(profile, proxy_env);
        let output = self.exec_command_timeout(&cmd, CHECK_PORT_TIMEOUT).await?;
        let combined = format!("{}\n{}", output.stdout, output.stderr);
        let reusable = combined
            .to_ascii_lowercase()
            .contains("used by a reusable chemssh server");
        Ok(CheckPortResult {
            reusable,
            output: combined.trim().to_string(),
            exit_code: output.exit_code,
        })
    }

    pub async fn start_remote_chemssh(
        &self,
        profile: &Profile,
        proxy_env: &[EnvPair],
    ) -> Result<
        (
            RemoteProcessHandle,
            mpsc::UnboundedReceiver<String>,
            oneshot::Receiver<Result<Option<u32>, String>>,
        ),
        SshError,
    > {
        let cmd = assemble_remote_command(profile, proxy_env);
        self.start_remote_process(&cmd).await
    }

    pub async fn stop_remote_pid(&self, pid: u32) -> Result<ExecOutput, SshError> {
        let cmd = assemble_kill_pid_command(pid);
        self.exec_command_timeout(&cmd, REMOTE_KILL_TIMEOUT).await
    }

    pub async fn open_direct_tcpip(
        &self,
        host: &str,
        port: u16,
    ) -> Result<russh::Channel<client::Msg>, SshError> {
        self.handle
            .channel_open_direct_tcpip(host, port as u32, "127.0.0.1", 0)
            .await
            .map_err(|e| SshError::Tunnel(format!("open direct-tcpip to {host}:{port}: {e}")))
    }

    pub async fn fetch_remote_identity(
        &self,
        remote_host: &str,
        remote_port: u16,
        token: Option<&str>,
    ) -> Result<ChemSshIdentity, SshError> {
        let channel = self.open_direct_tcpip(remote_host, remote_port).await?;
        fetch_identity_over_stream(channel.into_stream(), remote_host, remote_port, token).await
    }

    async fn open_sftp_channel(&self) -> Result<russh::Channel<client::Msg>, SshError> {
        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Sftp(format!("open session channel: {e}")))?;
        channel
            .request_subsystem(true, "sftp")
            .await
            .map_err(|e| SshError::Sftp(format!("request sftp subsystem: {e}")))?;
        Ok(channel)
    }

    pub async fn open_sftp(&self) -> Result<SftpSession, SshError> {
        let channel = self.open_sftp_channel().await?;
        SftpSession::new(channel.into_stream())
            .await
            .map_err(|e| SshError::Sftp(format!("init sftp session: {e}")))
    }

    /// Open a separate SFTP protocol channel for extensions the high-level client
    /// does not expose. The caller must negotiate it with `RawSftpSession::init`.
    pub async fn open_raw_sftp(&self) -> Result<RawSftpSession, SshError> {
        let channel = self.open_sftp_channel().await?;
        Ok(RawSftpSession::new(channel.into_stream()))
    }

    /// Lazily opens and caches a single persistent raw SFTP channel on this SSH
    /// connection so repeated file uploads/overwrites reuse the same remote
    /// `sftp-server` subsystem instead of spawning a new subprocess per file.
    pub async fn shared_raw_sftp(&self, timeout: Duration) -> Option<CachedRawSftp> {
        let mut guard = self.raw_sftp.lock().await;
        if let Some(cached) = guard.as_ref() {
            return Some(cached.clone());
        }
        let opened = tokio::time::timeout(timeout, async {
            let raw = self.open_raw_sftp().await.ok()?;
            let version = raw.init().await.ok()?;
            let has_posix_rename = version
                .extensions
                .get("posix-rename@openssh.com")
                .is_some_and(|v| v == "1");
            Some(CachedRawSftp {
                session: Arc::new(raw),
                has_posix_rename,
                verified_parents: Arc::new(std::sync::Mutex::new(std::collections::HashSet::new())),
            })
        })
        .await
        .ok()
        .flatten();
        if let Some(ref cached) = opened {
            *guard = Some(cached.clone());
        }
        opened
    }

    /// Drops the cached raw SFTP channel if it matches `stale` (e.g. after a channel error).
    pub async fn invalidate_raw_sftp(&self, stale: &CachedRawSftp) {
        let mut guard = self.raw_sftp.lock().await;
        if let Some(current) = guard.as_ref() {
            if Arc::ptr_eq(&current.session, &stale.session) {
                *guard = None;
            }
        }
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

    pub async fn setup_shell_integration(&self, proxy_env: &[EnvPair]) -> Option<ShellIntegration> {
        let setup_script = build_shell_integration_setup_script(proxy_env);
        let output = self
            .exec_with_stdin("/bin/sh -s", setup_script.as_bytes(), SHELL_SETUP_TIMEOUT)
            .await
            .ok()?;
        parse_shell_integration_output(&output.stdout)
    }

    pub async fn prepare_pty_channel_with_x11_fallback(
        &self,
        cols: u32,
        rows: u32,
        term: &str,
        x11: Option<&PreparedX11>,
    ) -> Result<(PreparedPtyChannel, Option<String>), SshError> {
        let x11_forwarder = x11.map(X11ForwarderState::from_prepared);
        self.set_x11_forwarder(x11_forwarder);

        let channel = self
            .handle
            .channel_open_session()
            .await
            .map_err(|e| SshError::Shell(format!("open shell channel: {e}")))?;

        let result = prepare_pty_channel(channel, cols, rows, term, x11).await;
        match result {
            Ok(prepared) => Ok((prepared, None)),
            Err(SshError::X11Timeout) if x11.is_some() => {
                self.set_x11_forwarder(None);
                let warn =
                    "X11 forwarding timed out on remote SSH server; connected without X11"
                        .to_string();
                let retry_channel = self
                    .handle
                    .channel_open_session()
                    .await
                    .map_err(|e| SshError::Shell(format!("reopen shell channel: {e}")))?;
                let prepared = prepare_pty_channel(retry_channel, cols, rows, term, None).await?;
                Ok((prepared, Some(warn)))
            }
            Err(SshError::Shell(msg)) if x11.is_some() && msg.contains("X11") => {
                self.set_x11_forwarder(None);
                let warn = format!("{msg}; connected without X11");
                let retry_channel = self
                    .handle
                    .channel_open_session()
                    .await
                    .map_err(|e| SshError::Shell(format!("reopen shell channel: {e}")))?;
                let prepared = prepare_pty_channel(retry_channel, cols, rows, term, None).await?;
                Ok((prepared, Some(warn)))
            }
            Err(err) => {
                self.set_x11_forwarder(None);
                Err(err)
            }
        }
    }

    pub async fn open_pty_shell(
        &self,
        options: ShellOpenOptions,
        proxy_env: &[EnvPair],
        use_shell_integration: bool,
    ) -> Result<OpenedPtyShell, SshError> {
        let integration_fut = async {
            if use_shell_integration {
                self.setup_shell_integration(proxy_env).await
            } else {
                None
            }
        };
        let channel_fut = self.prepare_pty_channel_with_x11_fallback(
            options.cols,
            options.rows,
            &options.term,
            options.x11.as_ref(),
        );

        let (integration, channel_res) = tokio::join!(integration_fut, channel_fut);
        let (prepared, x11_warning) = channel_res?;

        let startup_command = if let Some(cmd) = options.startup_command {
            Some(cmd)
        } else if let Some(ref integ) = integration {
            Some(integ.interactive_command(proxy_env))
        } else if !proxy_env.is_empty() {
            Some(proxy_login_shell_command("/bin/sh", proxy_env))
        } else {
            None
        };

        let (shell, events) = prepared
            .start(startup_command.as_deref(), integration.clone())
            .await?;

        Ok(OpenedPtyShell {
            shell,
            events,
            integration,
            x11_warning,
        })
    }

    pub async fn query_shell_directory(&self, integration: &ShellIntegration) -> Option<String> {
        let cmd = build_query_cwd_command(&integration.pid_file);
        let output = self
            .exec_command_timeout(&cmd, SHELL_CWD_TIMEOUT)
            .await
            .ok()?;
        parse_query_cwd_output(output.stdout.as_bytes())
    }

    pub async fn cleanup_shell_integration(&self, integration: &ShellIntegration) {
        if let Some(cmd) = cleanup_shell_integration_command(&integration.dir) {
            let _ = self
                .exec_command_timeout(&cmd, Duration::from_secs(3))
                .await;
        }
    }

    pub fn is_closed(&self) -> bool {
        self.handle.is_closed()
    }

    /// Probes the underlying SSH transport using a keepalive ping.
    /// Re-checks `is_closed()` because `russh::client::Handle::send_ping` ignores channel closure errors.
    pub async fn probe_alive(&self, timeout_dur: Duration) -> bool {
        if self.handle.is_closed() {
            return false;
        }
        let res = tokio::time::timeout(timeout_dur, self.handle.send_ping()).await;
        matches!(res, Ok(Ok(())) if !self.handle.is_closed())
    }

    pub async fn disconnect(&self) {
        *self.raw_sftp.lock().await = None;
        let _ = self
            .handle
            .disconnect(russh::Disconnect::ByApplication, "closed", "en")
            .await;
    }
}

pub async fn fetch_local_identity(
    host: &str,
    port: u16,
    token: Option<&str>,
) -> Result<ChemSshIdentity, SshError> {
    let connect_host = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    };
    let stream = tokio::time::timeout(IDENTITY_FETCH_TIMEOUT, TcpStream::connect(&connect_host))
        .await
        .map_err(|_| SshError::Timeout(IDENTITY_FETCH_TIMEOUT))?
        .map_err(|e| SshError::Protocol(format!("connect local identity endpoint: {e}")))?;

    fetch_identity_over_stream(stream, host, port, token).await
}

async fn fetch_identity_over_stream<S>(
    mut stream: S,
    host: &str,
    port: u16,
    token: Option<&str>,
) -> Result<ChemSshIdentity, SshError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    tokio::time::timeout(IDENTITY_FETCH_TIMEOUT, async {
        let mut req = format!(
            "GET /api/system/identity HTTP/1.1\r\nHost: {host}:{port}\r\nConnection: close\r\nUser-Agent: chemssh-launcher/0.4.0\r\n"
        );
        if let Some(tok) = token {
            let clean = tok.replace(['\r', '\n', '\0'], "");
            if !clean.is_empty() {
                req.push_str(&format!("X-ChemSSH-Token: {clean}\r\n"));
            }
        }
        req.push_str("\r\n");

        stream.write_all(req.as_bytes()).await?;
        stream.flush().await?;

        let mut buf = Vec::with_capacity(1024);
        let mut chunk = [0u8; 4096];
        loop {
            let n = stream.read(&mut chunk).await?;
            if n == 0 {
                break;
            }
            if buf.len() + n > IDENTITY_RESPONSE_MAX_BYTES {
                return Err(SshError::Protocol("identity response exceeded 64KB".into()));
            }
            buf.extend_from_slice(&chunk[..n]);
        }

        let (headers_raw, body_raw) = split_http_response(&buf)?;
        let mut lines = headers_raw.lines();
        let status_line = lines
            .next()
            .ok_or_else(|| SshError::Protocol("empty HTTP status line".into()))?;
        let status_code: u16 = status_line
            .split_whitespace()
            .nth(1)
            .and_then(|s| s.parse().ok())
            .ok_or_else(|| SshError::Protocol(format!("malformed HTTP status line: {status_line}")))?;
        if !(200..=299).contains(&status_code) {
            return Err(SshError::Protocol(format!(
                "identity endpoint returned HTTP {status_code}"
            )));
        }

        let is_chunked = lines.any(|l| {
            l.split_once(':')
                .map(|(k, v)| {
                    k.trim().eq_ignore_ascii_case("transfer-encoding")
                        && v.to_ascii_lowercase().contains("chunked")
                })
                .unwrap_or(false)
        });
        let body = if is_chunked {
            decode_chunked_http_body(body_raw)?
        } else {
            body_raw.to_vec()
        };

        let identity: ChemSshIdentity = serde_json::from_slice(&body)
            .map_err(|e| SshError::Protocol(format!("decode ChemSSH identity JSON: {e}")))?;
        if identity.pid == 0 {
            return Err(SshError::Protocol(
                "ChemSSH identity returned missing or zero pid".into(),
            ));
        }
        if !identity.app.is_empty() {
            if !identity.app.eq_ignore_ascii_case("chemssh") {
                return Err(SshError::Protocol(format!(
                    "unexpected identity app {:?}",
                    identity.app
                )));
            }
        } else if !identity.service.is_empty() && !identity.service.eq_ignore_ascii_case("chemssh") {
            return Err(SshError::Protocol(format!(
                "unexpected identity service {:?}",
                identity.service
            )));
        }
        Ok(identity)
    })
    .await
    .map_err(|_| SshError::Timeout(IDENTITY_FETCH_TIMEOUT))?
}

fn split_http_response(raw: &[u8]) -> Result<(&str, &[u8]), SshError> {
    if let Some(idx) = raw.windows(4).position(|w| w == b"\r\n\r\n") {
        let headers = std::str::from_utf8(&raw[..idx])
            .map_err(|e| SshError::Protocol(format!("non-UTF8 HTTP headers: {e}")))?;
        return Ok((headers, &raw[idx + 4..]));
    }
    if let Some(idx) = raw.windows(2).position(|w| w == b"\n\n") {
        let headers = std::str::from_utf8(&raw[..idx])
            .map_err(|e| SshError::Protocol(format!("non-UTF8 HTTP headers: {e}")))?;
        return Ok((headers, &raw[idx + 2..]));
    }
    Err(SshError::Protocol(
        "incomplete HTTP response from identity endpoint".into(),
    ))
}

fn decode_chunked_http_body(mut input: &[u8]) -> Result<Vec<u8>, SshError> {
    let mut out = Vec::new();
    loop {
        let Some(crlf) = input.windows(2).position(|w| w == b"\r\n") else {
            return Err(SshError::Protocol("malformed chunked response".into()));
        };
        let size_str = std::str::from_utf8(&input[..crlf])
            .map_err(|_| SshError::Protocol("invalid chunk size encoding".into()))?
            .split(';')
            .next()
            .unwrap_or("")
            .trim();
        let size = usize::from_str_radix(size_str, 16)
            .map_err(|_| SshError::Protocol("invalid chunk hex size".into()))?;
        input = &input[crlf + 2..];
        if size == 0 {
            return Ok(out);
        }
        if input.len() < size + 2 {
            return Err(SshError::Protocol("truncated HTTP chunk body".into()));
        }
        out.extend_from_slice(&input[..size]);
        input = &input[size + 2..];
    }
}

async fn connect_tcp_happy_eyeballs(host: &str, port: u16) -> Result<TcpStream, SshError> {
    if let Ok(ip) = host.trim_matches(['[', ']']).parse::<IpAddr>() {
        let addr = SocketAddr::new(ip, port);
        let stream = tokio::time::timeout(TCP_CONNECT_TIMEOUT, TcpStream::connect(addr))
            .await
            .map_err(|_| SshError::Connect(format!("TCP connect timed out for {addr}")))?
            .map_err(|e| SshError::Connect(e.to_string()))?;
        let _ = stream.set_nodelay(true);
        return Ok(stream);
    }

    let raw_addrs: Vec<SocketAddr> =
        tokio::time::timeout(TCP_CONNECT_TIMEOUT, tokio::net::lookup_host((host, port)))
            .await
            .map_err(|_| SshError::Connect(format!("DNS lookup timed out for {host}:{port}")))?
            .map_err(|e| SshError::Connect(format!("DNS lookup failed for {host}:{port}: {e}")))?
            .collect();

    if raw_addrs.is_empty() {
        return Err(SshError::Connect(format!(
            "no addresses resolved for {host}:{port}"
        )));
    }

    let addrs = interleave_socket_addrs(&raw_addrs);
    if addrs.len() == 1 {
        let stream = tokio::time::timeout(TCP_CONNECT_TIMEOUT, TcpStream::connect(addrs[0]))
            .await
            .map_err(|_| SshError::Connect(format!("TCP connect timed out for {host}:{port}")))?
            .map_err(|e| SshError::Connect(e.to_string()))?;
        let _ = stream.set_nodelay(true);
        return Ok(stream);
    }

    let deadline = tokio::time::Instant::now() + TCP_CONNECT_TIMEOUT;
    let (tx, mut rx) = mpsc::channel::<Result<TcpStream, std::io::Error>>(addrs.len());
    let mut handles: Vec<tokio::task::JoinHandle<()>> = Vec::with_capacity(addrs.len());
    let mut last_err: Option<std::io::Error> = None;
    let mut in_flight = 0usize;

    for (idx, addr) in addrs.into_iter().enumerate() {
        if idx > 0 {
            let now = tokio::time::Instant::now();
            if now >= deadline {
                break;
            }
            let wait_for = HAPPY_EYEBALLS_FALLBACK_DELAY.min(deadline - now);
            match tokio::time::timeout(wait_for, rx.recv()).await {
                Ok(Some(Ok(stream))) => {
                    for h in handles {
                        h.abort();
                    }
                    let _ = stream.set_nodelay(true);
                    return Ok(stream);
                }
                Ok(Some(Err(err))) => {
                    in_flight = in_flight.saturating_sub(1);
                    last_err = Some(err);
                }
                Ok(None) => break,
                Err(_) => {}
            }
        }

        let tx_clone = tx.clone();
        in_flight += 1;
        handles.push(tokio::spawn(async move {
            let res = TcpStream::connect(addr).await;
            let _ = tx_clone.send(res).await;
        }));
    }
    drop(tx);

    while in_flight > 0 {
        let now = tokio::time::Instant::now();
        if now >= deadline {
            break;
        }
        match tokio::time::timeout(deadline - now, rx.recv()).await {
            Ok(Some(Ok(stream))) => {
                for h in handles {
                    h.abort();
                }
                let _ = stream.set_nodelay(true);
                return Ok(stream);
            }
            Ok(Some(Err(err))) => {
                in_flight -= 1;
                last_err = Some(err);
            }
            Ok(None) => break,
            Err(_) => break,
        }
    }

    for h in handles {
        h.abort();
    }
    if let Some(err) = last_err {
        Err(SshError::Connect(err.to_string()))
    } else {
        Err(SshError::Connect(format!(
            "TCP connect timed out for {host}:{port}"
        )))
    }
}

fn interleave_socket_addrs(raw: &[SocketAddr]) -> Vec<SocketAddr> {
    let v4: Vec<SocketAddr> = raw.iter().copied().filter(|a| a.is_ipv4()).collect();
    let v6: Vec<SocketAddr> = raw.iter().copied().filter(|a| a.is_ipv6()).collect();
    let first_is_v6 = raw.first().map(|a| a.is_ipv6()).unwrap_or(false);
    let (primary, secondary) = if first_is_v6 { (v6, v4) } else { (v4, v6) };

    let mut out = Vec::with_capacity(raw.len());
    let max_len = primary.len().max(secondary.len());
    for i in 0..max_len {
        if let Some(&a) = primary.get(i) {
            out.push(a);
        }
        if let Some(&b) = secondary.get(i) {
            out.push(b);
        }
    }
    out
}

async fn authenticate_keyboard_interactive_password(
    handle: &mut Handle<SshClientHandler>,
    user: &str,
    password: &str,
) -> Result<AuthResult, SshError> {
    let mut reply = handle
        .authenticate_keyboard_interactive_start(user, None::<String>)
        .await
        .map_err(|e| {
            SshError::Auth(format!("keyboard-interactive authentication failed: {e}"))
        })?;

    for _ in 0..MAX_KEYBOARD_INTERACTIVE_ROUNDS {
        match reply {
            KeyboardInteractiveAuthResponse::Success => return Ok(AuthResult::Success),
            KeyboardInteractiveAuthResponse::Failure {
                remaining_methods,
                partial_success,
            } => {
                return Ok(AuthResult::Failure {
                    remaining_methods,
                    partial_success,
                });
            }
            KeyboardInteractiveAuthResponse::InfoRequest { prompts, .. } => {
                let responses = vec![password.to_string(); prompts.len()];
                reply = handle
                    .authenticate_keyboard_interactive_respond(responses)
                    .await
                    .map_err(|e| {
                        SshError::Auth(format!(
                            "keyboard-interactive response failed: {e}"
                        ))
                    })?;
            }
        }
    }

    Err(SshError::Auth(
        "keyboard-interactive authentication exceeded maximum challenge rounds".into(),
    ))
}

async fn authenticate_rsa_publickey_with_fallback(
    handle: &mut Handle<SshClientHandler>,
    user: &str,
    key: Arc<russh::keys::PrivateKey>,
) -> Result<AuthResult, SshError> {
    let negotiated = tokio::time::timeout(
        RSA_HASH_EXT_INFO_TIMEOUT,
        handle.best_supported_rsa_hash(),
    )
    .await
    .ok()
    .and_then(|r| r.ok())
    .flatten();

    let mut candidates: Vec<Option<HashAlg>> = Vec::with_capacity(3);
    if let Some(pref) = negotiated {
        candidates.push(pref);
    }
    for alg in [Some(HashAlg::Sha512), Some(HashAlg::Sha256), None] {
        if !candidates.contains(&alg) {
            candidates.push(alg);
        }
    }

    let mut last_failure = AuthResult::Failure {
        remaining_methods: russh::MethodSet::empty(),
        partial_success: false,
    };
    for hash_alg in candidates {
        let key_with_alg = PrivateKeyWithHashAlg::new(Arc::clone(&key), hash_alg);
        let res = handle
            .authenticate_publickey(user, key_with_alg)
            .await
            .map_err(|e| SshError::Auth(format!("public key authentication failed: {e}")))?;
        match res {
            AuthResult::Success => return Ok(AuthResult::Success),
            AuthResult::Failure {
                remaining_methods,
                partial_success,
            } => {
                let stop_early = !remaining_methods.is_empty()
                    && !remaining_methods.contains(&MethodKind::PublicKey);
                last_failure = AuthResult::Failure {
                    remaining_methods,
                    partial_success,
                };
                if stop_early {
                    break;
                }
            }
        }
    }
    Ok(last_failure)
}

pub(crate) fn expand_tilde_path(raw: &str) -> PathBuf {
    let trimmed = raw.trim();
    if trimmed == "~" || trimmed.starts_with("~/") || trimmed.starts_with("~\\") {
        if let Some(home) = std::env::var_os("USERPROFILE")
            .filter(|v| !v.is_empty())
            .or_else(|| std::env::var_os("HOME").filter(|v| !v.is_empty()))
        {
            let base = PathBuf::from(home);
            if trimmed == "~" {
                return base;
            }
            return base.join(&trimmed[2..]);
        }
    }
    PathBuf::from(trimmed)
}

fn format_auth_rejection(
    remaining_methods: &russh::MethodSet,
    partial_success: bool,
) -> String {
    if remaining_methods.is_empty() {
        return "authentication rejected by server".into();
    }
    let allowed: Vec<&'static str> = remaining_methods.iter().map(<&str>::from).collect();
    if partial_success {
        format!(
            "authentication rejected by server (allowed: {}, partial_success=true)",
            allowed.join(", ")
        )
    } else {
        format!(
            "authentication rejected by server (allowed: {})",
            allowed.join(", ")
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use russh::keys::{Algorithm, PrivateKey};
    use russh::server::{self, Server};
    use russh::ChannelId;
    use tokio::net::TcpListener;

    #[derive(Clone)]
    struct CheckPortBehavior {
        reusable: bool,
    }

    struct CheckPortServer {
        behavior: CheckPortBehavior,
    }

    impl server::Server for CheckPortServer {
        type Handler = CheckPortHandler;

        fn new_client(&mut self, _peer_addr: Option<std::net::SocketAddr>) -> Self::Handler {
            CheckPortHandler {
                behavior: self.behavior.clone(),
                channels: HashMap::new(),
            }
        }
    }

    struct CheckPortHandler {
        behavior: CheckPortBehavior,
        channels: HashMap<ChannelId, ()>,
    }

    impl server::Handler for CheckPortHandler {
        type Error = russh::Error;

        async fn auth_none(&mut self, _user: &str) -> Result<server::Auth, Self::Error> {
            Ok(server::Auth::Accept)
        }

        async fn channel_open_session(
            &mut self,
            channel: russh::Channel<server::Msg>,
            reply: server::ChannelOpenHandle,
            _session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            self.channels.insert(channel.id(), ());
            reply.accept().await;
            Ok(())
        }

        async fn exec_request(
            &mut self,
            channel: ChannelId,
            data: &[u8],
            session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            let cmd = String::from_utf8_lossy(data);
            assert!(cmd.contains("--check-port"));
            session.channel_success(channel)?;
            if self.behavior.reusable {
                session.data(
                    channel,
                    b"Port 8888 is already used by a reusable ChemSSH server\n".as_slice(),
                )?;
                session.exit_status_request(channel, 0)?;
            } else {
                session.data(channel, b"Port 8888 is available\n".as_slice())?;
                session.exit_status_request(channel, 1)?;
            }
            session.eof(channel)?;
            session.close(channel)?;
            Ok(())
        }
    }

    #[tokio::test]
    async fn run_check_port_command_detects_reusable_server() {
        let key = PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519).unwrap();
        let cfg = Arc::new(server::Config {
            keys: vec![key],
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let mut srv = CheckPortServer {
            behavior: CheckPortBehavior { reusable: true },
        };
        tokio::spawn(async move {
            let _ = srv.run_on_socket(cfg, &listener).await;
        });

        let kh_dir = tempfile::tempdir().unwrap();
        let verifier = KnownHostsVerifier::new(
            kh_dir.path().join("kh"),
            "127.0.0.1",
            addr.port(),
            crate::HostKeyPolicy::AcceptNew,
        );
        let handler = SshClientHandler::new(
            verifier,
            Arc::new(Mutex::new(None)),
            Arc::new(Mutex::new(None)),
            Arc::new(Mutex::new(None)),
        );
        let mut handle =
            russh::client::connect(Arc::new(russh::client::Config::default()), addr, handler)
                .await
                .unwrap();
        assert!(handle.authenticate_none("user").await.unwrap().success());
        let ssh_conn = SshConnection::from_raw_handle(handle);

        let mut profile = Profile::remote_defaults();
        profile.start_command = "chemssh --config config.yaml".into();
        let res = ssh_conn.run_check_port_command(&profile, &[]).await.unwrap();
        assert!(res.reusable);
        assert_eq!(res.exit_code, Some(0));
    }

    #[tokio::test]
    async fn fetch_local_identity_parses_chunked_json_and_token_header() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let (mut sock, _) = listener.accept().await.unwrap();
            let mut req = [0u8; 1024];
            let n = sock.read(&mut req).await.unwrap();
            let req_text = String::from_utf8_lossy(&req[..n]);
            assert!(req_text.contains("X-ChemSSH-Token: test-token"));
            let json = r#"{"service":"chemssh","pid":4242,"host":"127.0.0.1","port":8888}"#;
            let resp = format!(
                "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n{:x}\r\n{}\r\n0\r\n\r\n",
                json.len(),
                json
            );
            sock.write_all(resp.as_bytes()).await.unwrap();
        });

        let ident = fetch_local_identity("127.0.0.1", addr.port(), Some("test-token"))
            .await
            .unwrap();
        assert_eq!(ident.pid, 4242);
        assert_eq!(ident.service, "chemssh");
    }

    #[tokio::test]
    async fn fetch_local_identity_parses_full_chemssh_payload() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let (mut sock, _) = listener.accept().await.unwrap();
            let mut req = [0u8; 1024];
            let _ = sock.read(&mut req).await.unwrap();
            let json = r#"{"app":"chemssh","project_version":"1.2.3","pid":9001,"scheduler":"slurm","workspace_root":"/home/user/project"}"#;
            let resp = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{}",
                json.len(),
                json
            );
            sock.write_all(resp.as_bytes()).await.unwrap();
        });

        let ident = fetch_local_identity("127.0.0.1", addr.port(), None)
            .await
            .unwrap();
        assert_eq!(ident.app, "chemssh");
        assert_eq!(ident.project_version, "1.2.3");
        assert_eq!(ident.pid, 9001);
        assert_eq!(ident.scheduler, "slurm");
        assert_eq!(ident.workspace_root, "/home/user/project");
    }

    #[tokio::test]
    async fn fetch_local_identity_rejects_non_chemssh_app() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let (mut sock, _) = listener.accept().await.unwrap();
            let mut req = [0u8; 1024];
            let _ = sock.read(&mut req).await.unwrap();
            let json = r#"{"app":"other-service","pid":9001}"#;
            let resp = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{}",
                json.len(),
                json
            );
            sock.write_all(resp.as_bytes()).await.unwrap();
        });

        let err = fetch_local_identity("127.0.0.1", addr.port(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("unexpected identity app"));
    }

    #[test]
    fn expand_tilde_path_resolves_home_prefix() {
        assert_eq!(expand_tilde_path("/abs/key"), PathBuf::from("/abs/key"));
        assert_eq!(expand_tilde_path("rel/key"), PathBuf::from("rel/key"));
        let home = std::env::var_os("USERPROFILE")
            .filter(|v| !v.is_empty())
            .or_else(|| std::env::var_os("HOME").filter(|v| !v.is_empty()));
        if let Some(base) = home {
            let base_pb = PathBuf::from(base);
            assert_eq!(expand_tilde_path("~"), base_pb);
            assert_eq!(expand_tilde_path("~/.ssh/id_ed25519"), base_pb.join(".ssh/id_ed25519"));
        }
    }

    struct AuthFallbackServer {
        expected_password: Option<String>,
        expected_rsa_pub: Option<PublicKey>,
        reject_legacy_sha1: bool,
    }

    impl server::Server for AuthFallbackServer {
        type Handler = AuthFallbackHandler;

        fn new_client(&mut self, _peer_addr: Option<SocketAddr>) -> Self::Handler {
            AuthFallbackHandler {
                expected_password: self.expected_password.clone(),
                expected_rsa_pub: self.expected_rsa_pub.clone(),
                reject_legacy_sha1: self.reject_legacy_sha1,
            }
        }
    }

    struct AuthFallbackHandler {
        expected_password: Option<String>,
        expected_rsa_pub: Option<PublicKey>,
        reject_legacy_sha1: bool,
    }

    impl server::Handler for AuthFallbackHandler {
        type Error = russh::Error;

        async fn auth_password(&mut self, _user: &str, _password: &str) -> Result<server::Auth, Self::Error> {
            Ok(server::Auth::Reject {
                proceed_with_methods: Some(russh::MethodSet::from(&[MethodKind::KeyboardInteractive][..])),
                partial_success: false,
            })
        }

        async fn auth_keyboard_interactive<'a>(
            &'a mut self,
            _user: &str,
            _submethods: &str,
            response: Option<server::Response<'a>>,
        ) -> Result<server::Auth, Self::Error> {
            let Some(expected) = self.expected_password.as_deref() else {
                return Ok(server::Auth::reject());
            };
            let Some(mut resp) = response else {
                return Ok(server::Auth::Partial {
                    name: std::borrow::Cow::Borrowed(""),
                    instructions: std::borrow::Cow::Borrowed(""),
                    prompts: std::borrow::Cow::Borrowed(&[(
                        std::borrow::Cow::Borrowed("Password: "),
                        false,
                    )]),
                });
            };
            match resp.next() {
                Some(bytes) if bytes.as_ref() == expected.as_bytes() => Ok(server::Auth::Accept),
                _ => Ok(server::Auth::reject()),
            }
        }

        async fn auth_publickey(
            &mut self,
            _user: &str,
            key: server::VerifiedPublicKey<'_>,
        ) -> Result<server::Auth, Self::Error> {
            let Some(ref expected) = self.expected_rsa_pub else {
                return Ok(server::Auth::reject());
            };
            if self.reject_legacy_sha1
                && matches!(key.algorithm, russh::keys::Algorithm::Rsa { hash: None })
            {
                return Ok(server::Auth::reject());
            }
            if key.public_key.key_data() == expected.key_data() {
                Ok(server::Auth::Accept)
            } else {
                Ok(server::Auth::reject())
            }
        }
    }

    #[tokio::test]
    async fn password_auth_falls_back_to_keyboard_interactive() {
        let host_key = PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519).unwrap();
        let cfg = Arc::new(server::Config {
            keys: vec![host_key],
            auth_rejection_time: Duration::from_millis(10),
            auth_rejection_time_initial: Some(Duration::from_millis(10)),
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let mut srv = AuthFallbackServer {
            expected_password: Some("dummy-pw".into()),
            expected_rsa_pub: None,
            reject_legacy_sha1: false,
        };
        tokio::spawn(async move {
            let _ = russh::server::Server::run_on_socket(&mut srv, cfg, &listener).await;
        });

        let kh_dir = tempfile::tempdir().unwrap();
        let verifier = KnownHostsVerifier::new(
            kh_dir.path().join("kh"),
            "127.0.0.1",
            addr.port(),
            crate::HostKeyPolicy::AcceptNew,
        );
        let mut profile = Profile::remote_defaults();
        profile.ssh_host = "127.0.0.1".into();
        profile.ssh_port = addr.port();
        profile.ssh_user = "user".into();
        profile.auth_method = AUTH_PASSWORD.into();
        let creds = SshCredentials {
            password: Some("dummy-pw".into()),
            private_key_passphrase: None,
        };
        let conn = SshConnection::connect(&profile, &creds, verifier)
            .await
            .unwrap();
        conn.disconnect().await;
    }

    #[tokio::test]
    async fn rsa_publickey_auth_falls_back_to_sha2_when_no_ext_info() {
        let host_key = PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519).unwrap();
        let user_key = PrivateKey::random(&mut rand::rng(), Algorithm::Rsa { hash: None }).unwrap();
        let user_pub = user_key.public_key().clone();
        let key_dir = tempfile::tempdir().unwrap();
        let key_path = key_dir.path().join("id_rsa");
        user_key
            .write_openssh_file(&key_path, russh::keys::ssh_key::LineEnding::LF)
            .unwrap();

        let cfg = Arc::new(server::Config {
            keys: vec![host_key],
            auth_rejection_time: Duration::from_millis(10),
            auth_rejection_time_initial: Some(Duration::from_millis(10)),
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let mut srv = AuthFallbackServer {
            expected_password: None,
            expected_rsa_pub: Some(user_pub),
            reject_legacy_sha1: true,
        };
        tokio::spawn(async move {
            let _ = russh::server::Server::run_on_socket(&mut srv, cfg, &listener).await;
        });

        let kh_dir = tempfile::tempdir().unwrap();
        let verifier = KnownHostsVerifier::new(
            kh_dir.path().join("kh"),
            "127.0.0.1",
            addr.port(),
            crate::HostKeyPolicy::AcceptNew,
        );
        let mut profile = Profile::remote_defaults();
        profile.ssh_host = "127.0.0.1".into();
        profile.ssh_port = addr.port();
        profile.ssh_user = "user".into();
        profile.auth_method = AUTH_PRIVATE_KEY.into();
        profile.private_key_path = key_path.to_string_lossy().into_owned();
        let conn = SshConnection::connect(&profile, &SshCredentials::default(), verifier)
            .await
            .unwrap();
        conn.disconnect().await;
    }

    #[tokio::test]
    async fn connection_liveness_and_probe_reflect_disconnect() {
        let host_key = PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519).unwrap();
        let cfg = Arc::new(server::Config {
            keys: vec![host_key],
            auth_rejection_time: Duration::from_millis(10),
            auth_rejection_time_initial: Some(Duration::from_millis(10)),
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let mut srv = AuthFallbackServer {
            expected_password: Some("dummy-pw".into()),
            expected_rsa_pub: None,
            reject_legacy_sha1: false,
        };
        tokio::spawn(async move {
            let _ = russh::server::Server::run_on_socket(&mut srv, cfg, &listener).await;
        });

        let kh_dir = tempfile::tempdir().unwrap();
        let verifier = KnownHostsVerifier::new(
            kh_dir.path().join("kh"),
            "127.0.0.1",
            addr.port(),
            crate::HostKeyPolicy::AcceptNew,
        );
        let mut profile = Profile::remote_defaults();
        profile.ssh_host = "127.0.0.1".into();
        profile.ssh_port = addr.port();
        profile.ssh_user = "user".into();
        profile.auth_method = AUTH_PASSWORD.into();
        let creds = SshCredentials {
            password: Some("dummy-pw".into()),
            private_key_passphrase: None,
        };
        let conn = SshConnection::connect(&profile, &creds, verifier)
            .await
            .unwrap();

        assert!(!conn.is_closed());
        assert!(conn.probe_alive(Duration::from_secs(2)).await);

        conn.disconnect().await;
        for _ in 0..25 {
            if conn.is_closed() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert!(conn.is_closed());
        assert!(!conn.probe_alive(Duration::from_millis(500)).await);
    }
}
