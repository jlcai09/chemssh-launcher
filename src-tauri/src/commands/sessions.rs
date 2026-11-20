use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::time::Duration;

use core_config::{proxy_env_pairs, Profile, ProfileStore};
use core_secret::{KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE};
use core_session::{check_port_available, suggest_free_ports, wait_for_url};
use core_ssh::{
    assemble_remote_command, port_warning, remove_proxy_bridge, write_proxy_bridge, HostKeyPolicy,
    HostKeyVerificationError, KnownHostsVerifier, LocalTunnel, ReverseProxyChannel, SshConnection,
    SshCredentials, SshError,
};
use serde::Serialize;
use tauri::ipc::Channel;
use tauri::State;
use tokio::sync::{broadcast, mpsc, Mutex};

use crate::state::AppState;

const MAX_LOG_LINES: usize = 2000;

#[derive(Debug, Clone, Serialize)]
pub struct LocalProxyStatus {
    pub enabled: bool,
    pub proxy_url: Option<String>,
    pub bridge_path: Option<String>,
    pub error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct SessionStatus {
    pub profile_id: String,
    pub running: bool,
    pub forwarding_active: bool,
    pub phase: String,
    pub local_url: String,
    pub warnings: Vec<String>,
    pub local_proxy: LocalProxyStatus,
    pub last_error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct LogBatchEvent {
    pub profile_id: String,
    pub lines: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ConnectionTestResult {
    pub ok: bool,
    pub message: String,
    pub host_key_error: Option<HostKeyVerificationError>,
}

struct ActiveSession {
    connection: Arc<SshConnection>,
    tunnel: Mutex<Option<LocalTunnel>>,
    reverse_proxy: Mutex<Option<ReverseProxyChannel>>,
    proxy_bridge_path: Mutex<Option<String>>,
    status: Mutex<SessionStatus>,
    logs: Mutex<VecDeque<String>>,
    log_tx: broadcast::Sender<String>,
}

impl ActiveSession {
    fn append_log(&self, line: String) {
        if let Ok(mut logs) = self.logs.try_lock() {
            if logs.len() >= MAX_LOG_LINES {
                logs.pop_front();
            }
            logs.push_back(line.clone());
        }
        let _ = self.log_tx.send(line);
    }

    async fn push_log(&self, line: String) {
        {
            let mut logs = self.logs.lock().await;
            if logs.len() >= MAX_LOG_LINES {
                logs.pop_front();
            }
            logs.push_back(line.clone());
        }
        let _ = self.log_tx.send(line);
    }
}

#[derive(Default)]
pub struct SessionManager {
    sessions: Mutex<HashMap<String, Arc<ActiveSession>>>,
}

impl SessionManager {
    pub async fn status(&self, profile_id: &str) -> Option<SessionStatus> {
        let session = self.sessions.lock().await.get(profile_id).cloned()?;
        let snapshot = session.status.lock().await.clone();
        Some(snapshot)
    }

    pub async fn all_statuses(&self) -> Vec<SessionStatus> {
        let sessions: Vec<Arc<ActiveSession>> = self.sessions.lock().await.values().cloned().collect();
        let mut out = Vec::with_capacity(sessions.len());
        for s in sessions {
            out.push(s.status.lock().await.clone());
        }
        out
    }

    pub async fn logs(&self, profile_id: &str) -> Vec<String> {
        let Some(session) = self.sessions.lock().await.get(profile_id).cloned() else {
            return Vec::new();
        };
        let snapshot = session.logs.lock().await.iter().cloned().collect();
        snapshot
    }

    pub async fn subscribe_logs(&self, profile_id: &str) -> Option<broadcast::Receiver<String>> {
        let session = self.sessions.lock().await.get(profile_id).cloned()?;
        Some(session.log_tx.subscribe())
    }
}

pub(crate) fn load_ssh_credentials(state: &AppState, profile: &Profile) -> Result<SshCredentials, String> {
    let password = state
        .secrets
        .get(&profile.id, KEY_PASSWORD)
        .map_err(|e| e.to_string())?;
    let private_key_passphrase = state
        .secrets
        .get(&profile.id, KEY_PRIVATE_PASSPHRASE)
        .map_err(|e| e.to_string())?;
    Ok(SshCredentials {
        password,
        private_key_passphrase,
    })
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
            host_key_error: None,
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
                host_key_error: None,
            })
        }
        Err(SshError::HostKeyVerification(err)) => Ok(ConnectionTestResult {
            ok: false,
            message: err.to_string(),
            host_key_error: Some(err),
        }),
        Err(err) => Ok(ConnectionTestResult {
            ok: false,
            message: err.to_string(),
            host_key_error: None,
        }),
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

    if let Some(existing) = state.sessions.status(&profile.id).await {
        if existing.running {
            return Ok(existing);
        }
    }

    if let Err(err) = check_port_available(&profile.local_host, profile.local_port) {
        let suggestions = suggest_free_ports(&profile.local_host, profile.local_port, 3);
        return Err(format!("{err}; suggested free ports: {suggestions:?}"));
    }

    let mut warnings = profile.warnings();
    if let Some(pw) = port_warning(&profile) {
        warnings.push(pw);
    }

    let creds = load_ssh_credentials(&state, &profile)?;
    let verifier = KnownHostsVerifier::new(
        &state.known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );

    let connection = Arc::new(
        SshConnection::connect(&profile, &creds, verifier)
            .await
            .map_err(|e| e.to_string())?,
    );

    // Optional local network proxy: failure must degrade with warning, never fail the session.
    let mut proxy_env = Vec::new();
    let mut reverse_proxy: Option<ReverseProxyChannel> = None;
    let mut proxy_bridge_path: Option<String> = None;
    let mut local_proxy_status = LocalProxyStatus {
        enabled: profile.proxy_active(),
        proxy_url: None,
        bridge_path: None,
        error: None,
    };

    if profile.proxy_active() {
        let lp = profile.local_proxy.normalized();
        match connection.start_reverse_proxy(lp.address()).await {
            Ok(channel) => {
                let proxy_url = channel.proxy_url.clone();
                proxy_env = proxy_env_pairs(&proxy_url, &lp.no_proxy);
                local_proxy_status.proxy_url = Some(proxy_url.clone());

                match connection.open_sftp().await {
                    Ok(sftp) => {
                        match write_proxy_bridge(&sftp, profile.remote_port, &proxy_url, &lp.no_proxy).await {
                            Ok(path) => {
                                local_proxy_status.bridge_path = Some(path.clone());
                                proxy_bridge_path = Some(path);
                            }
                            Err(err) => {
                                let msg = format!("proxy bridge write failed (degraded): {err}");
                                warnings.push(msg.clone());
                                local_proxy_status.error = Some(msg);
                            }
                        }
                    }
                    Err(err) => {
                        let msg = format!("proxy bridge sftp open failed (degraded): {err}");
                        warnings.push(msg.clone());
                        local_proxy_status.error = Some(msg);
                    }
                }
                reverse_proxy = Some(channel);
            }
            Err(err) => {
                let msg = format!("reverse proxy channel failed (degraded): {err}");
                warnings.push(msg.clone());
                local_proxy_status.error = Some(msg);
            }
        }
    }

    let tunnel = connection
        .start_local_tunnel(
            &profile.local_host,
            profile.local_port,
            profile.remote_host.clone(),
            profile.remote_port,
        )
        .await
        .map_err(|e| e.to_string())?;

    let initial_status = SessionStatus {
        profile_id: profile.id.clone(),
        running: true,
        forwarding_active: true,
        phase: "starting".into(),
        local_url: profile.browser_url(),
        warnings: warnings.clone(),
        local_proxy: local_proxy_status,
        last_error: None,
    };

    let (log_tx, _) = broadcast::channel(512);
    let active = Arc::new(ActiveSession {
        connection: Arc::clone(&connection),
        tunnel: Mutex::new(Some(tunnel)),
        reverse_proxy: Mutex::new(reverse_proxy),
        proxy_bridge_path: Mutex::new(proxy_bridge_path),
        status: Mutex::new(initial_status.clone()),
        logs: Mutex::new(VecDeque::with_capacity(256)),
        log_tx,
    });

    for w in &warnings {
        active.push_log(format!("[warn] {w}")).await;
    }

    state
        .sessions
        .sessions
        .lock()
        .await
        .insert(profile.id.clone(), Arc::clone(&active));

    // Start remote ChemSSH command in background and stream output to session logs.
    let remote_cmd = assemble_remote_command(&profile, &proxy_env);
    let exec_session = Arc::clone(&active);
    let exec_conn = Arc::clone(&connection);
    tokio::spawn(async move {
        let (out_tx, mut out_rx) = mpsc::channel::<String>(256);
        let log_session = Arc::clone(&exec_session);
        tokio::spawn(async move {
            while let Some(chunk) = out_rx.recv().await {
                for line in chunk.lines() {
                    log_session.push_log(line.to_string()).await;
                }
            }
        });
        let result = exec_conn.exec_streaming(&remote_cmd, out_tx).await;
        match result {
            Ok(code) => {
                exec_session
                    .push_log(format!("[session] remote command exited with status {code:?}"))
                    .await;
            }
            Err(err) => {
                exec_session
                    .push_log(format!("[session] remote command error: {err}"))
                    .await;
            }
        }
        let mut st = exec_session.status.lock().await;
        st.running = false;
        st.forwarding_active = false;
        st.phase = "stopped".into();
    });

    // Wait briefly for health URL; if not yet ready, keep session running in starting phase.
    let health_url = profile.health_url();
    let health_session = Arc::clone(&active);
    tokio::spawn(async move {
        match wait_for_url(&health_url, Duration::from_secs(30), Duration::from_millis(400)).await {
            Ok(()) => {
                let mut st = health_session.status.lock().await;
                if st.running {
                    st.phase = "ready".into();
                }
                health_session.append_log(format!("[session] ready at {health_url}"));
            }
            Err(err) => {
                let mut st = health_session.status.lock().await;
                if st.running {
                    st.last_error = Some(err.to_string());
                }
                health_session.append_log(format!("[warn] health check: {err}"));
            }
        }
    });

    Ok(initial_status)
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

    if let Some(tunnel) = session.tunnel.lock().await.take() {
        tunnel.stop();
    }
    let mut status = session.status.lock().await;
    status.forwarding_active = false;
    status.phase = "paused".into();
    Ok(status.clone())
}

/// Full stop: closes local tunnel, closes reverse proxy channel, removes remote bridge file, disconnects SSH.
#[tauri::command]
pub async fn stop_session(state: State<'_, AppState>, profile_id: String) -> Result<(), String> {
    let session = state.sessions.sessions.lock().await.remove(&profile_id);
    let Some(session) = session else {
        return Ok(());
    };

    if let Some(tunnel) = session.tunnel.lock().await.take() {
        tunnel.stop();
    }
    if let Some(mut reverse) = session.reverse_proxy.lock().await.take() {
        reverse.close().await;
    }
    if let Some(bridge_path) = session.proxy_bridge_path.lock().await.take() {
        if let Ok(sftp) = session.connection.open_sftp().await {
            let _ = remove_proxy_bridge(&sftp, &bridge_path).await;
        }
    }
    session.connection.disconnect().await;

    let mut status = session.status.lock().await;
    status.running = false;
    status.forwarding_active = false;
    status.phase = "stopped".into();
    Ok(())
}

#[tauri::command]
pub async fn get_session_status(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<Option<SessionStatus>, String> {
    Ok(state.sessions.status(&profile_id).await)
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
pub async fn stream_session_logs(
    state: State<'_, AppState>,
    profile_id: String,
    on_event: Channel<LogBatchEvent>,
) -> Result<(), String> {
    let snapshot = state.sessions.logs(&profile_id).await;
    if !snapshot.is_empty() {
        let _ = on_event.send(LogBatchEvent {
            profile_id: profile_id.clone(),
            lines: snapshot,
        });
    }
    let Some(mut rx) = state.sessions.subscribe_logs(&profile_id).await else {
        return Ok(());
    };
    tokio::spawn(async move {
        let mut batch = Vec::new();
        let mut tick = tokio::time::interval(Duration::from_millis(80));
        loop {
            tokio::select! {
                res = rx.recv() => {
                    match res {
                        Ok(line) => batch.push(line),
                        Err(broadcast::error::RecvError::Lagged(_)) => continue,
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }
                _ = tick.tick(), if !batch.is_empty() => {
                    let lines = std::mem::take(&mut batch);
                    if on_event.send(LogBatchEvent { profile_id: profile_id.clone(), lines }).is_err() {
                        break;
                    }
                }
            }
        }
    });
    Ok(())
}
