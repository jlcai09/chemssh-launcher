use std::collections::HashMap;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use core_config::{new_id, FileStore, Profile, ProfileStore};
use core_secret::SecretStore;
use core_ssh::{shell_quote, HostKeyPolicy, KnownHostsVerifier, SshConnection};
use core_transfer::{
    build_compress_step, build_extract_step, copy_remote_to_remote_once, delete_remote_path,
    detect_remote_tools, download_file_resumable_once, is_sftp_transport_error, list_remote_dir,
    remote_io_err, run_remote_step, sftp_err, upload_file_resumable_once, ArchiveTools, FsEntry,
    TransferError,
};
use russh_sftp::client::fs::File as SftpRemoteFile;
use russh_sftp::client::SftpSession;
use russh_sftp::protocol::OpenFlags;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tauri::State;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{Mutex, Notify};

use crate::commands::backend::{open_path_in_system, open_text_file_in_system};
use crate::commands::sessions::{format_ssh_error, load_ssh_credentials_from};
use crate::commands::sftp_sync::{OpenSyncEvent, MAX_SYNC_FILE_BYTES};
use crate::state::{AppState, LauncherLogStore};

const DEFAULT_DOWNLOAD_BYTES_LIMIT: u64 = 256 * 1024 * 1024;
const DOWNLOAD_CHUNK_SIZE: usize = 256 * 1024;
const RECONNECT_COOLDOWN: Duration = Duration::from_secs(2);
const KEEPALIVE_BASE_INTERVAL: Duration = Duration::from_secs(25);
const KEEPALIVE_MAX_BACKOFF: Duration = Duration::from_secs(60);
const KEEPALIVE_PROBE_TIMEOUT: Duration = Duration::from_secs(5);
const COPY_REMOTE_PROBE_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Debug, Clone, Serialize)]
pub struct SftpConnectResponse {
    pub id: String,
    pub path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct SftpOpenFileResponse {
    pub path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct OpenSyncEventsResponse {
    pub events: Vec<OpenSyncEvent>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopySupportResponse {
    pub supported: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerCopyItem {
    pub source_path: String,
    pub relative_path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopyItemResult {
    pub source_path: String,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopyResponse {
    pub supported: bool,
    pub results: Vec<ServerCopyItemResult>,
}

#[derive(Debug, Clone, Serialize)]
pub struct UploadBytesBeginResponse {
    pub upload_id: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct DownloadBytesResponse {
    pub name: String,
    pub data_b64: String,
    pub size: u64,
}

#[derive(Clone)]
pub(crate) struct SftpTransport {
    pub connection: Arc<SshConnection>,
    pub sftp: Arc<SftpSession>,
    pub profile_id: String,
    pub ssh_host: String,
    pub ssh_port: u16,
    pub ssh_user: String,
}

pub(crate) trait SessionTransport: Clone + Send + Sync + 'static {
    fn is_closed(&self) -> bool;
    fn probe_alive<'a>(
        &'a self,
        timeout: Duration,
    ) -> Pin<Box<dyn Future<Output = bool> + Send + 'a>>;
    fn try_reopen_sftp_channel<'a>(
        &'a self,
    ) -> Pin<Box<dyn Future<Output = Option<(Self, String)>> + Send + 'a>>;
    fn disconnect_async(self);
}

pub(crate) trait SessionDialer<T>: Send + Sync {
    fn dial<'a>(
        &'a self,
        policy: Option<HostKeyPolicy>,
    ) -> Pin<Box<dyn Future<Output = Result<(T, String), String>> + Send + 'a>>;
}

impl SessionTransport for SftpTransport {
    fn is_closed(&self) -> bool {
        self.connection.is_closed()
    }

    fn probe_alive<'a>(
        &'a self,
        timeout: Duration,
    ) -> Pin<Box<dyn Future<Output = bool> + Send + 'a>> {
        Box::pin(async move { self.connection.probe_alive(timeout).await })
    }

    fn try_reopen_sftp_channel<'a>(
        &'a self,
    ) -> Pin<Box<dyn Future<Output = Option<(Self, String)>> + Send + 'a>> {
        Box::pin(async move {
            let sftp = self.connection.open_sftp().await.ok()?;
            let home = sftp
                .canonicalize(".")
                .await
                .unwrap_or_else(|_| "/".to_string());
            Some((
                SftpTransport {
                    connection: Arc::clone(&self.connection),
                    sftp: Arc::new(sftp),
                    profile_id: self.profile_id.clone(),
                    ssh_host: self.ssh_host.clone(),
                    ssh_port: self.ssh_port,
                    ssh_user: self.ssh_user.clone(),
                },
                home,
            ))
        })
    }

    fn disconnect_async(self) {
        let conn = self.connection;
        tokio::spawn(async move {
            conn.disconnect().await;
        });
    }
}

struct ProfileSftpDialer {
    profiles: Arc<FileStore>,
    secrets: Arc<dyn SecretStore>,
    known_hosts_path: PathBuf,
    profile_id: String,
}

impl SessionDialer<SftpTransport> for ProfileSftpDialer {
    fn dial<'a>(
        &'a self,
        policy: Option<HostKeyPolicy>,
    ) -> Pin<Box<dyn Future<Output = Result<(SftpTransport, String), String>> + Send + 'a>> {
        Box::pin(async move {
            let profile = self.profiles.get(&self.profile_id).map_err(|e| e.to_string())?;
            dial_sftp_transport(
                self.secrets.as_ref(),
                &self.known_hosts_path,
                &profile,
                policy,
            )
            .await
        })
    }
}

struct SlotState<T> {
    transport: T,
    generation: u64,
    last_dial: Option<Instant>,
    closed: bool,
}

pub(crate) struct ReconnectableSession<T: SessionTransport> {
    pub id: String,
    cooldown: Duration,
    launcher_logs: Arc<LauncherLogStore>,
    dialer: Arc<dyn SessionDialer<T>>,
    state: Mutex<SlotState<T>>,
    reconnect_mu: Mutex<()>,
    monitor_cancel: Arc<Notify>,
}

impl<T: SessionTransport> ReconnectableSession<T> {
    fn new(
        id: String,
        cooldown: Duration,
        launcher_logs: Arc<LauncherLogStore>,
        dialer: Arc<dyn SessionDialer<T>>,
        initial: T,
    ) -> Self {
        Self {
            id,
            cooldown,
            launcher_logs,
            dialer,
            state: Mutex::new(SlotState {
                transport: initial,
                generation: 1,
                // Allow immediate recovery on the very first unexpected drop.
                last_dial: None,
                closed: false,
            }),
            reconnect_mu: Mutex::new(()),
            monitor_cancel: Arc::new(Notify::new()),
        }
    }

    async fn is_slot_closed(&self) -> bool {
        self.state.lock().await.closed
    }

    async fn snapshot(&self) -> Result<(T, u64), String> {
        let st = self.state.lock().await;
        if st.closed {
            return Err(format!("sftp session disconnected: {}", self.id));
        }
        Ok((st.transport.clone(), st.generation))
    }

    async fn ensure_live(&self) -> Result<(T, u64), String> {
        let (transport, gen) = self.snapshot().await?;
        if !transport.is_closed() {
            return Ok((transport, gen));
        }
        let (fresh, new_gen, _) = self
            .reconnect(Some(gen), Some(HostKeyPolicy::Strict), false, false)
            .await?;
        Ok((fresh, new_gen))
    }

    async fn reconnect(
        &self,
        failed_generation: Option<u64>,
        policy: Option<HostKeyPolicy>,
        force: bool,
        quiet_fail: bool,
    ) -> Result<(T, u64, String), String> {
        let _reconnect_guard = self.reconnect_mu.lock().await;

        let (old_transport, last_dial) = {
            let st = self.state.lock().await;
            if st.closed {
                return Err(format!("sftp session disconnected: {}", self.id));
            }
            if let Some(fg) = failed_generation {
                if st.generation > fg && !st.transport.is_closed() {
                    return Ok((st.transport.clone(), st.generation, String::new()));
                }
            }
            (st.transport.clone(), st.last_dial)
        };

        if !force
            && !old_transport.is_closed()
            && old_transport.probe_alive(KEEPALIVE_PROBE_TIMEOUT).await
        {
            if failed_generation.is_none() {
                let st = self.state.lock().await;
                if st.closed {
                    return Err(format!("sftp session disconnected: {}", self.id));
                }
                return Ok((st.transport.clone(), st.generation, String::new()));
            }
            if let Some((reopened, home)) = old_transport.try_reopen_sftp_channel().await {
                let gen = {
                    let mut st = self.state.lock().await;
                    if st.closed {
                        drop(st);
                        reopened.disconnect_async();
                        return Err(format!("sftp session disconnected: {}", self.id));
                    }
                    st.transport = reopened.clone();
                    st.generation = st.generation.saturating_add(1);
                    st.generation
                };
                self.launcher_logs.push(format!(
                    "[sftp:{}] reopened SFTP channel on live SSH transport",
                    self.id
                ));
                return Ok((reopened, gen, home));
            }
        }

        if !force {
            if let Some(prev) = last_dial {
                let elapsed = prev.elapsed();
                if elapsed < self.cooldown {
                    let wait_ms = (self.cooldown - elapsed).as_millis().max(1);
                    return Err(format!(
                        "SFTP reconnect is rate limited (retry in {wait_ms}ms)"
                    ));
                }
            }
        }

        {
            let mut st = self.state.lock().await;
            if st.closed {
                return Err(format!("sftp session disconnected: {}", self.id));
            }
            st.last_dial = Some(Instant::now());
        }

        old_transport.disconnect_async();

        if !quiet_fail {
            self.launcher_logs.push(format!(
                "[sftp:{}] connection lost, auto-reconnecting...",
                self.id
            ));
        }

        let (new_transport, home) = match self.dialer.dial(policy).await {
            Ok(ok) => ok,
            Err(err) => {
                if !quiet_fail {
                    self.launcher_logs.push(format!(
                        "[sftp:{}] auto-reconnect failed: {err}",
                        self.id
                    ));
                }
                return Err(err);
            }
        };

        let gen = {
            let mut st = self.state.lock().await;
            if st.closed {
                drop(st);
                new_transport.disconnect_async();
                return Err(format!("sftp session disconnected: {}", self.id));
            }
            st.transport = new_transport.clone();
            st.generation = st.generation.saturating_add(1);
            st.generation
        };

        self.launcher_logs
            .push(format!("[sftp:{}] auto-reconnected", self.id));
        Ok((new_transport, gen, home))
    }

    async fn close(&self) {
        let transport = {
            let mut st = self.state.lock().await;
            if st.closed {
                return;
            }
            st.closed = true;
            st.transport.clone()
        };
        self.monitor_cancel.notify_waiters();
        transport.disconnect_async();
    }

    fn start_keepalive_monitor(self: &Arc<Self>) {
        let session = Arc::clone(self);
        tokio::spawn(async move {
            let mut interval = KEEPALIVE_BASE_INTERVAL;
            let mut offline_logged = false;
            loop {
                tokio::select! {
                    _ = session.monitor_cancel.notified() => break,
                    _ = tokio::time::sleep(interval) => {}
                }

                let Ok((transport, gen)) = session.snapshot().await else {
                    break;
                };

                let alive =
                    !transport.is_closed() && transport.probe_alive(KEEPALIVE_PROBE_TIMEOUT).await;
                if alive {
                    interval = KEEPALIVE_BASE_INTERVAL;
                    offline_logged = false;
                    continue;
                }

                match session
                    .reconnect(
                        Some(gen),
                        Some(HostKeyPolicy::Strict),
                        false,
                        offline_logged,
                    )
                    .await
                {
                    Ok(_) => {
                        interval = KEEPALIVE_BASE_INTERVAL;
                        offline_logged = false;
                    }
                    Err(_) => {
                        if session.is_slot_closed().await {
                            break;
                        }
                        offline_logged = true;
                        interval = (interval.saturating_mul(2)).min(KEEPALIVE_MAX_BACKOFF);
                    }
                }
            }
        });
    }
}

type ManagedSftpSession = ReconnectableSession<SftpTransport>;

#[derive(Debug, Clone)]
pub(crate) struct SftpOpError {
    pub message: String,
    pub transport_lost: bool,
}

impl SftpOpError {
    fn into_command_string(self) -> String {
        format_sftp_command_error(self.message, self.transport_lost)
    }
}

pub(crate) fn format_sftp_command_error(msg: String, transport_lost: bool) -> String {
    let trimmed = msg.trim();
    if !transport_lost || (trimmed.starts_with('{') && trimmed.ends_with('}')) {
        return msg;
    }
    serde_json::json!({
        "error": msg,
        "transport_lost": true,
    })
    .to_string()
}

pub struct ActiveByteUpload {
    session_id: String,
    file: SftpRemoteFile,
    transfer_id: String,
}

#[derive(Default)]
pub struct SftpManager {
    sessions: Mutex<HashMap<String, Arc<ManagedSftpSession>>>,
    by_profile: Mutex<HashMap<String, String>>,
    uploads: Mutex<HashMap<String, Arc<Mutex<ActiveByteUpload>>>>,
}

impl SftpManager {
    pub(crate) async fn get_session(&self, id: &str) -> Result<Arc<ManagedSftpSession>, String> {
        if let Some(session) = self.sessions.lock().await.get(id).cloned() {
            return Ok(session);
        }

        let mapped_id = self.by_profile.lock().await.get(id).cloned();
        if let Some(sid) = mapped_id {
            if let Some(session) = self.sessions.lock().await.get(&sid).cloned() {
                return Ok(session);
            }
        }
        Err(format!("sftp session not connected: {id}"))
    }

    pub(crate) async fn get_entry_with_id(
        &self,
        id: &str,
    ) -> Result<(String, SftpTransport), String> {
        let session = self.get_session(id).await?;
        let (transport, _) = session.snapshot().await?;
        Ok((session.id.clone(), transport))
    }

    pub(crate) async fn run_with_reconnect<R, E, F, Fut>(
        &self,
        id: &str,
        is_transport_err: fn(&E) -> bool,
        op: F,
    ) -> Result<R, SftpOpError>
    where
        E: std::fmt::Display,
        F: Fn(SftpTransport) -> Fut,
        Fut: Future<Output = Result<R, E>>,
    {
        let session = self.get_session(id).await.map_err(|message| SftpOpError {
            message,
            transport_lost: false,
        })?;

        let (transport, gen) = session.ensure_live().await.map_err(|message| SftpOpError {
            transport_lost: !is_explicit_disconnect_error(&message),
            message,
        })?;

        match op(transport).await {
            Ok(val) => Ok(val),
            Err(err) => {
                if !is_transport_err(&err) {
                    return Err(SftpOpError {
                        message: err.to_string(),
                        transport_lost: false,
                    });
                }

                let (fresh_transport, _, _) = session
                    .reconnect(Some(gen), Some(HostKeyPolicy::Strict), false, false)
                    .await
                    .map_err(|re_err| SftpOpError {
                        transport_lost: !is_explicit_disconnect_error(&re_err),
                        message: format!("{err} (auto-reconnect failed: {re_err})"),
                    })?;

                op(fresh_transport).await.map_err(|err2| SftpOpError {
                    transport_lost: is_transport_err(&err2),
                    message: err2.to_string(),
                })
            }
        }
    }

    pub(crate) async fn run_sftp_op<R, F, Fut>(
        &self,
        id: &str,
        op: F,
    ) -> Result<R, String>
    where
        F: Fn(Arc<SftpSession>) -> Fut,
        Fut: Future<Output = Result<R, TransferError>>,
    {
        self.run_with_reconnect(id, TransferError::is_transport_error, move |t| {
            op(Arc::clone(&t.sftp))
        })
        .await
        .map_err(|e| e.message)
    }

    pub(crate) async fn connect_dedicated(
        &self,
        state: &AppState,
        profile: &Profile,
    ) -> Result<String, String> {
        let (transport, _) = dial_sftp_transport(
            state.secrets.as_ref(),
            &state.known_hosts_path,
            profile,
            Some(HostKeyPolicy::Strict),
        )
        .await?;
        let session_id = new_id();
        let dialer: Arc<dyn SessionDialer<SftpTransport>> = Arc::new(ProfileSftpDialer {
            profiles: Arc::clone(&state.profiles),
            secrets: Arc::clone(&state.secrets),
            known_hosts_path: state.known_hosts_path.clone(),
            profile_id: profile.id.clone(),
        });
        let managed = Arc::new(ReconnectableSession::new(
            session_id.clone(),
            RECONNECT_COOLDOWN,
            Arc::clone(&state.launcher_logs),
            dialer,
            transport,
        ));
        // Dedicated bridge sessions are never registered in `by_profile` so
        // user-opened SFTP tabs keep their own session mapping untouched.
        self.sessions
            .lock()
            .await
            .insert(session_id.clone(), Arc::clone(&managed));
        managed.start_keepalive_monitor();
        Ok(session_id)
    }

    pub(crate) async fn is_connected(&self, session_id: &str) -> bool {
        if session_id.is_empty() {
            return false;
        }
        let session = self.sessions.lock().await.get(session_id).cloned();
        match session {
            Some(s) => !s.is_slot_closed().await,
            None => false,
        }
    }

    pub(crate) async fn disconnect_session(&self, session_id: &str) {
        if session_id.is_empty() {
            return;
        }
        let session = self.sessions.lock().await.remove(session_id);
        {
            let mut by_profile = self.by_profile.lock().await;
            by_profile.retain(|_, v| v != session_id);
        }
        self.remove_uploads_for_session(session_id).await;
        if let Some(session) = session {
            session.close().await;
        }
    }

    pub async fn disconnect_all(&self) {
        let sessions: Vec<Arc<ManagedSftpSession>> = {
            let mut sessions = self.sessions.lock().await;
            let mut by_profile = self.by_profile.lock().await;
            by_profile.clear();
            sessions.drain().map(|(_, v)| v).collect()
        };
        let _ = self.uploads.lock().await.drain();
        for session in sessions {
            session.close().await;
        }
    }

    async fn remove_uploads_for_session(&self, session_id: &str) {
        let handles: Vec<(String, Arc<Mutex<ActiveByteUpload>>)> = {
            let guard = self.uploads.lock().await;
            guard
                .iter()
                .map(|(k, v)| (k.clone(), Arc::clone(v)))
                .collect()
        };
        let mut to_remove = Vec::new();
        for (upload_id, handle) in handles {
            if handle.lock().await.session_id == session_id {
                to_remove.push(upload_id);
            }
        }
        if !to_remove.is_empty() {
            let mut guard = self.uploads.lock().await;
            for upload_id in to_remove {
                guard.remove(&upload_id);
            }
        }
    }
}

fn is_explicit_disconnect_error(msg: &str) -> bool {
    msg.contains("sftp session not connected") || msg.contains("sftp session disconnected")
}

async fn dial_sftp_transport(
    secrets: &dyn SecretStore,
    known_hosts_path: &Path,
    profile: &Profile,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<(SftpTransport, String), String> {
    if profile.is_local() {
        return Err("local profiles do not support SFTP".into());
    }
    let creds = load_ssh_credentials_from(secrets, profile)?;
    let verifier = KnownHostsVerifier::new(
        known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );
    let connection = Arc::new(
        SshConnection::connect(profile, &creds, verifier)
            .await
            .map_err(format_ssh_error)?,
    );
    let sftp = Arc::new(connection.open_sftp().await.map_err(|e| e.to_string())?);
    let home = sftp
        .canonicalize(".")
        .await
        .unwrap_or_else(|_| "/".to_string());

    Ok((
        SftpTransport {
            connection,
            sftp,
            profile_id: profile.id.clone(),
            ssh_host: profile.ssh_host.trim().to_ascii_lowercase(),
            ssh_port: profile.ssh_port,
            ssh_user: profile.ssh_user.trim().to_string(),
        },
        home,
    ))
}

#[tauri::command]
pub async fn sftp_connect(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SftpConnectResponse, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    let (transport, home) = dial_sftp_transport(
        state.secrets.as_ref(),
        &state.known_hosts_path,
        &profile,
        host_key_policy,
    )
    .await?;

    let session_id = new_id();
    let dialer: Arc<dyn SessionDialer<SftpTransport>> = Arc::new(ProfileSftpDialer {
        profiles: Arc::clone(&state.profiles),
        secrets: Arc::clone(&state.secrets),
        known_hosts_path: state.known_hosts_path.clone(),
        profile_id: profile.id.clone(),
    });
    let managed = Arc::new(ReconnectableSession::new(
        session_id.clone(),
        RECONNECT_COOLDOWN,
        Arc::clone(&state.launcher_logs),
        dialer,
        transport,
    ));

    state
        .sftp
        .sessions
        .lock()
        .await
        .insert(session_id.clone(), Arc::clone(&managed));
    state
        .sftp
        .by_profile
        .lock()
        .await
        .insert(profile.id.clone(), session_id.clone());
    managed.start_keepalive_monitor();

    Ok(SftpConnectResponse {
        id: session_id,
        path: home,
    })
}

#[tauri::command]
pub async fn sftp_reconnect(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SftpConnectResponse, String> {
    if let Ok(existing) = state.sftp.get_session(&profile_id).await {
        let (_, _, home) = existing.reconnect(None, host_key_policy, true, false).await?;
        return Ok(SftpConnectResponse {
            id: existing.id.clone(),
            path: home,
        });
    }

    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    let (transport, home) = dial_sftp_transport(
        state.secrets.as_ref(),
        &state.known_hosts_path,
        &profile,
        host_key_policy,
    )
    .await?;

    let session_id = new_id();
    let dialer: Arc<dyn SessionDialer<SftpTransport>> = Arc::new(ProfileSftpDialer {
        profiles: Arc::clone(&state.profiles),
        secrets: Arc::clone(&state.secrets),
        known_hosts_path: state.known_hosts_path.clone(),
        profile_id: profile.id.clone(),
    });
    let managed = Arc::new(ReconnectableSession::new(
        session_id.clone(),
        RECONNECT_COOLDOWN,
        Arc::clone(&state.launcher_logs),
        dialer,
        transport,
    ));

    state
        .sftp
        .sessions
        .lock()
        .await
        .insert(session_id.clone(), Arc::clone(&managed));
    state
        .sftp
        .by_profile
        .lock()
        .await
        .insert(profile.id.clone(), session_id.clone());
    managed.start_keepalive_monitor();

    Ok(SftpConnectResponse {
        id: session_id,
        path: home,
    })
}

#[tauri::command]
pub async fn sftp_disconnect(state: State<'_, AppState>, profile_id: String) -> Result<(), String> {
    let sid = {
        let sessions = state.sftp.sessions.lock().await;
        if sessions.contains_key(&profile_id) {
            profile_id.clone()
        } else {
            state
                .sftp
                .by_profile
                .lock()
                .await
                .get(&profile_id)
                .cloned()
                .unwrap_or_else(|| profile_id.clone())
        }
    };

    state.sftp_sync.unregister_session(&sid).await;
    state.sftp.disconnect_session(&sid).await;
    Ok(())
}

#[tauri::command]
pub async fn sftp_list(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<Vec<FsEntry>, String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let p = path.clone();
            async move { list_remote_dir(&t.sftp, &p).await }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_realpath(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<String, String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let p = path.clone();
            async move {
                t.sftp
                    .canonicalize(&p)
                    .await
                    .map_err(|e| sftp_err(format!("realpath {p}"), e))
            }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_mkdir(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<(), String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let p = path.clone();
            async move { mkdir_remote_recursive(&t.sftp, &p).await }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_mkdirs(
    state: State<'_, AppState>,
    profile_id: String,
    paths: Vec<String>,
) -> Result<(), String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let ps = paths.clone();
            async move {
                for p in &ps {
                    mkdir_remote_recursive(&t.sftp, p).await?;
                }
                Ok(())
            }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

async fn mkdir_remote_recursive(sftp: &SftpSession, path: &str) -> Result<(), TransferError> {
    let trimmed = path.trim();
    if trimmed.is_empty() || trimmed == "/" || trimmed == "." {
        return Ok(());
    }
    match sftp.try_exists(trimmed).await {
        Ok(true) => return Ok(()),
        Ok(false) => {}
        Err(err) => return Err(sftp_err(format!("stat {trimmed}"), err)),
    }
    let is_abs = trimmed.starts_with('/');
    let mut cur = String::new();
    for seg in trimmed.split('/').filter(|s| !s.is_empty()) {
        if is_abs || !cur.is_empty() {
            cur.push('/');
        }
        cur.push_str(seg);
        match sftp.try_exists(&cur).await {
            Ok(true) => continue,
            Ok(false) => {}
            Err(err) => return Err(sftp_err(format!("stat {cur}"), err)),
        }
        if let Err(err) = sftp.create_dir(&cur).await {
            if is_sftp_transport_error(&err) {
                return Err(sftp_err(format!("mkdir {cur}"), err));
            }
            match sftp.try_exists(&cur).await {
                Ok(true) => continue,
                Ok(false) => return Err(sftp_err(format!("mkdir {cur}"), err)),
                Err(stat_err) => return Err(sftp_err(format!("stat {cur}"), stat_err)),
            }
        }
    }
    Ok(())
}

#[tauri::command]
pub async fn sftp_create_file(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<(), String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let p = path.clone();
            async move {
                let _file = t
                    .sftp
                    .open_with_flags(&p, OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::EXCLUDE)
                    .await
                    .map_err(|e| sftp_err(format!("create {p}"), e))?;
                Ok(())
            }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_rename(
    state: State<'_, AppState>,
    profile_id: String,
    from: String,
    to: String,
) -> Result<(), String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let src = from.clone();
            let dst = to.clone();
            async move {
                t.sftp
                    .rename(&src, &dst)
                    .await
                    .map_err(|e| sftp_err(format!("rename {src} -> {dst}"), e))
            }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_delete(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
    is_dir: Option<bool>,
) -> Result<(), String> {
    let _ = is_dir;
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let p = path.clone();
            async move { delete_remote_path(&t.sftp, &p).await }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

pub(crate) fn sftp_open_cache_path(
    cache_root: &Path,
    session_id: &str,
    remote_path: &str,
) -> Result<PathBuf, String> {
    let mut hasher = Sha256::new();
    hasher.update(session_id.as_bytes());
    hasher.update(b"\n");
    hasher.update(remote_path.as_bytes());
    let digest = hasher.finalize();
    let bucket: String = digest[..8].iter().map(|b| format!("{b:02x}")).collect();

    let raw_name = remote_path
        .rsplit('/')
        .next()
        .unwrap_or("")
        .trim();
    let safe_name = if raw_name.is_empty() || raw_name == "." || raw_name == ".." {
        "remote-file".to_string()
    } else {
        sanitize_filename_segment(raw_name)
    };
    Ok(cache_root.join(bucket).join(safe_name))
}

async fn cache_sftp_file_for_open_inner(
    state: &AppState,
    session_id: &str,
    remote_path: &str,
) -> Result<PathBuf, SftpOpError> {
    let resolved_sid = {
        let session = state
            .sftp
            .get_session(session_id)
            .await
            .map_err(|message| SftpOpError {
                message,
                transport_lost: false,
            })?;
        session.id.clone()
    };

    let local_path = sftp_open_cache_path(&state.sftp_cache_dir, &resolved_sid, remote_path)
        .map_err(|message| SftpOpError {
            message,
            transport_lost: false,
        })?;
    if let Some(parent) = local_path.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| SftpOpError {
                message: format!("create cache dir: {e}"),
                transport_lost: false,
            })?;
    }

    let rpath = remote_path.to_string();
    let lpath = local_path.clone();
    let transfers = Arc::clone(&state.transfers);

    state
        .sftp
        .run_with_reconnect(session_id, TransferError::is_transport_error, move |t| {
            let rpath = rpath.clone();
            let lpath = lpath.clone();
            let transfers = Arc::clone(&transfers);
            async move {
                let meta = t
                    .sftp
                    .metadata(&rpath)
                    .await
                    .map_err(|e| sftp_err(format!("stat {rpath}"), e))?;
                if meta.is_dir() {
                    return Err(TransferError::InvalidInput(
                        "cannot open a directory as a file".into(),
                    ));
                }
                let size = meta.size.unwrap_or(0);
                if size > MAX_SYNC_FILE_BYTES {
                    return Err(TransferError::InvalidInput(format!(
                        "file is too large to open with sync (limit 100 MB): {size} bytes"
                    )));
                }

                let transfer_id = format!("open-{}", new_id());
                download_file_resumable_once(
                    &t.sftp,
                    &rpath,
                    &lpath,
                    &transfer_id,
                    false,
                    &transfers,
                )
                .await
            }
        })
        .await?;

    Ok(local_path)
}

pub(crate) async fn cache_sftp_file_for_open(
    state: &AppState,
    session_id: &str,
    remote_path: &str,
) -> Result<PathBuf, String> {
    cache_sftp_file_for_open_inner(state, session_id, remote_path)
        .await
        .map_err(|err| err.message)
}

#[tauri::command]
pub async fn sftp_open_file(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
    as_text: Option<bool>,
) -> Result<SftpOpenFileResponse, String> {
    let (session_id, _) = state.sftp.get_entry_with_id(&profile_id).await?;
    let local_path = cache_sftp_file_for_open_inner(&state, &session_id, &path)
        .await
        .map_err(SftpOpError::into_command_string)?;

    state
        .sftp_sync
        .register(
            Arc::clone(&state.sftp),
            Arc::clone(&state.launcher_logs),
            session_id,
            path,
            local_path.clone(),
        )
        .await;

    if as_text.unwrap_or(false) {
        open_text_file_in_system(&local_path)?;
    } else {
        open_path_in_system(&local_path)?;
    }

    Ok(SftpOpenFileResponse {
        path: local_path.display().to_string(),
    })
}

#[tauri::command]
pub async fn sftp_open_sync_events(
    state: State<'_, AppState>,
    after: Option<u64>,
) -> Result<OpenSyncEventsResponse, String> {
    let events = state.sftp_sync.events_after(after.unwrap_or(0)).await;
    Ok(OpenSyncEventsResponse { events })
}

#[tauri::command]
pub async fn sftp_server_copy_support(
    state: State<'_, AppState>,
    source: String,
    target: String,
) -> Result<ServerCopySupportResponse, String> {
    let supported = match (
        state.sftp.get_entry_with_id(&source).await,
        state.sftp.get_entry_with_id(&target).await,
    ) {
        (Ok((_, s)), Ok((_, t))) => is_same_remote_host(&s, &t),
        _ => false,
    };
    Ok(ServerCopySupportResponse { supported })
}

fn is_exec_transport_error(msg: &str) -> bool {
    !msg.contains("remote command execution rejected")
}

#[tauri::command]
pub async fn sftp_copy_on_server(
    state: State<'_, AppState>,
    source_session: String,
    target_session: String,
    target_path: String,
    items: Vec<ServerCopyItem>,
) -> Result<ServerCopyResponse, String> {
    let src_entry = state.sftp.get_entry_with_id(&source_session).await;
    let dst_entry = state.sftp.get_entry_with_id(&target_session).await;
    let (Ok((_, src)), Ok((_, dst))) = (src_entry, dst_entry) else {
        return Ok(ServerCopyResponse {
            supported: false,
            results: Vec::new(),
        });
    };
    if !is_same_remote_host(&src, &dst) {
        return Ok(ServerCopyResponse {
            supported: false,
            results: Vec::new(),
        });
    }

    let mut results = Vec::with_capacity(items.len());
    let base = target_path.trim_end_matches('/');
    for item in items {
        let rel = item.relative_path.trim_start_matches('/');
        if rel.is_empty()
            || rel.split('/').any(|s| s.is_empty() || s == "." || s == "..")
        {
            results.push(ServerCopyItemResult {
                source_path: item.source_path,
                ok: false,
                error: Some("unsafe relative target path".into()),
            });
            continue;
        }
        let dst_item_path = if base.is_empty() {
            format!("/{rel}")
        } else {
            format!("{base}/{rel}")
        };
        let parent_dir = dst_item_path
            .rsplit_once('/')
            .map(|(p, _)| if p.is_empty() { "/" } else { p })
            .unwrap_or("/");
        let cmd = format!(
            "mkdir -p -- {} && cp -R -P -- {} {}",
            shell_quote(parent_dir),
            shell_quote(&item.source_path),
            shell_quote(&dst_item_path)
        );
        let exec_res = state
            .sftp
            .run_with_reconnect(
                &target_session,
                |e: &TransferError| e.is_transport_error(),
                |t| {
                    let cmd = cmd.clone();
                    async move {
                        t.connection.exec_command(&cmd).await.map_err(|e| {
                            let msg = e.to_string();
                            if is_exec_transport_error(&msg) {
                                TransferError::SftpTransport(msg)
                            } else {
                                TransferError::Archive(msg)
                            }
                        })
                    }
                },
            )
            .await;

        match exec_res {
            Ok(out) if out.exit_code == Some(0) => {
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: true,
                    error: None,
                });
            }
            Ok(out) => {
                let err = out.stderr.trim();
                let msg = if err.is_empty() {
                    format!("cp exited with code {:?}", out.exit_code)
                } else {
                    err.to_string()
                };
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: false,
                    error: Some(msg),
                });
            }
            Err(err) => {
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: false,
                    error: Some(err.message),
                });
            }
        }
    }

    Ok(ServerCopyResponse {
        supported: true,
        results,
    })
}

fn is_same_remote_host(a: &SftpTransport, b: &SftpTransport) -> bool {
    a.profile_id == b.profile_id
        || (a.ssh_host == b.ssh_host && a.ssh_port == b.ssh_port && a.ssh_user == b.ssh_user)
}

fn sanitize_filename_segment(seg: &str) -> String {
    seg.chars()
        .map(|c| {
            if matches!(c, '<' | '>' | ':' | '"' | '/' | '\\' | '|' | '?' | '*') {
                '_'
            } else {
                c
            }
        })
        .collect()
}

#[tauri::command]
pub async fn sftp_upload_local(
    state: State<'_, AppState>,
    profile_id: String,
    local_path: String,
    remote_path: String,
    transfer_id: String,
    resume: Option<bool>,
) -> Result<(), String> {
    let session = state.sftp.get_session(&profile_id).await?;
    let (transport, gen) = session.ensure_live().await.map_err(|msg| {
        let out = format_sftp_command_error(msg.clone(), !is_explicit_disconnect_error(&msg));
        state.transfers.finish(&transfer_id, Some(msg));
        out
    })?;

    let local = PathBuf::from(&local_path);
    match upload_file_resumable_once(
        &transport.sftp,
        &local,
        &remote_path,
        &transfer_id,
        resume.unwrap_or(false),
        &state.transfers,
    )
    .await
    {
        Ok(()) => Ok(()),
        Err(err) if err.is_transport_error() && !state.transfers.is_cancelled(&transfer_id) => {
            let (fresh_transport, _, _) = session
                .reconnect(Some(gen), Some(HostKeyPolicy::Strict), false, false)
                .await
                .map_err(|re_err| {
                    let msg = format!("{err} (auto-reconnect failed: {re_err})");
                    state.transfers.finish(&transfer_id, Some(msg.clone()));
                    format_sftp_command_error(msg, !is_explicit_disconnect_error(&re_err))
                })?;

            match upload_file_resumable_once(
                &fresh_transport.sftp,
                &local,
                &remote_path,
                &transfer_id,
                true,
                &state.transfers,
            )
            .await
            {
                Ok(()) => Ok(()),
                Err(err2) => {
                    let is_transport = err2.is_transport_error();
                    let msg = err2.to_string();
                    if is_transport {
                        state.transfers.finish(&transfer_id, Some(msg.clone()));
                    }
                    Err(format_sftp_command_error(msg, is_transport))
                }
            }
        }
        Err(err) => {
            let is_transport = err.is_transport_error();
            let msg = err.to_string();
            if is_transport {
                state.transfers.finish(&transfer_id, Some(msg.clone()));
            }
            Err(format_sftp_command_error(msg, is_transport))
        }
    }
}

#[tauri::command]
pub async fn sftp_download_local(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    local_path: String,
    transfer_id: String,
    resume: Option<bool>,
) -> Result<(), String> {
    let session = state.sftp.get_session(&profile_id).await?;
    let (transport, gen) = session.ensure_live().await.map_err(|msg| {
        let out = format_sftp_command_error(msg.clone(), !is_explicit_disconnect_error(&msg));
        state.transfers.finish(&transfer_id, Some(msg));
        out
    })?;

    let local = PathBuf::from(&local_path);
    match download_file_resumable_once(
        &transport.sftp,
        &remote_path,
        &local,
        &transfer_id,
        resume.unwrap_or(false),
        &state.transfers,
    )
    .await
    {
        Ok(()) => Ok(()),
        Err(err) if err.is_transport_error() && !state.transfers.is_cancelled(&transfer_id) => {
            let (fresh_transport, _, _) = session
                .reconnect(Some(gen), Some(HostKeyPolicy::Strict), false, false)
                .await
                .map_err(|re_err| {
                    let msg = format!("{err} (auto-reconnect failed: {re_err})");
                    state.transfers.finish(&transfer_id, Some(msg.clone()));
                    format_sftp_command_error(msg, !is_explicit_disconnect_error(&re_err))
                })?;

            match download_file_resumable_once(
                &fresh_transport.sftp,
                &remote_path,
                &local,
                &transfer_id,
                true,
                &state.transfers,
            )
            .await
            {
                Ok(()) => Ok(()),
                Err(err2) => {
                    let is_transport = err2.is_transport_error();
                    let msg = err2.to_string();
                    if is_transport {
                        state.transfers.finish(&transfer_id, Some(msg.clone()));
                    }
                    Err(format_sftp_command_error(msg, is_transport))
                }
            }
        }
        Err(err) => {
            let is_transport = err.is_transport_error();
            let msg = err.to_string();
            if is_transport {
                state.transfers.finish(&transfer_id, Some(msg.clone()));
            }
            Err(format_sftp_command_error(msg, is_transport))
        }
    }
}

#[tauri::command]
pub async fn sftp_upload_bytes_begin(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    transfer_id: Option<String>,
    total_size: Option<u64>,
) -> Result<UploadBytesBeginResponse, String> {
    let (resolved_sid, _) = state.sftp.get_entry_with_id(&profile_id).await?;
    let tid = transfer_id
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| format!("upload-{}", new_id()));
    let _ = state.transfers.begin(&tid, total_size.unwrap_or(0), false);

    let rpath = remote_path.clone();
    let file = match state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let rpath = rpath.clone();
            async move {
                if let Some((parent, _)) = rpath.rsplit_once('/') {
                    if !parent.is_empty() {
                        mkdir_remote_recursive(&t.sftp, parent).await?;
                    }
                }
                t.sftp
                    .open_with_flags(
                        &rpath,
                        OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
                    )
                    .await
                    .map_err(|e| sftp_err(format!("open remote {rpath}"), e))
            }
        })
        .await
    {
        Ok(f) => f,
        Err(err) => {
            state.transfers.finish(&tid, Some(err.message.clone()));
            return Err(err.into_command_string());
        }
    };

    let upload_id = new_id();
    state.sftp.uploads.lock().await.insert(
        upload_id.clone(),
        Arc::new(Mutex::new(ActiveByteUpload {
            session_id: resolved_sid,
            file,
            transfer_id: tid,
        })),
    );
    Ok(UploadBytesBeginResponse { upload_id })
}

#[tauri::command]
pub async fn sftp_upload_bytes_chunk(
    state: State<'_, AppState>,
    upload_id: String,
    chunk_b64: String,
) -> Result<(), String> {
    let bytes = BASE64
        .decode(chunk_b64.as_bytes())
        .map_err(|e| format!("invalid base64 chunk: {e}"))?;
    let handle = state
        .sftp
        .uploads
        .lock()
        .await
        .get(&upload_id)
        .cloned()
        .ok_or_else(|| format!("upload handle not found: {upload_id}"))?;

    let mut guard = handle.lock().await;
    if let Err(err) = state.transfers.wait_if_paused(&guard.transfer_id).await {
        state
            .transfers
            .finish(&guard.transfer_id, Some(err.to_string()));
        return Err(err.to_string());
    }
    if let Err(err) = guard.file.write_all(&bytes).await {
        let te = remote_io_err("write remote chunk", err);
        let msg = te.to_string();
        state.transfers.finish(&guard.transfer_id, Some(msg.clone()));
        return Err(format_sftp_command_error(msg, te.is_transport_error()));
    }
    state
        .transfers
        .add_loaded(&guard.transfer_id, bytes.len() as u64);
    Ok(())
}

#[tauri::command]
pub async fn sftp_upload_bytes_finish(
    state: State<'_, AppState>,
    upload_id: String,
    abort: Option<bool>,
) -> Result<(), String> {
    let handle = state
        .sftp
        .uploads
        .lock()
        .await
        .remove(&upload_id)
        .ok_or_else(|| format!("upload handle not found: {upload_id}"))?;

    let mut guard = handle.lock().await;
    if abort.unwrap_or(false) {
        state
            .transfers
            .finish(&guard.transfer_id, Some("upload aborted".into()));
        return Ok(());
    }
    if let Err(err) = guard.file.shutdown().await {
        let te = remote_io_err("finalize remote file", err);
        let msg = te.to_string();
        state.transfers.finish(&guard.transfer_id, Some(msg.clone()));
        return Err(format_sftp_command_error(msg, te.is_transport_error()));
    }
    state.transfers.finish(&guard.transfer_id, None);
    Ok(())
}

#[tauri::command]
pub async fn sftp_download_bytes(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    max_bytes: Option<u64>,
) -> Result<DownloadBytesResponse, String> {
    let limit = max_bytes.unwrap_or(DEFAULT_DOWNLOAD_BYTES_LIMIT);
    let rpath = remote_path.clone();
    let data = state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let rpath = rpath.clone();
            async move {
                let meta = t
                    .sftp
                    .metadata(&rpath)
                    .await
                    .map_err(|e| sftp_err(format!("stat {rpath}"), e))?;
                if meta.is_dir() {
                    return Err(TransferError::InvalidInput(
                        "cannot download a directory directly as bytes".into(),
                    ));
                }
                let expected_size = meta.size.unwrap_or(0);
                if expected_size > limit {
                    return Err(TransferError::InvalidInput(format!(
                        "remote file size ({expected_size} bytes) exceeds browser download limit ({limit} bytes); please transfer into a local folder instead"
                    )));
                }

                let mut file = t
                    .sftp
                    .open(&rpath)
                    .await
                    .map_err(|e| sftp_err(format!("open {rpath}"), e))?;
                let mut data = Vec::with_capacity(expected_size.min(4 * 1024 * 1024) as usize);
                let mut buf = vec![0u8; DOWNLOAD_CHUNK_SIZE];
                loop {
                    let n = file
                        .read(&mut buf)
                        .await
                        .map_err(|e| remote_io_err(format!("read {rpath}"), e))?;
                    if n == 0 {
                        break;
                    }
                    if (data.len() as u64).saturating_add(n as u64) > limit {
                        return Err(TransferError::InvalidInput(format!(
                            "remote file exceeded browser download limit ({limit} bytes)"
                        )));
                    }
                    data.extend_from_slice(&buf[..n]);
                }
                Ok(data)
            }
        })
        .await
        .map_err(SftpOpError::into_command_string)?;

    let name = remote_path
        .rsplit('/')
        .next()
        .filter(|s| !s.is_empty())
        .unwrap_or("download")
        .to_string();
    let size = data.len() as u64;
    Ok(DownloadBytesResponse {
        name,
        data_b64: BASE64.encode(&data),
        size,
    })
}

#[tauri::command]
pub async fn sftp_copy_remote(
    state: State<'_, AppState>,
    src_profile_id: String,
    src_path: String,
    dst_profile_id: String,
    dst_path: String,
    transfer_id: String,
) -> Result<(), String> {
    let src_session = state.sftp.get_session(&src_profile_id).await?;
    let dst_session = state.sftp.get_session(&dst_profile_id).await?;

    let (src_t, src_gen) = src_session.ensure_live().await.map_err(|msg| {
        let out = format_sftp_command_error(msg.clone(), !is_explicit_disconnect_error(&msg));
        state.transfers.finish(&transfer_id, Some(msg));
        out
    })?;
    let (dst_t, dst_gen) = dst_session.ensure_live().await.map_err(|msg| {
        let out = format_sftp_command_error(msg.clone(), !is_explicit_disconnect_error(&msg));
        state.transfers.finish(&transfer_id, Some(msg));
        out
    })?;

    match copy_remote_to_remote_once(
        &src_t.sftp,
        &src_path,
        &dst_t.sftp,
        &dst_path,
        &transfer_id,
        &state.transfers,
    )
    .await
    {
        Ok(()) => Ok(()),
        Err(err) if err.is_transport_error() && !state.transfers.is_cancelled(&transfer_id) => {
            let src_dead =
                src_t.is_closed() || !src_t.probe_alive(COPY_REMOTE_PROBE_TIMEOUT).await;
            let dst_dead =
                dst_t.is_closed() || !dst_t.probe_alive(COPY_REMOTE_PROBE_TIMEOUT).await;
            let (reconnect_src, reconnect_dst) = if !src_dead && !dst_dead {
                (true, true)
            } else {
                (src_dead, dst_dead)
            };

            let next_src = if reconnect_src {
                src_session
                    .reconnect(Some(src_gen), Some(HostKeyPolicy::Strict), false, false)
                    .await
                    .map(|(t, _, _)| t)
            } else {
                Ok(src_t)
            };
            let new_src = match next_src {
                Ok(t) => t,
                Err(re_err) => {
                    let msg = format!("{err} (source auto-reconnect failed: {re_err})");
                    state.transfers.finish(&transfer_id, Some(msg.clone()));
                    return Err(format_sftp_command_error(
                        msg,
                        !is_explicit_disconnect_error(&re_err),
                    ));
                }
            };

            let next_dst = if reconnect_dst {
                dst_session
                    .reconnect(Some(dst_gen), Some(HostKeyPolicy::Strict), false, false)
                    .await
                    .map(|(t, _, _)| t)
            } else {
                Ok(dst_t)
            };
            let new_dst = match next_dst {
                Ok(t) => t,
                Err(re_err) => {
                    let msg = format!("{err} (target auto-reconnect failed: {re_err})");
                    state.transfers.finish(&transfer_id, Some(msg.clone()));
                    return Err(format_sftp_command_error(
                        msg,
                        !is_explicit_disconnect_error(&re_err),
                    ));
                }
            };

            match copy_remote_to_remote_once(
                &new_src.sftp,
                &src_path,
                &new_dst.sftp,
                &dst_path,
                &transfer_id,
                &state.transfers,
            )
            .await
            {
                Ok(()) => Ok(()),
                Err(err2) => {
                    let is_transport = err2.is_transport_error();
                    let msg = err2.to_string();
                    if is_transport {
                        state.transfers.finish(&transfer_id, Some(msg.clone()));
                    }
                    Err(format_sftp_command_error(msg, is_transport))
                }
            }
        }
        Err(err) => {
            let is_transport = err.is_transport_error();
            let msg = err.to_string();
            if is_transport {
                state.transfers.finish(&transfer_id, Some(msg.clone()));
            }
            Err(format_sftp_command_error(msg, is_transport))
        }
    }
}

#[tauri::command]
pub async fn sftp_archive_tools(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<ArchiveTools, String> {
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| async move {
            detect_remote_tools(&t.connection).await
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_compress(
    state: State<'_, AppState>,
    profile_id: String,
    cwd: String,
    format: String,
    archive_name: String,
    entries: Vec<String>,
) -> Result<(), String> {
    let step = build_compress_step(&format, &archive_name, &entries).map_err(|e| e.to_string())?;
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let cwd = cwd.clone();
            let step = step.clone();
            async move { run_remote_step(&t.connection, &cwd, &step).await }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[tauri::command]
pub async fn sftp_extract(
    state: State<'_, AppState>,
    profile_id: String,
    cwd: String,
    archive_name: String,
    dest_dir: String,
) -> Result<(), String> {
    let step = build_extract_step(&archive_name, &dest_dir).map_err(|e| e.to_string())?;
    state
        .sftp
        .run_with_reconnect(&profile_id, TransferError::is_transport_error, |t| {
            let cwd = cwd.clone();
            let step = step.clone();
            async move { run_remote_step(&t.connection, &cwd, &step).await }
        })
        .await
        .map_err(SftpOpError::into_command_string)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    #[derive(Clone)]
    struct FakeTransport {
        id: usize,
        closed: Arc<AtomicBool>,
        probe_ok: Arc<AtomicBool>,
        reopen_result: Arc<Mutex<Option<(usize, bool)>>>,
        disconnected_count: Arc<AtomicUsize>,
    }

    impl FakeTransport {
        fn new(id: usize, closed: bool, probe_ok: bool) -> Self {
            Self {
                id,
                closed: Arc::new(AtomicBool::new(closed)),
                probe_ok: Arc::new(AtomicBool::new(probe_ok)),
                reopen_result: Arc::new(Mutex::new(None)),
                disconnected_count: Arc::new(AtomicUsize::new(0)),
            }
        }
    }

    impl SessionTransport for FakeTransport {
        fn is_closed(&self) -> bool {
            self.closed.load(Ordering::SeqCst)
        }

        fn probe_alive<'a>(
            &'a self,
            _timeout: Duration,
        ) -> Pin<Box<dyn Future<Output = bool> + Send + 'a>> {
            Box::pin(async move {
                !self.closed.load(Ordering::SeqCst) && self.probe_ok.load(Ordering::SeqCst)
            })
        }

        fn try_reopen_sftp_channel<'a>(
            &'a self,
        ) -> Pin<Box<dyn Future<Output = Option<(Self, String)>> + Send + 'a>> {
            Box::pin(async move {
                let guard = self.reopen_result.lock().await;
                guard.map(|(new_id_val, ok)| {
                    (
                        FakeTransport::new(new_id_val, !ok, ok),
                        "/home/user".to_string(),
                    )
                })
            })
        }

        fn disconnect_async(self) {
            self.closed.store(true, Ordering::SeqCst);
            self.disconnected_count.fetch_add(1, Ordering::SeqCst);
        }
    }

    struct FakeDialer {
        dial_count: Arc<AtomicUsize>,
        delay: Duration,
        next_transport: Arc<Mutex<Option<Result<FakeTransport, String>>>>,
    }

    impl FakeDialer {
        fn succeeding(delay: Duration) -> Self {
            Self {
                dial_count: Arc::new(AtomicUsize::new(0)),
                delay,
                next_transport: Arc::new(Mutex::new(None)),
            }
        }
    }

    impl SessionDialer<FakeTransport> for FakeDialer {
        fn dial<'a>(
            &'a self,
            _policy: Option<HostKeyPolicy>,
        ) -> Pin<Box<dyn Future<Output = Result<(FakeTransport, String), String>> + Send + 'a>>
        {
            Box::pin(async move {
                let count = self.dial_count.fetch_add(1, Ordering::SeqCst) + 1;
                if !self.delay.is_zero() {
                    tokio::time::sleep(self.delay).await;
                }
                if let Some(custom) = self.next_transport.lock().await.take() {
                    return custom.map(|t| (t, "/home/user".to_string()));
                }
                Ok((
                    FakeTransport::new(count + 100, false, true),
                    "/home/user".to_string(),
                ))
            })
        }
    }

    async fn run_fake_with_reconnect<R, F, Fut>(
        session: &ReconnectableSession<FakeTransport>,
        op: F,
    ) -> Result<R, SftpOpError>
    where
        F: Fn(FakeTransport) -> Fut,
        Fut: Future<Output = Result<R, TransferError>>,
    {
        let (t, gen) = session.ensure_live().await.map_err(|message| SftpOpError {
            transport_lost: !is_explicit_disconnect_error(&message),
            message,
        })?;
        match op(t).await {
            Ok(val) => Ok(val),
            Err(err) if err.is_transport_error() => {
                let (fresh, _, _) = session
                    .reconnect(Some(gen), Some(HostKeyPolicy::Strict), false, false)
                    .await
                    .map_err(|re_err| SftpOpError {
                        transport_lost: !is_explicit_disconnect_error(&re_err),
                        message: format!("{err} (auto-reconnect failed: {re_err})"),
                    })?;
                op(fresh).await.map_err(|e2| SftpOpError {
                    transport_lost: e2.is_transport_error(),
                    message: e2.to_string(),
                })
            }
            Err(err) => Err(SftpOpError {
                transport_lost: false,
                message: err.to_string(),
            }),
        }
    }

    #[tokio::test]
    async fn auto_reconnects_on_transport_error_and_retries_once() {
        let initial = FakeTransport::new(1, false, false);
        let dialer = Arc::new(FakeDialer::succeeding(Duration::ZERO));
        let session = ReconnectableSession::new(
            "s-auto".into(),
            Duration::ZERO,
            Arc::new(LauncherLogStore::default()),
            dialer.clone(),
            initial,
        );

        let attempts = Arc::new(AtomicUsize::new(0));
        let attempts_c = Arc::clone(&attempts);
        let res = run_fake_with_reconnect(&session, move |t| {
            let attempts_c = Arc::clone(&attempts_c);
            async move {
                attempts_c.fetch_add(1, Ordering::SeqCst);
                if t.id == 1 {
                    Err(TransferError::SftpTransport(
                        "list /home/user: session closed".into(),
                    ))
                } else {
                    Ok(t.id)
                }
            }
        })
        .await
        .unwrap();

        assert_eq!(res, 101);
        assert_eq!(attempts.load(Ordering::SeqCst), 2);
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn proactively_reconnects_when_transport_already_closed() {
        let initial = FakeTransport::new(1, true, false);
        let dialer = Arc::new(FakeDialer::succeeding(Duration::ZERO));
        let session = ReconnectableSession::new(
            "s-proactive".into(),
            Duration::ZERO,
            Arc::new(LauncherLogStore::default()),
            dialer.clone(),
            initial,
        );

        let attempts = Arc::new(AtomicUsize::new(0));
        let attempts_c = Arc::clone(&attempts);
        let res = run_fake_with_reconnect(&session, move |t| {
            let attempts_c = Arc::clone(&attempts_c);
            async move {
                attempts_c.fetch_add(1, Ordering::SeqCst);
                Ok(t.id)
            }
        })
        .await
        .unwrap();

        assert_eq!(res, 101);
        assert_eq!(attempts.load(Ordering::SeqCst), 1);
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn does_not_retry_or_reconnect_on_server_status_error() {
        let initial = FakeTransport::new(1, false, true);
        let dialer = Arc::new(FakeDialer::succeeding(Duration::ZERO));
        let session = ReconnectableSession::new(
            "s-status".into(),
            Duration::ZERO,
            Arc::new(LauncherLogStore::default()),
            dialer.clone(),
            initial,
        );

        let attempts = Arc::new(AtomicUsize::new(0));
        let attempts_c = Arc::clone(&attempts);
        let err = run_fake_with_reconnect(&session, move |_| {
            let attempts_c = Arc::clone(&attempts_c);
            async move {
                attempts_c.fetch_add(1, Ordering::SeqCst);
                Err::<(), TransferError>(TransferError::Sftp("permission denied".into()))
            }
        })
        .await
        .unwrap_err();

        assert!(!err.transport_lost);
        assert_eq!(attempts.load(Ordering::SeqCst), 1);
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn deduplicates_concurrent_reconnects_by_generation() {
        let initial = FakeTransport::new(1, true, false);
        let dialer = Arc::new(FakeDialer::succeeding(Duration::from_millis(30)));
        let session = Arc::new(ReconnectableSession::new(
            "s-dedup".into(),
            Duration::from_secs(10),
            Arc::new(LauncherLogStore::default()),
            dialer.clone(),
            initial,
        ));

        let s1 = Arc::clone(&session);
        let s2 = Arc::clone(&session);
        let (r1, r2) = tokio::join!(
            s1.reconnect(Some(1), Some(HostKeyPolicy::Strict), false, false),
            s2.reconnect(Some(1), Some(HostKeyPolicy::Strict), false, false)
        );

        let (t1, g1, _) = r1.unwrap();
        let (t2, g2, _) = r2.unwrap();
        assert_eq!(g1, 2);
        assert_eq!(g2, 2);
        assert_eq!(t1.id, 101);
        assert_eq!(t2.id, 101);
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn disconnect_during_reconnect_closes_new_connection_and_does_not_resurrect() {
        let initial = FakeTransport::new(1, true, false);
        let newly_dialed = FakeTransport::new(202, false, true);
        let new_closed_flag = Arc::clone(&newly_dialed.closed);
        let dialer = Arc::new(FakeDialer {
            dial_count: Arc::new(AtomicUsize::new(0)),
            delay: Duration::from_millis(60),
            next_transport: Arc::new(Mutex::new(Some(Ok(newly_dialed)))),
        });
        let session = Arc::new(ReconnectableSession::new(
            "s-close".into(),
            Duration::ZERO,
            Arc::new(LauncherLogStore::default()),
            dialer,
            initial,
        ));

        let s1 = Arc::clone(&session);
        let re_task = tokio::spawn(async move {
            s1.reconnect(Some(1), Some(HostKeyPolicy::Strict), false, false)
                .await
        });

        tokio::time::sleep(Duration::from_millis(20)).await;
        session.close().await;

        let re_res = re_task.await.unwrap();
        assert!(re_res.is_err());
        assert!(new_closed_flag.load(Ordering::SeqCst));
        assert!(session.is_slot_closed().await);
        assert!(session.snapshot().await.is_err());
    }

    #[tokio::test]
    async fn cooldown_limits_auto_reconnect_while_force_bypasses() {
        let initial = FakeTransport::new(1, true, false);
        let dialer = Arc::new(FakeDialer::succeeding(Duration::ZERO));
        let session = ReconnectableSession::new(
            "s-cooldown".into(),
            Duration::from_secs(60),
            Arc::new(LauncherLogStore::default()),
            dialer.clone(),
            initial,
        );

        let (first, g1, _) = session
            .reconnect(Some(1), Some(HostKeyPolicy::Strict), false, false)
            .await
            .unwrap();
        assert_eq!(first.id, 101);
        assert_eq!(g1, 2);

        // Second auto-reconnect immediately within cooldown is rate-limited.
        first.closed.store(true, Ordering::SeqCst);
        let err = session
            .reconnect(Some(g1), Some(HostKeyPolicy::Strict), false, false)
            .await
            .err()
            .unwrap();
        assert!(err.contains("rate limited"));
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 1);

        // Explicit manual reconnect (force=true) bypasses cooldown.
        let (forced, gen, _) = session
            .reconnect(None, Some(HostKeyPolicy::Strict), true, false)
            .await
            .unwrap();
        assert_eq!(forced.id, 102);
        assert_eq!(gen, 3);
        assert_eq!(dialer.dial_count.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn format_sftp_command_error_marks_transport_lost_without_corrupting_json() {
        let marked = format_sftp_command_error("sftp error: list /home/user: session closed".into(), true);
        let parsed: serde_json::Value = serde_json::from_str(&marked).unwrap();
        assert_eq!(
            parsed["error"].as_str(),
            Some("sftp error: list /home/user: session closed")
        );
        assert_eq!(parsed["transport_lost"].as_bool(), Some(true));

        let host_key_json = r#"{"error":"host key mismatch","host_key":{"host":"example.com"}}"#;
        assert_eq!(
            format_sftp_command_error(host_key_json.into(), true),
            host_key_json
        );

        assert_eq!(
            format_sftp_command_error("permission denied".into(), false),
            "permission denied"
        );
    }
}
