use std::collections::{HashMap, VecDeque};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use core_config::{proxy_env_pairs, Profile, ProfileStore};
use core_secret::{SecretStore, KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE, KEY_SECURITY_TOKEN};
use core_session::{
    check_port_available, probe_once, suggest_free_ports, wait_for_url_with_options, HealthOptions,
};
use core_ssh::{
    assemble_local_command, fetch_local_identity, port_warning, remove_proxy_bridge,
    write_proxy_bridge, HostKeyPolicy, KnownHostsVerifier, LocalTunnel, RemoteProcessHandle,
    ReverseProxyChannel, SshConnection, SshCredentials, SshError,
};
use percent_encoding::{percent_decode_str, utf8_percent_encode, AsciiSet, NON_ALPHANUMERIC};
use serde::Serialize;
use tauri::ipc::Channel;
use tauri::State;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::sync::{broadcast, Mutex};
use url::Url;

use crate::state::{AppState, LauncherLogStore};

const MAX_LOG_LINES: usize = 2000;
const HEALTH_TIMEOUT: Duration = Duration::from_secs(90);
const HEALTH_INTERVAL: Duration = Duration::from_millis(400);
const LOCAL_PROBE_TIMEOUT: Duration = Duration::from_millis(700);
static NEXT_OPEN_SEQ: AtomicU64 = AtomicU64::new(1);

pub(crate) fn format_ssh_error(err: SshError) -> String {
    match err {
        SshError::HostKeyVerification(hk) => serde_json::json!({
            "error": hk.to_string(),
            "host_key": hk,
        })
        .to_string(),
        other => other.to_string(),
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct LocalProxyStatus {
    pub enabled: bool,
    pub active: bool,
    pub proxy_url: Option<String>,
    pub bridge: bool,
    pub bridge_path: Option<String>,
    pub error: Option<String>,
}

impl Default for LocalProxyStatus {
    fn default() -> Self {
        Self {
            enabled: false,
            active: false,
            proxy_url: None,
            bridge: false,
            bridge_path: None,
            error: None,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct SessionStatus {
    pub profile_id: String,
    pub name: String,
    pub running: bool,
    pub forwarding: bool,
    pub forwarding_active: bool,
    pub open_seq: u64,
    pub open_browser: bool,
    pub phase: String,
    pub url: String,
    pub local_url: String,
    pub warnings: Vec<String>,
    pub local_proxy: LocalProxyStatus,
    pub last_error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct LogBatchEvent {
    pub profile_id: String,
    pub lines: Vec<String>,
    pub is_replay: bool,
    pub last_seq: u64,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct LogPollResponse {
    pub profile_id: String,
    pub lines: Vec<String>,
    pub last_seq: u64,
    pub reset: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct ConnectionTestResult {
    pub ok: bool,
    pub message: String,
}

const PATH_SEGMENT_ENCODE_SET: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'.')
    .remove(b'_')
    .remove(b'~');

pub(crate) fn encode_path_segment(value: &str) -> String {
    utf8_percent_encode(value, PATH_SEGMENT_ENCODE_SET).to_string()
}

pub(crate) fn profile_proxy_slug(profile: &Profile) -> String {
    let name = profile.name.trim();
    if !name.is_empty() {
        name.to_string()
    } else {
        profile.id.clone()
    }
}

pub(crate) fn profile_proxy_prefix(profile: &Profile) -> String {
    format!("/{}/chemssh", encode_path_segment(&profile_proxy_slug(profile)))
}

pub(crate) fn normalized_proxy_path(path: &str) -> String {
    if path.is_empty() {
        "/".into()
    } else if path.starts_with('/') {
        path.to_string()
    } else {
        format!("/{path}")
    }
}

pub(crate) fn session_proxy_url(proxy_base_url: &str, profile: &Profile, open_seq: u64) -> String {
    let base = proxy_base_url.trim_end_matches('/');
    if base.is_empty() {
        return profile.browser_url();
    }
    let prefix = profile_proxy_prefix(profile);
    let path = normalized_proxy_path(&profile.local_url_path);
    let mut raw = if path != "/" {
        format!("{base}{prefix}{path}")
    } else {
        format!("{base}{prefix}")
    };
    let sep = if raw.contains('?') { '&' } else { '?' };
    raw.push(sep);
    raw.push_str("profile_id=");
    raw.push_str(&encode_path_segment(&profile.id));
    if open_seq > 0 {
        raw.push_str("&launcher_open_seq=");
        raw.push_str(&open_seq.to_string());
    }
    raw
}

pub(crate) fn profile_proxy_slug_from_path(path: &str) -> Option<String> {
    let trimmed = path.trim_start_matches('/');
    let mut parts = trimmed.split('/');
    let first = parts.next().unwrap_or("");
    let second = parts.next().unwrap_or("");
    if first.is_empty() || second.is_empty() {
        return None;
    }
    match first {
        "api" | "assets" | "launcher-logs" | "shell" | "sftp" | "static" | "terminal"
        | "chemssh" | "chemssh-wait" => return None,
        _ => {}
    }
    let decoded = percent_decode_str(first).decode_utf8().ok()?;
    let slug = decoded.trim();
    if slug.is_empty() {
        None
    } else {
        Some(slug.to_string())
    }
}

pub(crate) enum SessionBackend {
    Remote {
        profile: Profile,
        connection: Arc<SshConnection>,
        tunnel: Mutex<Option<LocalTunnel>>,
        reverse_proxy: Mutex<Option<ReverseProxyChannel>>,
        proxy_bridge_path: Mutex<Option<String>>,
        remote_process: Mutex<Option<RemoteProcessHandle>>,
        security_token: Option<String>,
    },
    Local {
        profile: Profile,
        child: Mutex<Option<tokio::process::Child>>,
        security_token: Option<String>,
    },
}

pub(crate) struct ActiveSession {
    pub(crate) profile_id: String,
    pub(crate) profile_name: String,
    pub(crate) status: Mutex<SessionStatus>,
    pub(crate) backend: SessionBackend,
    pub(crate) stop_requested: AtomicBool,
    pub(crate) remote_pid: Mutex<u32>,
    pub(crate) workspace_root: Mutex<String>,
}

impl ActiveSession {
    pub(crate) fn profile(&self) -> &Profile {
        match &self.backend {
            SessionBackend::Remote { profile, .. } | SessionBackend::Local { profile, .. } => {
                profile
            }
        }
    }

    pub(crate) fn security_token(&self) -> Option<&str> {
        match &self.backend {
            SessionBackend::Remote { security_token, .. }
            | SessionBackend::Local { security_token, .. } => security_token.as_deref(),
        }
    }

    pub(crate) fn remote_connection(&self) -> Option<Arc<SshConnection>> {
        match &self.backend {
            SessionBackend::Remote { connection, .. } => Some(Arc::clone(connection)),
            SessionBackend::Local { .. } => None,
        }
    }

    pub(crate) async fn is_forwarding(&self) -> bool {
        self.status.lock().await.forwarding
    }

    pub(crate) async fn is_ready(&self) -> bool {
        self.status.lock().await.phase == "ready"
    }

    pub(crate) async fn workspace_root(&self) -> String {
        self.workspace_root.lock().await.clone()
    }

    pub(crate) async fn set_identity(&self, pid: u32, workspace_root: String) {
        *self.remote_pid.lock().await = pid;
        *self.workspace_root.lock().await = workspace_root;
    }
}

#[derive(Default)]
struct ProfileLogBuffer {
    /// 1-based sequence number of the oldest retained line minus 1.
    /// Equal to the number of evicted lines; `base_seq + lines.len()` is `last_seq`.
    base_seq: u64,
    lines: VecDeque<String>,
}

impl ProfileLogBuffer {
    fn push(&mut self, line: String) -> u64 {
        while self.lines.len() >= MAX_LOG_LINES {
            self.lines.pop_front();
            self.base_seq += 1;
        }
        self.lines.push_back(line);
        self.last_seq()
    }

    fn last_seq(&self) -> u64 {
        self.base_seq + self.lines.len() as u64
    }

    fn all_lines(&self) -> Vec<String> {
        self.lines.iter().cloned().collect()
    }

    fn slice_since(&self, after_seq: u64) -> (Vec<String>, u64, bool) {
        let last_seq = self.last_seq();
        if after_seq == 0 || after_seq < self.base_seq {
            return (self.all_lines(), last_seq, true);
        }
        if after_seq >= last_seq {
            return (Vec::new(), last_seq, false);
        }
        let skip = (after_seq - self.base_seq) as usize;
        let lines = self.lines.iter().skip(skip).cloned().collect();
        (lines, last_seq, false)
    }
}

#[derive(Default)]
pub struct SessionManager {
    sessions: Mutex<HashMap<String, Arc<ActiveSession>>>,
    active_profile_id: Mutex<Option<String>>,
    profile_logs: Mutex<HashMap<String, ProfileLogBuffer>>,
    log_txs: Mutex<HashMap<String, broadcast::Sender<(u64, String)>>>,
    log_stream_gens: Mutex<HashMap<String, Arc<AtomicU64>>>,
    last_failures: Mutex<HashMap<String, String>>,
}

impl SessionManager {
    /// True while the profile has a ChemSSH session that is starting or running.
    pub(crate) async fn is_profile_running(&self, profile_id: &str) -> bool {
        let session = self.sessions.lock().await.get(profile_id).cloned();
        match session {
            Some(session) => session.status.lock().await.running,
            None => false,
        }
    }

    async fn ensure_log_tx(&self, profile_id: &str) -> broadcast::Sender<(u64, String)> {
        let mut txs = self.log_txs.lock().await;
        txs.entry(profile_id.to_string())
            .or_insert_with(|| broadcast::channel(512).0)
            .clone()
    }

    async fn next_log_stream_gen(&self, profile_id: &str) -> (Arc<AtomicU64>, u64) {
        let counter = {
            let mut gens = self.log_stream_gens.lock().await;
            gens.entry(profile_id.to_string())
                .or_insert_with(|| Arc::new(AtomicU64::new(0)))
                .clone()
        };
        let gen = counter.fetch_add(1, Ordering::SeqCst) + 1;
        (counter, gen)
    }

    pub async fn push_log(
        &self,
        profile_id: &str,
        profile_name: &str,
        launcher_logs: &LauncherLogStore,
        line: impl Into<String>,
    ) {
        let raw = line.into();
        let trimmed = raw.trim_end_matches(['\r', '\n']).to_string();
        if trimmed.is_empty() {
            return;
        }
        let seq = {
            let mut logs_map = self.profile_logs.lock().await;
            let buf = logs_map
                .entry(profile_id.to_string())
                .or_insert_with(|| ProfileLogBuffer {
                    base_seq: 0,
                    lines: VecDeque::with_capacity(256),
                });
            buf.push(trimmed.clone())
        };
        let tx = self.ensure_log_tx(profile_id).await;
        let _ = tx.send((seq, trimmed.clone()));
        launcher_logs.push(format!("[{profile_name}] {trimmed}"));
    }

    pub async fn set_failure(&self, profile_id: &str, err: String) {
        self.last_failures
            .lock()
            .await
            .insert(profile_id.to_string(), err);
    }

    pub async fn clear_failure(&self, profile_id: &str) {
        self.last_failures.lock().await.remove(profile_id);
    }

    pub async fn status(&self, profile_id: &str) -> Option<SessionStatus> {
        if profile_id.is_empty() {
            let active_id = self.active_profile_id.lock().await.clone();
            if let Some(id) = active_id {
                if let Some(s) = self.sessions.lock().await.get(&id).cloned() {
                    let st = s.status.lock().await.clone();
                    if st.running {
                        return Some(st);
                    }
                }
            }
            let sessions: Vec<Arc<ActiveSession>> =
                self.sessions.lock().await.values().cloned().collect();
            for s in sessions {
                let st = s.status.lock().await.clone();
                if st.running {
                    return Some(st);
                }
            }
            return None;
        }
        let session = self.sessions.lock().await.get(profile_id).cloned()?;
        let st = session.status.lock().await.clone();
        Some(st)
    }

    pub async fn activate(&self, profile_id: &str) {
        *self.active_profile_id.lock().await = Some(profile_id.to_string());
    }

    #[cfg(test)]
    pub(crate) async fn insert_active_for_test(
        &self,
        profile_id: String,
        session: Arc<ActiveSession>,
    ) {
        self.sessions
            .lock()
            .await
            .insert(profile_id.clone(), session);
        *self.active_profile_id.lock().await = Some(profile_id);
    }

    pub(crate) async fn is_same_session(&self, target: &Arc<ActiveSession>) -> bool {
        self.sessions
            .lock()
            .await
            .get(&target.profile_id)
            .map(|current| Arc::ptr_eq(current, target))
            .unwrap_or(false)
    }

    pub(crate) async fn find_for_proxy_request(
        &self,
        query_profile_id: Option<&str>,
        req_path: &str,
        referer: Option<&str>,
    ) -> Option<Arc<ActiveSession>> {
        let sessions = self.sessions.lock().await.clone();
        let active_id = self.active_profile_id.lock().await.clone();

        if let Some(pid) = query_profile_id.map(str::trim).filter(|s| !s.is_empty()) {
            if let Some(found) = sessions.get(pid) {
                return Some(Arc::clone(found));
            }
        }

        if let Some(slug) = profile_proxy_slug_from_path(req_path) {
            if let Some(found) = match_session_by_slug(&sessions, &slug) {
                return Some(found);
            }
        }

        if let Some(ref_val) = referer.map(str::trim).filter(|s| !s.is_empty()) {
            if let Ok(parsed) = Url::parse(ref_val) {
                for (k, v) in parsed.query_pairs() {
                    if k == "profile_id" && !v.trim().is_empty() {
                        if let Some(found) = sessions.get(v.trim()) {
                            return Some(Arc::clone(found));
                        }
                    }
                }
                if let Some(slug) = profile_proxy_slug_from_path(parsed.path()) {
                    if let Some(found) = match_session_by_slug(&sessions, &slug) {
                        return Some(found);
                    }
                }
            }
        }

        if let Some(id) = active_id {
            if let Some(found) = sessions.get(&id) {
                return Some(Arc::clone(found));
            }
        }

        for session in sessions.values() {
            if session.is_forwarding().await {
                return Some(Arc::clone(session));
            }
        }
        None
    }

    pub async fn all_statuses(&self) -> Vec<SessionStatus> {
        let sessions: Vec<Arc<ActiveSession>> =
            self.sessions.lock().await.values().cloned().collect();
        let mut out = Vec::with_capacity(sessions.len());
        for s in sessions {
            out.push(s.status.lock().await.clone());
        }
        out
    }

    pub async fn logs(&self, profile_id: &str) -> Vec<String> {
        self.profile_logs
            .lock()
            .await
            .get(profile_id)
            .map(ProfileLogBuffer::all_lines)
            .unwrap_or_default()
    }

    pub async fn logs_since(&self, profile_id: &str, after_seq: u64) -> LogPollResponse {
        let guard = self.profile_logs.lock().await;
        match guard.get(profile_id) {
            Some(buf) => {
                let (lines, last_seq, reset) = buf.slice_since(after_seq);
                LogPollResponse {
                    profile_id: profile_id.to_string(),
                    lines,
                    last_seq,
                    reset,
                }
            }
            None => LogPollResponse {
                profile_id: profile_id.to_string(),
                lines: Vec::new(),
                last_seq: 0,
                reset: after_seq == 0,
            },
        }
    }

    pub async fn subscribe_logs(&self, profile_id: &str) -> broadcast::Receiver<(u64, String)> {
        self.ensure_log_tx(profile_id).await.subscribe()
    }

    /// Subscribes to the live broadcast channel *before* taking the snapshot so
    /// no lines pushed concurrently are missed between replay and live delivery.
    pub async fn logs_snapshot_and_subscribe(
        &self,
        profile_id: &str,
    ) -> (Vec<String>, u64, broadcast::Receiver<(u64, String)>) {
        let rx = self.subscribe_logs(profile_id).await;
        let guard = self.profile_logs.lock().await;
        let (lines, last_seq) = guard
            .get(profile_id)
            .map(|buf| (buf.all_lines(), buf.last_seq()))
            .unwrap_or_else(|| (Vec::new(), 0));
        (lines, last_seq, rx)
    }

    /// Stops launcher-owned tunnels, reverse proxies, bridge files, SSH connections, and local
    /// child processes during application exit without terminating remote ChemSSH servers.
    pub async fn stop_all_on_exit(&self) {
        let active_sessions: Vec<Arc<ActiveSession>> = {
            let mut guard = self.sessions.lock().await;
            guard.drain().map(|(_, s)| s).collect()
        };
        for session in active_sessions {
            session.stop_requested.store(true, Ordering::SeqCst);
            match &session.backend {
                SessionBackend::Remote {
                    connection,
                    tunnel,
                    reverse_proxy,
                    proxy_bridge_path,
                    remote_process,
                    ..
                } => {
                    if let Some(proc_handle) = remote_process.lock().await.take() {
                        drop(proc_handle);
                    }
                    if let Some(t) = tunnel.lock().await.take() {
                        t.stop();
                    }
                    if let Some(mut rp) = reverse_proxy.lock().await.take() {
                        rp.close().await;
                    }
                    if let Some(bridge_path) = proxy_bridge_path.lock().await.take() {
                        if let Ok(sftp) = connection.open_sftp().await {
                            let _ = remove_proxy_bridge(&sftp, &bridge_path).await;
                        }
                    }
                    connection.disconnect().await;
                }
                SessionBackend::Local { child, .. } => {
                    if let Some(mut proc_child) = child.lock().await.take() {
                        let _ = proc_child.kill().await;
                    }
                }
            }
            let mut st = session.status.lock().await;
            st.running = false;
            st.forwarding = false;
            st.forwarding_active = false;
            st.phase = "stopped".into();
        }
    }
}

fn match_session_by_slug(
    sessions: &HashMap<String, Arc<ActiveSession>>,
    slug: &str,
) -> Option<Arc<ActiveSession>> {
    sessions
        .values()
        .find(|s| {
            profile_proxy_slug(s.profile()).eq_ignore_ascii_case(slug) || s.profile_id == slug
        })
        .cloned()
}

pub(crate) fn load_ssh_credentials_from(
    secrets: &dyn SecretStore,
    profile: &Profile,
) -> Result<SshCredentials, String> {
    let password = secrets
        .get(&profile.id, KEY_PASSWORD)
        .map_err(|e| e.to_string())?;
    let private_key_passphrase = secrets
        .get(&profile.id, KEY_PRIVATE_PASSPHRASE)
        .map_err(|e| e.to_string())?;
    Ok(SshCredentials {
        password,
        private_key_passphrase,
    })
}

pub(crate) fn load_ssh_credentials(
    state: &AppState,
    profile: &Profile,
) -> Result<SshCredentials, String> {
    load_ssh_credentials_from(state.secrets.as_ref(), profile)
}

fn load_security_token(state: &AppState, profile: &Profile) -> Result<Option<String>, String> {
    if !profile.has_security_token {
        return Ok(None);
    }
    let token = state
        .secrets
        .get(&profile.id, KEY_SECURITY_TOKEN)
        .map_err(|e| e.to_string())?;
    if token.as_deref().map(str::trim).unwrap_or("").is_empty() {
        return Err(
            "profile enables security token, but no token is stored in the credential vault".into(),
        );
    }
    Ok(token)
}

fn build_health_check_target(
    profile: &Profile,
    security_token: Option<&str>,
) -> (String, HealthOptions) {
    let base_url = profile.health_url();
    let mut opts = HealthOptions::default();

    let Some(token) = security_token.map(str::trim).filter(|t| !t.is_empty()) else {
        return (base_url, opts);
    };

    opts.headers
        .push(("X-ChemSSH-Token".into(), token.to_string()));
    opts.reject_statuses
        .insert(401, "security token rejected (HTTP 401)".into());
    opts.reject_statuses
        .insert(403, "security token rejected (HTTP 403)".into());

    if let Ok(mut parsed) = Url::parse(&base_url) {
        parsed.query_pairs_mut().append_pair("token", token);
        return (parsed.to_string(), opts);
    }
    (base_url, opts)
}

#[tauri::command]
pub async fn test_profile_connection(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<ConnectionTestResult, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    if profile.is_local() {
        return Ok(ConnectionTestResult {
            ok: true,
            message: format!("Local profile targets {}", profile.browser_url()),
        });
    }
    let creds = load_ssh_credentials(&state, &profile)?;
    let verifier = KnownHostsVerifier::new(
        &state.known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );
    match SshConnection::connect(&profile, &creds, verifier).await {
        Ok(conn) => {
            conn.disconnect().await;
            Ok(ConnectionTestResult {
                ok: true,
                message: "SSH connection and authentication succeeded".into(),
            })
        }
        Err(err) => Err(format_ssh_error(err)),
    }
}

#[tauri::command]
pub async fn start_session(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SessionStatus, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    profile.validate().map_err(|e| e.to_string())?;

    let existing = state
        .sessions
        .sessions
        .lock()
        .await
        .get(&profile.id)
        .cloned();

    if let Some(session) = existing {
        let snapshot = session.status.lock().await.clone();
        // A start that is still bringing the tunnel up must not be raced by a second one.
        if snapshot.running && snapshot.phase == "starting" {
            state.sessions.activate(&profile.id).await;
            return Ok(snapshot);
        }
        if snapshot.running {
            return resume_or_reuse_session(&state, &profile, session).await;
        }
    }

    let security_token = load_security_token(&state, &profile)?;
    state.sessions.clear_failure(&profile.id).await;

    if profile.is_local() {
        start_local_session(&state, profile, security_token).await
    } else {
        start_remote_session(&state, profile, security_token, host_key_policy).await
    }
}

async fn resume_or_reuse_session(
    state: &AppState,
    profile: &Profile,
    session: Arc<ActiveSession>,
) -> Result<SessionStatus, String> {
    state.sessions.activate(&profile.id).await;

    let currently_forwarding = session.status.lock().await.forwarding_active;
    if currently_forwarding || profile.is_local() {
        let mut st = session.status.lock().await;
        st.open_seq = NEXT_OPEN_SEQ.fetch_add(1, Ordering::SeqCst);
        st.url = session_proxy_url(&state.proxy_base_url, profile, st.open_seq);
        st.local_url = profile.browser_url();
        let snapshot = st.clone();
        drop(st);
        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                "[session] reusing active ChemSSH session",
            )
            .await;
        return Ok(snapshot);
    }

    // Forwarding was paused while SSH connection stayed alive: resume forwarding.
    if let Err(err) = check_port_available(&profile.local_host, profile.local_port) {
        let suggestions = suggest_free_ports(&profile.local_host, profile.local_port, 3);
        let msg = format!("{err}; suggested free ports: {suggestions:?}");
        state.sessions.set_failure(&profile.id, msg.clone()).await;
        return Err(msg);
    }

    let SessionBackend::Remote {
        connection,
        tunnel,
        reverse_proxy,
        proxy_bridge_path,
        security_token,
        ..
    } = &session.backend
    else {
        let st = session.status.lock().await.clone();
        return Ok(st);
    };

    let mut extra_warnings = Vec::new();
    let mut local_proxy_update: Option<LocalProxyStatus> = None;
    if profile.proxy_active() && reverse_proxy.lock().await.is_none() {
        let lp = profile.local_proxy.normalized();
        let mut lps = LocalProxyStatus {
            enabled: true,
            ..Default::default()
        };
        match connection.start_reverse_proxy(lp.address()).await {
            Ok(ch) => {
                let url = ch.proxy_url.clone();
                lps.active = true;
                lps.proxy_url = Some(url.clone());
                match connection.open_sftp().await {
                    Ok(sftp) => {
                        match write_proxy_bridge(&sftp, profile.remote_port, &url, &lp.no_proxy)
                            .await
                        {
                            Ok(path) => {
                                lps.bridge = true;
                                lps.bridge_path = Some(path.clone());
                                *proxy_bridge_path.lock().await = Some(path);
                            }
                            Err(err) => {
                                let m = format!("proxy bridge write failed (degraded): {err}");
                                extra_warnings.push(m.clone());
                                lps.error = Some(m);
                            }
                        }
                    }
                    Err(err) => {
                        let m = format!("proxy bridge sftp open failed (degraded): {err}");
                        extra_warnings.push(m.clone());
                        lps.error = Some(m);
                    }
                }
                *reverse_proxy.lock().await = Some(ch);
            }
            Err(err) => {
                let m = format!("reverse proxy channel failed (degraded): {err}");
                extra_warnings.push(m.clone());
                lps.error = Some(m);
            }
        }
        local_proxy_update = Some(lps);
    }

    let new_tunnel = connection
        .start_local_tunnel(
            &profile.local_host,
            profile.local_port,
            profile.remote_host.clone(),
            profile.remote_port,
        )
        .await
        .map_err(|e| e.to_string())?;
    *tunnel.lock().await = Some(new_tunnel);

    let snapshot = {
        let mut st = session.status.lock().await;
        st.forwarding = true;
        st.forwarding_active = true;
        st.open_seq = NEXT_OPEN_SEQ.fetch_add(1, Ordering::SeqCst);
        st.url = session_proxy_url(&state.proxy_base_url, profile, st.open_seq);
        st.local_url = profile.browser_url();
        st.phase = "starting".into();
        st.last_error = None;
        for w in extra_warnings {
            st.warnings.push(w);
        }
        if let Some(lps) = local_proxy_update {
            st.local_proxy = lps;
        }
        st.clone()
    };

    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            "[session] resumed paused local forwarding",
        )
        .await;

    spawn_health_check_task(
        state.clone(),
        Arc::clone(&session),
        profile.clone(),
        security_token.clone(),
    );

    Ok(snapshot)
}

async fn start_local_session(
    state: &AppState,
    profile: Profile,
    security_token: Option<String>,
) -> Result<SessionStatus, String> {
    state
        .chemssh_bridge
        .close_for_profile(&profile.id, &state.sftp, &state.sftp_sync, &state.launcher_logs)
        .await;
    let browser_url = profile.browser_url();
    let (health_url, health_opts) =
        build_health_check_target(&profile, security_token.as_deref());

    let mut warnings = profile.warnings();
    if let Some(pw) = port_warning(&profile) {
        warnings.push(pw);
    }

    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("start requested for {}", profile.name),
        )
        .await;

    // Probe first for 700ms so an already-running local ChemSSH server is reused cleanly.
    if probe_once(&health_url, LOCAL_PROBE_TIMEOUT, &health_opts)
        .await
        .is_ok()
    {
        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                format!("[session] local ChemSSH already healthy at {browser_url}; reusing existing service"),
            )
            .await;

        let open_seq = NEXT_OPEN_SEQ.fetch_add(1, Ordering::SeqCst);
        let proxy_url = session_proxy_url(&state.proxy_base_url, &profile, open_seq);
        let ready_status = SessionStatus {
            profile_id: profile.id.clone(),
            name: profile.name.clone(),
            running: true,
            forwarding: true,
            forwarding_active: true,
            open_seq,
            open_browser: profile.open_browser,
            phase: "ready".into(),
            url: proxy_url,
            local_url: browser_url,
            warnings,
            local_proxy: LocalProxyStatus::default(),
            last_error: None,
        };
        let active = Arc::new(ActiveSession {
            profile_id: profile.id.clone(),
            profile_name: profile.name.clone(),
            status: Mutex::new(ready_status.clone()),
            backend: SessionBackend::Local {
                profile: profile.clone(),
                child: Mutex::new(None),
                security_token: security_token.clone(),
            },
            stop_requested: AtomicBool::new(false),
            remote_pid: Mutex::new(0),
            workspace_root: Mutex::new(String::new()),
        });
        state
            .sessions
            .sessions
            .lock()
            .await
            .insert(profile.id.clone(), Arc::clone(&active));
        state.sessions.activate(&profile.id).await;
        tokio::spawn(refresh_session_identity_and_warm_bridge(
            state.clone(),
            active,
            profile,
            security_token,
        ));
        return Ok(ready_status);
    }

    if let Err(err) = check_port_available(&profile.local_host, profile.local_port) {
        let suggestions = suggest_free_ports(&profile.local_host, profile.local_port, 3);
        let msg = format!("{err}; suggested free ports: {suggestions:?}");
        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                format!("[error] {msg}"),
            )
            .await;
        state.sessions.set_failure(&profile.id, msg.clone()).await;
        return Err(msg);
    }

    let local_cmd = assemble_local_command(&profile);
    let mut cmd = build_local_shell_command(&local_cmd);
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut child = match cmd.spawn() {
        Ok(child) => child,
        Err(e) => {
            let msg = format!("failed to launch local ChemSSH command: {e}");
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!("[error] {msg}"),
                )
                .await;
            return Err(msg);
        }
    };
    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("starting {} at {browser_url}", profile.name),
        )
        .await;

    let stdout = child.stdout.take();
    let stderr = child.stderr.take();

    let open_seq = NEXT_OPEN_SEQ.fetch_add(1, Ordering::SeqCst);
    let proxy_url = session_proxy_url(&state.proxy_base_url, &profile, open_seq);
    let initial_status = SessionStatus {
        profile_id: profile.id.clone(),
        name: profile.name.clone(),
        running: true,
        forwarding: true,
        forwarding_active: true,
        open_seq,
        open_browser: profile.open_browser,
        phase: "starting".into(),
        url: proxy_url,
        local_url: browser_url,
        warnings: warnings.clone(),
        local_proxy: LocalProxyStatus::default(),
        last_error: None,
    };

    let active = Arc::new(ActiveSession {
        profile_id: profile.id.clone(),
        profile_name: profile.name.clone(),
        status: Mutex::new(initial_status.clone()),
        backend: SessionBackend::Local {
            profile: profile.clone(),
            child: Mutex::new(Some(child)),
            security_token: security_token.clone(),
        },
        stop_requested: AtomicBool::new(false),
        remote_pid: Mutex::new(0),
        workspace_root: Mutex::new(String::new()),
    });

    for w in &warnings {
        state
            .sessions
            .push_log(&profile.id, &profile.name, &state.launcher_logs, format!("[warn] {w}"))
            .await;
    }

    state
        .sessions
        .sessions
        .lock()
        .await
        .insert(profile.id.clone(), Arc::clone(&active));
    state.sessions.activate(&profile.id).await;

    if let Some(out) = stdout {
        spawn_pipe_log_reader(
            out,
            profile.id.clone(),
            profile.name.clone(),
            Arc::clone(&state.sessions),
            Arc::clone(&state.launcher_logs),
        );
    }
    if let Some(err) = stderr {
        spawn_pipe_log_reader(
            err,
            profile.id.clone(),
            profile.name.clone(),
            Arc::clone(&state.sessions),
            Arc::clone(&state.launcher_logs),
        );
    }

    // Monitor local process exit
    let monitor_session = Arc::clone(&active);
    let sessions_mgr = Arc::clone(&state.sessions);
    let launcher_logs = Arc::clone(&state.launcher_logs);
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(Duration::from_millis(250)).await;
            if monitor_session.stop_requested.load(Ordering::SeqCst) {
                break;
            }
            let exit_res = {
                let SessionBackend::Local { child, .. } = &monitor_session.backend else {
                    break;
                };
                let mut guard = child.lock().await;
                match guard.as_mut() {
                    Some(proc_child) => match proc_child.try_wait() {
                        Ok(Some(status)) => {
                            *guard = None;
                            Some(Ok(status))
                        }
                        Ok(None) => None,
                        Err(e) => {
                            *guard = None;
                            Some(Err(e))
                        }
                    },
                    None => break,
                }
            };
            if let Some(res) = exit_res {
                let msg = match res {
                    Ok(status) => format!("[session] local process exited with status {status}"),
                    Err(ref err) => format!("[session] local process wait error: {err}"),
                };
                sessions_mgr
                    .push_log(
                        &monitor_session.profile_id,
                        &monitor_session.profile_name,
                        &launcher_logs,
                        msg.clone(),
                    )
                    .await;
                let keep_alive = match res {
                    Ok(status) if status.success() => true,
                    _ => false,
                };
                if !keep_alive {
                    let mut st = monitor_session.status.lock().await;
                    st.running = false;
                    st.forwarding = false;
                    st.forwarding_active = false;
                    st.phase = "stopped".into();
                    st.last_error = Some(msg.clone());
                    sessions_mgr
                        .set_failure(&monitor_session.profile_id, msg)
                        .await;
                }
                break;
            }
        }
    });

    spawn_health_check_task(
        state.clone(),
        Arc::clone(&active),
        profile,
        security_token,
    );

    Ok(initial_status)
}

async fn start_remote_session(
    state: &AppState,
    profile: Profile,
    security_token: Option<String>,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SessionStatus, String> {
    state
        .chemssh_bridge
        .close_for_profile(&profile.id, &state.sftp, &state.sftp_sync, &state.launcher_logs)
        .await;
    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("start requested for {}", profile.name),
        )
        .await;
    if let Err(err) = check_port_available(&profile.local_host, profile.local_port) {
        let suggestions = suggest_free_ports(&profile.local_host, profile.local_port, 3);
        let msg = format!("{err}; suggested free ports: {suggestions:?}");
        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                format!("[error] {msg}"),
            )
            .await;
        state.sessions.set_failure(&profile.id, msg.clone()).await;
        return Err(msg);
    }

    let mut warnings = profile.warnings();
    if let Some(pw) = port_warning(&profile) {
        warnings.push(pw);
    }

    let creds = match load_ssh_credentials(state, &profile) {
        Ok(creds) => creds,
        Err(err) => {
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!("[error] {err}"),
                )
                .await;
            return Err(err);
        }
    };
    let verifier = KnownHostsVerifier::new(
        &state.known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );

    let connection = match SshConnection::connect(&profile, &creds, verifier).await {
        Ok(conn) => Arc::new(conn),
        Err(err) => {
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!("SSH connection failed for {}: {err}", profile.name),
                )
                .await;
            return Err(format_ssh_error(err));
        }
    };
    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("SSH connection OK for {}", profile.name),
        )
        .await;

    // Everything past the SSH handshake runs in the background so the UI gets an
    // immediate "starting" status; the host-key prompt above stays synchronous.
    let browser_url = profile.browser_url();
    let open_seq = NEXT_OPEN_SEQ.fetch_add(1, Ordering::SeqCst);
    let proxy_url = session_proxy_url(&state.proxy_base_url, &profile, open_seq);
    let initial_status = SessionStatus {
        profile_id: profile.id.clone(),
        name: profile.name.clone(),
        running: true,
        forwarding: false,
        forwarding_active: false,
        open_seq,
        open_browser: profile.open_browser,
        phase: "starting".into(),
        url: proxy_url,
        local_url: browser_url,
        warnings: warnings.clone(),
        local_proxy: LocalProxyStatus {
            enabled: profile.proxy_active(),
            ..Default::default()
        },
        last_error: None,
    };

    let active = Arc::new(ActiveSession {
        profile_id: profile.id.clone(),
        profile_name: profile.name.clone(),
        status: Mutex::new(initial_status.clone()),
        backend: SessionBackend::Remote {
            profile: profile.clone(),
            connection: Arc::clone(&connection),
            tunnel: Mutex::new(None),
            reverse_proxy: Mutex::new(None),
            proxy_bridge_path: Mutex::new(None),
            remote_process: Mutex::new(None),
            security_token: security_token.clone(),
        },
        stop_requested: AtomicBool::new(false),
        remote_pid: Mutex::new(0),
        workspace_root: Mutex::new(String::new()),
    });

    for w in &warnings {
        state
            .sessions
            .push_log(&profile.id, &profile.name, &state.launcher_logs, format!("[warn] {w}"))
            .await;
    }

    state
        .sessions
        .sessions
        .lock()
        .await
        .insert(profile.id.clone(), Arc::clone(&active));
    state.sessions.activate(&profile.id).await;

    tokio::spawn(finish_remote_startup(
        Arc::clone(&active),
        state.clone(),
        profile,
        security_token,
    ));

    Ok(initial_status)
}

async fn fail_remote_startup(active: &Arc<ActiveSession>, state: &AppState, msg: String) {
    if active.stop_requested.load(Ordering::SeqCst) {
        return;
    }
    state
        .chemssh_bridge
        .close_for_profile(
            &active.profile_id,
            &state.sftp,
            &state.sftp_sync,
            &state.launcher_logs,
        )
        .await;
    state.sessions.set_failure(&active.profile_id, msg.clone()).await;
    state
        .sessions
        .push_log(
            &active.profile_id,
            &active.profile_name,
            &state.launcher_logs,
            format!("[error] {msg}"),
        )
        .await;
    cleanup_remote_backend(active).await;
    let mut st = active.status.lock().await;
    st.running = false;
    st.forwarding = false;
    st.forwarding_active = false;
    st.phase = "failed".into();
    st.last_error = Some(msg);
}

async fn finish_remote_startup(
    active: Arc<ActiveSession>,
    state: AppState,
    profile: Profile,
    security_token: Option<String>,
) {
    let sessions = Arc::clone(&state.sessions);
    let logs = Arc::clone(&state.launcher_logs);
    let SessionBackend::Remote {
        connection,
        tunnel,
        reverse_proxy,
        proxy_bridge_path,
        remote_process,
        ..
    } = &active.backend
    else {
        return;
    };

    // Optional local network proxy: failure must degrade with warning, never fail the session.
    let mut proxy_env = Vec::new();
    if profile.proxy_active() {
        let lp = profile.local_proxy.normalized();
        let mut local_proxy_status = LocalProxyStatus {
            enabled: true,
            ..Default::default()
        };
        let mut new_warnings: Vec<String> = Vec::new();
        match connection.start_reverse_proxy(lp.address()).await {
            Ok(channel) => {
                let proxy_url = channel.proxy_url.clone();
                proxy_env = proxy_env_pairs(&proxy_url, &lp.no_proxy);
                local_proxy_status.active = true;
                local_proxy_status.proxy_url = Some(proxy_url.clone());
                sessions
                    .push_log(
                        &profile.id,
                        &profile.name,
                        &logs,
                        format!("local proxy active at {proxy_url}"),
                    )
                    .await;
                match connection.open_sftp().await {
                    Ok(sftp) => {
                        match write_proxy_bridge(&sftp, profile.remote_port, &proxy_url, &lp.no_proxy)
                            .await
                        {
                            Ok(path) => {
                                local_proxy_status.bridge = true;
                                local_proxy_status.bridge_path = Some(path.clone());
                                *proxy_bridge_path.lock().await = Some(path);
                            }
                            Err(err) => {
                                let msg = format!("proxy bridge write failed (degraded): {err}");
                                new_warnings.push(msg.clone());
                                local_proxy_status.error = Some(msg);
                            }
                        }
                    }
                    Err(err) => {
                        let msg = format!("proxy bridge sftp open failed (degraded): {err}");
                        new_warnings.push(msg.clone());
                        local_proxy_status.error = Some(msg);
                    }
                }
                *reverse_proxy.lock().await = Some(channel);
            }
            Err(err) => {
                let msg = format!("reverse proxy channel failed (degraded): {err}");
                new_warnings.push(msg.clone());
                local_proxy_status.error = Some(msg);
            }
        }
        for w in &new_warnings {
            sessions
                .push_log(&profile.id, &profile.name, &logs, format!("[warn] {w}"))
                .await;
        }
        let mut st = active.status.lock().await;
        st.warnings.extend(new_warnings);
        st.local_proxy = local_proxy_status;
    }
    if active.stop_requested.load(Ordering::SeqCst) {
        return;
    }

    // Probe remote port with `--check-port` before starting a new remote ChemSSH instance.
    let mut reusable_remote = false;
    match connection.run_check_port_command(&profile, &proxy_env).await {
        Ok(check) if check.reusable => {
            reusable_remote = true;
            sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &logs,
                    "remote ChemSSH is reusable; starting tunnel without launching another server",
                )
                .await;
        }
        Ok(check) if !check.output.is_empty() => {
            sessions
                .push_log(&profile.id, &profile.name, &logs, format!("[check-port] {}", check.output))
                .await;
        }
        Ok(_) => {
            sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &logs,
                    "remote ChemSSH port is available; starting configured command",
                )
                .await;
        }
        Err(err) => {
            sessions
                .push_log(&profile.id, &profile.name, &logs, format!("[check-port] skipped: {err}"))
                .await;
        }
    }
    if active.stop_requested.load(Ordering::SeqCst) {
        return;
    }

    match connection
        .start_local_tunnel(
            &profile.local_host,
            profile.local_port,
            profile.remote_host.clone(),
            profile.remote_port,
        )
        .await
    {
        Ok(t) => {
            *tunnel.lock().await = Some(t);
            let mut st = active.status.lock().await;
            st.forwarding = true;
            st.forwarding_active = true;
            drop(st);
            sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &logs,
                    format!("starting {} at {}", profile.name, profile.browser_url()),
                )
                .await;
        }
        Err(err) => {
            fail_remote_startup(&active, &state, err.to_string()).await;
            return;
        }
    }

    if !reusable_remote {
        match connection.start_remote_chemssh(&profile, &proxy_env).await {
            Ok((handle, mut out_rx, exit_rx)) => {
                *remote_process.lock().await = Some(handle);

                let log_mgr = Arc::clone(&sessions);
                let launcher_logs = Arc::clone(&logs);
                let pid_copy = profile.id.clone();
                let pname_copy = profile.name.clone();
                tokio::spawn(async move {
                    while let Some(chunk) = out_rx.recv().await {
                        for line in chunk.lines() {
                            log_mgr
                                .push_log(&pid_copy, &pname_copy, &launcher_logs, line.to_string())
                                .await;
                        }
                    }
                });

                let watch_session = Arc::clone(&active);
                let watch_state = state.clone();
                tokio::spawn(async move {
                    let exit_res = exit_rx.await;
                    if watch_session.stop_requested.load(Ordering::SeqCst) {
                        return;
                    }
                    match exit_res {
                        Ok(Ok(Some(0))) | Ok(Ok(None)) => {
                            // Clean exit (for example backgrounded/reused remote service): keep tunnel & session alive.
                            watch_state
                                .sessions
                                .push_log(
                                    &watch_session.profile_id,
                                    &watch_session.profile_name,
                                    &watch_state.launcher_logs,
                                    "[session] remote launcher command exited cleanly; keeping SSH tunnel open",
                                )
                                .await;
                        }
                        other => {
                            let msg = match other {
                                Ok(Ok(Some(code))) => format!("remote command exited with status {code}"),
                                Ok(Err(reason)) => format!("remote command terminated: {reason}"),
                                Err(_) => "remote command monitor dropped".to_string(),
                                Ok(Ok(None)) => unreachable!(),
                            };
                            watch_state
                                .sessions
                                .push_log(
                                    &watch_session.profile_id,
                                    &watch_session.profile_name,
                                    &watch_state.launcher_logs,
                                    format!("[session] {msg}"),
                                )
                                .await;
                            let already_ready = watch_session.status.lock().await.phase == "ready";
                            if !already_ready {
                                fail_remote_startup(&watch_session, &watch_state, msg).await;
                            }
                        }
                    }
                });
            }
            Err(err) => {
                fail_remote_startup(&active, &state, err.to_string()).await;
                return;
            }
        }
    }
    if active.stop_requested.load(Ordering::SeqCst) {
        return;
    }

    spawn_health_check_task(state, Arc::clone(&active), profile, security_token);
}

fn spawn_health_check_task(
    state: AppState,
    session: Arc<ActiveSession>,
    profile: Profile,
    security_token: Option<String>,
) {
    tokio::spawn(async move {
        let display_url = profile.health_url();
        let (health_url, health_opts) =
            build_health_check_target(&profile, security_token.as_deref());
        match wait_for_url_with_options(
            &health_url,
            HEALTH_TIMEOUT,
            HEALTH_INTERVAL,
            &health_opts,
        )
        .await
        {
            Ok(()) => {
                let mut st = session.status.lock().await;
                if st.running {
                    st.phase = "ready".into();
                    st.last_error = None;
                }
                drop(st);
                state.sessions.clear_failure(&profile.id).await;
                state
                    .sessions
                    .push_log(
                        &profile.id,
                        &profile.name,
                        &state.launcher_logs,
                        format!("[session] ready at {display_url}"),
                    )
                    .await;
                tokio::spawn(refresh_session_identity_and_warm_bridge(
                    state,
                    session,
                    profile,
                    security_token,
                ));
            }
            Err(err) => {
                let msg = err.to_string();
                let mut st = session.status.lock().await;
                if st.running {
                    st.phase = "failed".into();
                    st.last_error = Some(msg.clone());
                }
                drop(st);
                state.sessions.set_failure(&profile.id, msg.clone()).await;
                state
                    .sessions
                    .push_log(
                        &profile.id,
                        &profile.name,
                        &state.launcher_logs,
                        format!("[warn] health check: {msg}"),
                    )
                    .await;
            }
        }
    });
}

async fn refresh_session_identity_and_warm_bridge(
    state: AppState,
    session: Arc<ActiveSession>,
    profile: Profile,
    security_token: Option<String>,
) {
    if !state.sessions.is_same_session(&session).await || !session.is_forwarding().await {
        return;
    }
    let res = if profile.is_local() {
        fetch_local_identity(
            &profile.local_host,
            profile.local_port,
            security_token.as_deref(),
        )
        .await
    } else {
        let Some(conn) = session.remote_connection() else {
            return;
        };
        conn.fetch_remote_identity(
            &profile.remote_host,
            profile.remote_port,
            security_token.as_deref(),
        )
        .await
    };

    let identity = match res {
        Ok(id) => id,
        Err(err) => {
            let scope = if profile.is_local() { "local " } else { "" };
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!("warning: could not read {scope}ChemSSH identity: {err}"),
                )
                .await;
            return;
        }
    };

    if !state.sessions.is_same_session(&session).await {
        return;
    }
    session
        .set_identity(identity.pid, identity.workspace_root.clone())
        .await;
    let scope = if profile.is_local() {
        "local ChemSSH identity"
    } else {
        "ChemSSH identity"
    };
    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("{scope} OK: pid {}, version {}", identity.pid, identity.project_version),
        )
        .await;

    if !profile.is_local() && !identity.workspace_root.trim().is_empty() {
        state
            .chemssh_bridge
            .spawn_warm_sftp(state.clone(), session, profile, identity.workspace_root);
    }
}

fn build_local_shell_command(script: &str) -> tokio::process::Command {
    #[cfg(target_os = "windows")]
    {
        let mut cmd = tokio::process::Command::new("powershell.exe");
        cmd.args(["-NoProfile", "-NonInteractive", "-Command", script]);
        cmd.creation_flags(0x0800_0000);
        cmd
    }
    #[cfg(not(target_os = "windows"))]
    {
        let mut cmd = tokio::process::Command::new("/bin/sh");
        cmd.args(["-c", script]);
        cmd
    }
}

fn spawn_pipe_log_reader<R: tokio::io::AsyncRead + Unpin + Send + 'static>(
    reader: R,
    profile_id: String,
    profile_name: String,
    sessions_mgr: Arc<SessionManager>,
    launcher_logs: Arc<LauncherLogStore>,
) {
    tokio::spawn(async move {
        let mut lines = BufReader::new(reader).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            sessions_mgr
                .push_log(&profile_id, &profile_name, &launcher_logs, line)
                .await;
        }
    });
}

async fn cleanup_remote_backend(session: &ActiveSession) {
    let SessionBackend::Remote {
        connection,
        tunnel,
        reverse_proxy,
        proxy_bridge_path,
        remote_process,
        ..
    } = &session.backend
    else {
        return;
    };

    if let Some(proc_handle) = remote_process.lock().await.take() {
        proc_handle.stop();
    }
    if let Some(t) = tunnel.lock().await.take() {
        t.stop();
    }
    if let Some(mut rp) = reverse_proxy.lock().await.take() {
        rp.close().await;
    }
    if let Some(bridge_path) = proxy_bridge_path.lock().await.take() {
        if let Ok(sftp) = connection.open_sftp().await {
            let _ = remove_proxy_bridge(&sftp, &bridge_path).await;
        }
    }
    connection.disconnect().await;
}

async fn kill_local_pid(pid: u32) {
    if pid == 0 {
        return;
    }
    #[cfg(target_os = "windows")]
    {
        let mut cmd = tokio::process::Command::new("taskkill.exe");
        cmd.args(["/PID", &pid.to_string(), "/T", "/F"]);
        cmd.creation_flags(0x0800_0000);
        let _ = cmd.output().await;
    }
    #[cfg(not(target_os = "windows"))]
    {
        let _ = tokio::process::Command::new("kill")
            .args(["-TERM", &pid.to_string()])
            .output()
            .await;
    }
}

#[tauri::command]
pub async fn activate_session(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<(), String> {
    state.sessions.activate(&profile_id).await;
    Ok(())
}

/// Pauses local forwarding while keeping the remote reverse proxy channel and bridge file alive.
#[tauri::command]
pub async fn pause_forwarding(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<SessionStatus, String> {
    let session = state
        .sessions
        .sessions
        .lock()
        .await
        .get(&profile_id)
        .cloned()
        .ok_or_else(|| format!("session not found: {profile_id}"))?;

    if let SessionBackend::Remote { tunnel, .. } = &session.backend {
        if let Some(t) = tunnel.lock().await.take() {
            t.stop();
        }
    }
    let mut status = session.status.lock().await;
    status.forwarding = false;
    status.forwarding_active = false;
    status.phase = "paused".into();
    let snapshot = status.clone();
    drop(status);

    state
        .chemssh_bridge
        .close_for_profile(&profile_id, &state.sftp, &state.sftp_sync, &state.launcher_logs)
        .await;

    state
        .sessions
        .push_log(
            &session.profile_id,
            &session.profile_name,
            &state.launcher_logs,
            "[session] paused local forwarding",
        )
        .await;
    Ok(snapshot)
}

/// Full stop: stops ChemSSH process via `/api/system/identity` PID + process handle, closes tunnel,
/// closes reverse proxy channel, removes remote bridge file, and disconnects SSH.
#[tauri::command]
pub async fn stop_session(state: State<'_, AppState>, profile_id: String) -> Result<(), String> {
    let session = state
        .sessions
        .sessions
        .lock()
        .await
        .get(&profile_id)
        .cloned();
    state
        .chemssh_bridge
        .close_for_profile(&profile_id, &state.sftp, &state.sftp_sync, &state.launcher_logs)
        .await;
    let Some(session) = session else {
        return Ok(());
    };

    session.stop_requested.store(true, Ordering::SeqCst);

    match &session.backend {
        SessionBackend::Remote {
            profile,
            connection,
            security_token,
            ..
        } => {
            if let Ok(ident) = connection
                .fetch_remote_identity(
                    &profile.remote_host,
                    profile.remote_port,
                    security_token.as_deref(),
                )
                .await
            {
                state
                    .sessions
                    .push_log(
                        &session.profile_id,
                        &session.profile_name,
                        &state.launcher_logs,
                        format!("[session] stopping remote ChemSSH pid {}", ident.pid),
                    )
                    .await;
                let _ = connection.stop_remote_pid(ident.pid).await;
            }
            cleanup_remote_backend(&session).await;
        }
        SessionBackend::Local {
            profile,
            child,
            security_token,
        } => {
            let ident_pid = fetch_local_identity(
                &profile.local_host,
                profile.local_port,
                security_token.as_deref(),
            )
            .await
            .ok()
            .map(|i| i.pid);

            if let Some(mut proc_child) = child.lock().await.take() {
                let _ = proc_child.kill().await;
            }
            if let Some(pid) = ident_pid {
                state
                    .sessions
                    .push_log(
                        &session.profile_id,
                        &session.profile_name,
                        &state.launcher_logs,
                        format!("[session] stopping local ChemSSH pid {pid}"),
                    )
                    .await;
                kill_local_pid(pid).await;
            }
        }
    }

    let mut status = session.status.lock().await;
    status.running = false;
    status.forwarding = false;
    status.forwarding_active = false;
    status.phase = "stopped".into();
    drop(status);

    state
        .sessions
        .push_log(
            &session.profile_id,
            &session.profile_name,
            &state.launcher_logs,
            "[session] stopped",
        )
        .await;
    Ok(())
}

#[tauri::command]
pub async fn get_session_status(
    state: State<'_, AppState>,
    profile_id: Option<String>,
) -> Result<Option<SessionStatus>, String> {
    Ok(state
        .sessions
        .status(profile_id.as_deref().unwrap_or(""))
        .await)
}

#[tauri::command]
pub async fn list_session_statuses(
    state: State<'_, AppState>,
) -> Result<Vec<SessionStatus>, String> {
    Ok(state.sessions.all_statuses().await)
}

#[tauri::command]
pub async fn get_session_logs(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<Vec<String>, String> {
    Ok(state.sessions.logs(&profile_id).await)
}

#[tauri::command]
pub async fn poll_session_logs(
    state: State<'_, AppState>,
    profile_id: String,
    after_seq: Option<u64>,
) -> Result<LogPollResponse, String> {
    Ok(state
        .sessions
        .logs_since(&profile_id, after_seq.unwrap_or(0))
        .await)
}

#[tauri::command]
pub async fn stream_session_logs(
    state: State<'_, AppState>,
    profile_id: String,
    on_event: Channel<LogBatchEvent>,
) -> Result<(), String> {
    let (gen_counter, my_gen) = state.sessions.next_log_stream_gen(&profile_id).await;
    let (snapshot, mut last_sent_seq, mut rx) = state
        .sessions
        .logs_snapshot_and_subscribe(&profile_id)
        .await;
    let _ = on_event.send(LogBatchEvent {
        profile_id: profile_id.clone(),
        lines: snapshot,
        is_replay: true,
        last_seq: last_sent_seq,
    });
    tokio::spawn(async move {
        let mut batch = Vec::new();
        let mut tick = tokio::time::interval(Duration::from_millis(80));
        loop {
            if gen_counter.load(Ordering::SeqCst) != my_gen {
                break;
            }
            tokio::select! {
                res = rx.recv() => {
                    match res {
                        Ok((seq, line)) => {
                            if seq > last_sent_seq {
                                last_sent_seq = seq;
                                batch.push(line);
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(_)) => continue,
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }
                _ = tick.tick(), if !batch.is_empty() => {
                    if gen_counter.load(Ordering::SeqCst) != my_gen {
                        break;
                    }
                    let lines = std::mem::take(&mut batch);
                    if on_event.send(LogBatchEvent {
                        profile_id: profile_id.clone(),
                        lines,
                        is_replay: false,
                        last_seq: last_sent_seq,
                    }).is_err() {
                        break;
                    }
                }
            }
        }
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn session_logs_poll_initial_incremental_and_caught_up() {
        let mgr = SessionManager::default();
        let launcher_logs = LauncherLogStore::default();

        let empty = mgr.logs_since("p1", 0).await;
        assert!(empty.lines.is_empty());
        assert_eq!(empty.last_seq, 0);
        assert!(empty.reset);

        mgr.push_log("p1", "example-server", &launcher_logs, "line-1")
            .await;
        mgr.push_log("p1", "example-server", &launcher_logs, "line-2")
            .await;

        let initial = mgr.logs_since("p1", 0).await;
        assert_eq!(initial.lines, vec!["line-1", "line-2"]);
        assert_eq!(initial.last_seq, 2);
        assert!(initial.reset);

        let caught_up = mgr.logs_since("p1", 2).await;
        assert!(caught_up.lines.is_empty());
        assert_eq!(caught_up.last_seq, 2);
        assert!(!caught_up.reset);

        mgr.push_log("p1", "example-server", &launcher_logs, "line-3")
            .await;
        let delta = mgr.logs_since("p1", 2).await;
        assert_eq!(delta.lines, vec!["line-3"]);
        assert_eq!(delta.last_seq, 3);
        assert!(!delta.reset);
    }

    #[tokio::test]
    async fn session_logs_snapshot_and_subscribe_has_no_gap() {
        let mgr = SessionManager::default();
        let launcher_logs = LauncherLogStore::default();

        mgr.push_log("p2", "example-server", &launcher_logs, "before")
            .await;
        let (snapshot, last_seq, mut rx) = mgr.logs_snapshot_and_subscribe("p2").await;
        assert_eq!(snapshot, vec!["before"]);
        assert_eq!(last_seq, 1);

        mgr.push_log("p2", "example-server", &launcher_logs, "after")
            .await;
        let (seq, line) = rx.recv().await.expect("live line received");
        assert_eq!(seq, 2);
        assert_eq!(line, "after");
    }

    #[test]
    fn profile_log_buffer_resets_cursor_after_ring_eviction() {
        let mut buf = ProfileLogBuffer::default();
        for i in 1..=(MAX_LOG_LINES + 5) {
            buf.push(format!("line-{i}"));
        }
        assert_eq!(buf.base_seq, 5);
        assert_eq!(buf.last_seq(), (MAX_LOG_LINES + 5) as u64);

        let (lines, last_seq, reset) = buf.slice_since(2);
        assert!(reset);
        assert_eq!(lines.len(), MAX_LOG_LINES);
        assert_eq!(lines.first().map(String::as_str), Some("line-6"));
        assert_eq!(last_seq, (MAX_LOG_LINES + 5) as u64);

        let (delta, delta_seq, delta_reset) = buf.slice_since((MAX_LOG_LINES + 3) as u64);
        assert!(!delta_reset);
        assert_eq!(delta, vec![format!("line-{}", MAX_LOG_LINES + 4), format!("line-{}", MAX_LOG_LINES + 5)]);
        assert_eq!(delta_seq, (MAX_LOG_LINES + 5) as u64);
    }
}
