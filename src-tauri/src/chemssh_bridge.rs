use std::collections::HashMap;
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use core_config::Profile;
use core_ssh::fetch_local_identity;
use serde::{Deserialize, Serialize};
use tokio::sync::{Mutex, Notify};

use crate::commands::backend::{open_path_in_system, open_text_file_in_system};
use crate::commands::sessions::ActiveSession;
use crate::commands::sftp::{cache_sftp_file_for_open, SftpManager};
use crate::commands::sftp_sync::{OpenSyncEvent, SftpOpenSyncManager};
use crate::state::{AppState, LauncherLogStore};

const CHEMSSH_BRIDGE_SFTP_WARM_DELAY: Duration = Duration::from_secs(2);

#[derive(Debug, Clone, Serialize)]
pub struct ChemSshBridgeCapabilities {
    pub enabled: bool,
    pub version: u32,
    pub launcher: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub session_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub workspace_root: String,
    pub features: ChemSshBridgeFeatures,
    pub endpoints: ChemSshBridgeEndpoints,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct ChemSshBridgeFeatures {
    pub system_icons: bool,
    pub open_default: bool,
    pub open_text: bool,
    pub open_sync_events: bool,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct ChemSshBridgeEndpoints {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub icon: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub open: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub open_text: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub sync_events: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ChemSshBridgeOpenRequest {
    pub path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ChemSshBridgeOpenResponse {
    pub ok: bool,
    pub remote_path: String,
    pub local_path: String,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct ChemSshBridgeSyncEvent {
    pub seq: u64,
    pub time: String,
    pub remote_path: String,
    pub local_path: String,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

impl From<OpenSyncEvent> for ChemSshBridgeSyncEvent {
    fn from(ev: OpenSyncEvent) -> Self {
        Self {
            seq: ev.seq,
            time: ev.time,
            remote_path: ev.remote_path,
            local_path: ev.local_path,
            status: ev.status,
            error: ev.error,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BridgeHttpError {
    pub status: u16,
    pub message: String,
}

impl BridgeHttpError {
    pub fn new(status: u16, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
        }
    }
}

impl std::fmt::Display for BridgeHttpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

struct ConnectInFlight {
    workspace_root: String,
    done: Notify,
    outcome: Mutex<Option<Result<String, BridgeHttpError>>>,
}

#[derive(Clone)]
enum ProfileBridgeState {
    Connected {
        sftp_session_id: String,
        workspace_root: String,
    },
    Connecting(Arc<ConnectInFlight>),
}

#[derive(Default)]
struct BridgeInner {
    by_profile: HashMap<String, ProfileBridgeState>,
    last_profile_id: Option<String>,
}

#[derive(Default)]
pub struct ChemSshBridgeManager {
    inner: Mutex<BridgeInner>,
}

impl ChemSshBridgeManager {
    pub fn new() -> Self {
        Self::default()
    }

    pub(crate) fn spawn_warm_sftp(
        self: &Arc<Self>,
        state: AppState,
        session: Arc<ActiveSession>,
        profile: Profile,
        workspace_root: String,
    ) {
        if profile.is_local() || workspace_root.trim().is_empty() {
            return;
        }
        let bridge = Arc::clone(self);
        tokio::spawn(async move {
            tokio::time::sleep(CHEMSSH_BRIDGE_SFTP_WARM_DELAY).await;
            if !state.sessions.is_same_session(&session).await || !session.is_forwarding().await {
                return;
            }
            match bridge
                .ensure_sftp_session(&state, &session, &profile, &workspace_root)
                .await
            {
                Ok(_) => {
                    state
                        .sessions
                        .push_log(
                            &profile.id,
                            &profile.name,
                            &state.launcher_logs,
                            format!("ChemSSH bridge SFTP preconnected for {}", profile.name),
                        )
                        .await;
                }
                Err(err) => {
                    state
                        .sessions
                        .push_log(
                            &profile.id,
                            &profile.name,
                            &state.launcher_logs,
                            format!("warning: ChemSSH bridge SFTP preconnect failed: {err}"),
                        )
                        .await;
                }
            }
        });
    }

    pub(crate) async fn capabilities(
        &self,
        state: &AppState,
        session: Option<&Arc<ActiveSession>>,
    ) -> ChemSshBridgeCapabilities {
        let Ok((profile, workspace_root)) = resolve_bridge_context(state, session, false).await
        else {
            return build_capabilities(false, false, false, "", "", false);
        };
        let is_local = profile.is_local();
        build_capabilities(
            true,
            true,
            !is_local,
            &profile.id,
            &workspace_root,
            is_local,
        )
    }

    pub(crate) async fn open_file(
        &self,
        state: &AppState,
        session: Option<&Arc<ActiveSession>>,
        raw_path: &str,
        text_mode: bool,
    ) -> Result<ChemSshBridgeOpenResponse, BridgeHttpError> {
        let Some(sess) = session else {
            return Err(BridgeHttpError::new(
                503,
                "chemssh service is not available; start forwarding first",
            ));
        };
        let (profile, workspace_root) = resolve_bridge_context(state, Some(sess), true).await?;

        if profile.is_local() {
            let local_path = validate_bridge_local_path(raw_path, &workspace_root)?;
            let local_str = local_path.display().to_string();
            if text_mode {
                open_text_file_in_system(&local_path).map_err(|e| BridgeHttpError::new(502, e))?;
                state
                    .sessions
                    .push_log(
                        &profile.id,
                        &profile.name,
                        &state.launcher_logs,
                        format!("ChemSSH bridge open text local file: {local_str}"),
                    )
                    .await;
            } else {
                open_path_in_system(&local_path).map_err(|e| BridgeHttpError::new(502, e))?;
                state
                    .sessions
                    .push_log(
                        &profile.id,
                        &profile.name,
                        &state.launcher_logs,
                        format!("ChemSSH bridge open local file: {local_str}"),
                    )
                    .await;
            }
            return Ok(ChemSshBridgeOpenResponse {
                ok: true,
                remote_path: clean_remote_path(raw_path),
                local_path: local_str,
            });
        }

        let cleaned_remote = validate_bridge_remote_path(raw_path, &workspace_root)?;
        let sftp_session_id = self
            .ensure_sftp_session(state, sess, &profile, &workspace_root)
            .await?;
        let cleaned_remote = validate_bridge_remote_path(&cleaned_remote, &workspace_root)?;

        let cache_path = match cache_sftp_file_for_open(
            state,
            &sftp_session_id,
            &cleaned_remote,
            Some(&workspace_root),
        )
        .await
        {
            Ok(p) => p,
            Err(err) => {
                // Evict stale dedicated SFTP session if the underlying channel dropped.
                if is_sftp_transport_failure(&err) {
                    self.evict_if_session_id(
                        &profile.id,
                        &sftp_session_id,
                        &state.sftp,
                        &state.sftp_sync,
                    )
                    .await;
                }
                return Err(BridgeHttpError::new(sftp_error_status(&err), err));
            }
        };

        let local_str = cache_path.display().to_string();
        if text_mode {
            open_text_file_in_system(&cache_path).map_err(|e| BridgeHttpError::new(502, e))?;
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!(
                        "ChemSSH bridge open text cached file: {cleaned_remote} -> {local_str}"
                    ),
                )
                .await;
        } else {
            open_path_in_system(&cache_path).map_err(|e| BridgeHttpError::new(502, e))?;
            state
                .sessions
                .push_log(
                    &profile.id,
                    &profile.name,
                    &state.launcher_logs,
                    format!("ChemSSH bridge open cached file: {cleaned_remote} -> {local_str}"),
                )
                .await;
        }

        Ok(ChemSshBridgeOpenResponse {
            ok: true,
            remote_path: cleaned_remote,
            local_path: local_str,
        })
    }

    pub(crate) async fn open_sync_events(
        &self,
        state: &AppState,
        session: Option<&Arc<ActiveSession>>,
        after_seq: u64,
    ) -> Vec<ChemSshBridgeSyncEvent> {
        let sftp_session_id = {
            let inner = self.inner.lock().await;
            let target_pid = session
                .map(|s| s.profile_id.as_str())
                .or(inner.last_profile_id.as_deref());
            target_pid.and_then(|pid| match inner.by_profile.get(pid) {
                Some(ProfileBridgeState::Connected {
                    sftp_session_id, ..
                }) => Some(sftp_session_id.clone()),
                _ => None,
            })
        };

        let Some(sid) = sftp_session_id else {
            return Vec::new();
        };
        state
            .sftp_sync
            .events_after_for_session(&sid, after_seq)
            .await
            .into_iter()
            .map(ChemSshBridgeSyncEvent::from)
            .collect()
    }

    pub(crate) async fn ensure_sftp_session(
        &self,
        state: &AppState,
        session: &Arc<ActiveSession>,
        profile: &Profile,
        workspace_root: &str,
    ) -> Result<String, BridgeHttpError> {
        let cleaned_ws = clean_remote_path(workspace_root);
        if cleaned_ws.is_empty() {
            return Err(BridgeHttpError::new(
                503,
                "ChemSSH identity did not include workspace_root",
            ));
        }

        enum Action {
            Reuse(String),
            Wait(Arc<ConnectInFlight>),
            Connect(Arc<ConnectInFlight>, Option<String>),
        }

        loop {
            let action = {
                let mut inner = self.inner.lock().await;
                match inner.by_profile.get(&profile.id).cloned() {
                    Some(ProfileBridgeState::Connected {
                        sftp_session_id,
                        workspace_root: existing_ws,
                    }) => {
                        if existing_ws == cleaned_ws
                            && state.sftp.is_connected(&sftp_session_id).await
                        {
                            inner.last_profile_id = Some(profile.id.clone());
                            Action::Reuse(sftp_session_id)
                        } else {
                            let flight = Arc::new(ConnectInFlight {
                                workspace_root: cleaned_ws.clone(),
                                done: Notify::new(),
                                outcome: Mutex::new(None),
                            });
                            inner.by_profile.insert(
                                profile.id.clone(),
                                ProfileBridgeState::Connecting(Arc::clone(&flight)),
                            );
                            Action::Connect(flight, Some(sftp_session_id))
                        }
                    }
                    Some(ProfileBridgeState::Connecting(flight))
                        if flight.workspace_root == cleaned_ws =>
                    {
                        Action::Wait(flight)
                    }
                    Some(ProfileBridgeState::Connecting(_)) | None => {
                        let flight = Arc::new(ConnectInFlight {
                            workspace_root: cleaned_ws.clone(),
                            done: Notify::new(),
                            outcome: Mutex::new(None),
                        });
                        inner.by_profile.insert(
                            profile.id.clone(),
                            ProfileBridgeState::Connecting(Arc::clone(&flight)),
                        );
                        Action::Connect(flight, None)
                    }
                }
            };

            match action {
                Action::Reuse(sid) => return Ok(sid),
                Action::Wait(flight) => {
                    let notified = flight.done.notified();
                    if let Some(res) = flight.outcome.lock().await.clone() {
                        return res;
                    }
                    notified.await;
                    if let Some(res) = flight.outcome.lock().await.clone() {
                        return res;
                    }
                    // Loop and re-evaluate if superseded before completion.
                    continue;
                }
                Action::Connect(flight, stale_sid) => {
                    if let Some(old_sid) = stale_sid {
                        state.sftp_sync.unregister_session(&old_sid).await;
                        state.sftp.disconnect_session(&old_sid).await;
                        state
                            .sessions
                            .push_log(
                                &profile.id,
                                &profile.name,
                                &state.launcher_logs,
                                "ChemSSH bridge SFTP disconnected",
                            )
                            .await;
                    }
                    return self
                        .connect_dedicated_for_flight(state, session, profile, &flight)
                        .await;
                }
            }
        }
    }

    async fn connect_dedicated_for_flight(
        &self,
        state: &AppState,
        session: &Arc<ActiveSession>,
        profile: &Profile,
        flight: &Arc<ConnectInFlight>,
    ) -> Result<String, BridgeHttpError> {
        let connect_res = state.sftp.connect_dedicated(state, profile).await;
        let new_sid = match connect_res {
            Ok(sid) => sid,
            Err(err) => {
                let berr = BridgeHttpError::new(502, err);
                let _ = self
                    .finish_flight(&profile.id, flight, Err(berr.clone()))
                    .await;
                return Err(berr);
            }
        };

        let still_valid =
            state.sessions.is_same_session(session).await && session.is_forwarding().await;
        if !still_valid {
            state.sftp.disconnect_session(&new_sid).await;
            let berr = BridgeHttpError::new(
                503,
                "active ChemSSH session changed while opening bridge SFTP session",
            );
            let _ = self
                .finish_flight(&profile.id, flight, Err(berr.clone()))
                .await;
            return Err(berr);
        }

        if let Err(err) = self
            .finish_flight(&profile.id, flight, Ok(new_sid.clone()))
            .await
        {
            state.sftp.disconnect_session(&new_sid).await;
            return Err(err);
        }

        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                format!("ChemSSH bridge SFTP connection OK for {}", profile.name),
            )
            .await;
        Ok(new_sid)
    }

    async fn finish_flight(
        &self,
        profile_id: &str,
        flight: &Arc<ConnectInFlight>,
        result: Result<String, BridgeHttpError>,
    ) -> Result<(), BridgeHttpError> {
        let mut inner = self.inner.lock().await;
        let same_flight = matches!(
            inner.by_profile.get(profile_id),
            Some(ProfileBridgeState::Connecting(cur)) if Arc::ptr_eq(cur, flight)
        );
        if !same_flight {
            let err = match result {
                Ok(_) => BridgeHttpError::new(
                    503,
                    "active ChemSSH bridge changed while opening SFTP session",
                ),
                Err(e) => e,
            };
            *flight.outcome.lock().await = Some(Err(err.clone()));
            flight.done.notify_waiters();
            return Err(err);
        }

        match &result {
            Ok(sid) => {
                inner.by_profile.insert(
                    profile_id.to_string(),
                    ProfileBridgeState::Connected {
                        sftp_session_id: sid.clone(),
                        workspace_root: flight.workspace_root.clone(),
                    },
                );
                inner.last_profile_id = Some(profile_id.to_string());
                *flight.outcome.lock().await = Some(Ok(sid.clone()));
                flight.done.notify_waiters();
                Ok(())
            }
            Err(err) => {
                inner.by_profile.remove(profile_id);
                *flight.outcome.lock().await = Some(Err(err.clone()));
                flight.done.notify_waiters();
                Err(err.clone())
            }
        }
    }

    async fn evict_if_session_id(
        &self,
        profile_id: &str,
        target_sid: &str,
        sftp: &SftpManager,
        sftp_sync: &SftpOpenSyncManager,
    ) {
        let removed = {
            let mut inner = self.inner.lock().await;
            match inner.by_profile.get(profile_id) {
                Some(ProfileBridgeState::Connected {
                    sftp_session_id, ..
                }) if sftp_session_id == target_sid => {
                    inner.by_profile.remove(profile_id);
                    true
                }
                _ => false,
            }
        };
        if removed {
            sftp_sync.unregister_session(target_sid).await;
            sftp.disconnect_session(target_sid).await;
        }
    }

    pub(crate) async fn close_for_profile(
        &self,
        profile_id: &str,
        sftp: &SftpManager,
        sftp_sync: &SftpOpenSyncManager,
        launcher_logs: &LauncherLogStore,
    ) {
        let removed = {
            let mut inner = self.inner.lock().await;
            if inner.last_profile_id.as_deref() == Some(profile_id) {
                inner.last_profile_id = None;
            }
            inner.by_profile.remove(profile_id)
        };
        if let Some(ProfileBridgeState::Connected {
            sftp_session_id, ..
        }) = removed
        {
            sftp_sync.unregister_session(&sftp_session_id).await;
            sftp.disconnect_session(&sftp_session_id).await;
            launcher_logs.push(format!("[{profile_id}] ChemSSH bridge SFTP disconnected"));
        }
    }

    pub async fn close_all(
        &self,
        sftp: &SftpManager,
        sftp_sync: &SftpOpenSyncManager,
        launcher_logs: &LauncherLogStore,
    ) {
        let entries: Vec<(String, ProfileBridgeState)> = {
            let mut inner = self.inner.lock().await;
            inner.last_profile_id = None;
            inner.by_profile.drain().collect()
        };
        for (pid, st) in entries {
            if let ProfileBridgeState::Connected {
                sftp_session_id, ..
            } = st
            {
                sftp_sync.unregister_session(&sftp_session_id).await;
                sftp.disconnect_session(&sftp_session_id).await;
                launcher_logs.push(format!("[{pid}] ChemSSH bridge SFTP disconnected"));
            }
        }
    }
}

pub fn build_capabilities(
    enabled: bool,
    open_available: bool,
    sync_available: bool,
    session_id: &str,
    workspace_root: &str,
    local: bool,
) -> ChemSshBridgeCapabilities {
    let features = ChemSshBridgeFeatures {
        system_icons: enabled,
        open_default: enabled && open_available,
        open_text: enabled && open_available,
        open_sync_events: enabled && sync_available,
    };
    let mut endpoints = ChemSshBridgeEndpoints::default();
    if enabled {
        endpoints.icon = "/api/file-icon".into();
        if open_available {
            endpoints.open = "/api/chemssh-bridge/open".into();
            endpoints.open_text = "/api/chemssh-bridge/open-text".into();
        }
        if sync_available {
            endpoints.sync_events = "/api/chemssh-bridge/open-sync-events".into();
        }
    }
    let cleaned_workspace = if local {
        clean_local_workspace(workspace_root)
    } else {
        clean_remote_path(workspace_root)
    };
    ChemSshBridgeCapabilities {
        enabled,
        version: 1,
        launcher: "chemssh-launcher".into(),
        session_id: session_id.to_string(),
        workspace_root: cleaned_workspace,
        features,
        endpoints,
    }
}

async fn resolve_bridge_context(
    state: &AppState,
    session: Option<&Arc<ActiveSession>>,
    require_workspace: bool,
) -> Result<(Profile, String), BridgeHttpError> {
    let Some(sess) = session else {
        return Err(BridgeHttpError::new(
            503,
            "chemssh service is not available; start forwarding first",
        ));
    };
    if !sess.is_forwarding().await {
        return Err(BridgeHttpError::new(
            503,
            "chemssh service is not available; start forwarding first",
        ));
    }

    let profile = sess.profile().clone();
    let cached_ws = clean_bridge_workspace(&profile, &sess.workspace_root().await);
    if !cached_ws.is_empty() {
        return Ok((profile, cached_ws));
    }
    if !require_workspace && !profile.is_local() {
        return Ok((profile, String::new()));
    }

    let token = sess.security_token().map(str::to_string);
    if profile.is_local() {
        let identity =
            match fetch_local_identity(&profile.local_host, profile.local_port, token.as_deref())
                .await
            {
                Ok(id) => id,
                Err(err) => {
                    state
                        .sessions
                        .push_log(
                            &profile.id,
                            &profile.name,
                            &state.launcher_logs,
                            format!(
                                "warning: could not read local ChemSSH identity for bridge: {err}"
                            ),
                        )
                        .await;
                    if !require_workspace {
                        return Ok((profile, String::new()));
                    }
                    return Err(BridgeHttpError::new(503, err.to_string()));
                }
            };

        let ws = clean_local_workspace(&identity.workspace_root);
        if ws.is_empty() {
            if !require_workspace {
                return Ok((profile, String::new()));
            }
            return Err(BridgeHttpError::new(
                503,
                "local ChemSSH identity did not include workspace_root",
            ));
        }
        if !state.sessions.is_same_session(sess).await || !sess.is_forwarding().await {
            return Err(BridgeHttpError::new(
                503,
                "active ChemSSH session changed while reading identity",
            ));
        }
        sess.set_identity(identity.pid, ws.clone()).await;
        state
            .sessions
            .push_log(
                &profile.id,
                &profile.name,
                &state.launcher_logs,
                format!("ChemSSH bridge local identity OK: workspace {ws}"),
            )
            .await;
        return Ok((profile, ws));
    }

    let Some(conn) = sess.remote_connection() else {
        return Err(BridgeHttpError::new(
            503,
            "active ChemSSH session has no SSH client",
        ));
    };
    let identity = conn
        .fetch_remote_identity(&profile.remote_host, profile.remote_port, token.as_deref())
        .await
        .map_err(|err| BridgeHttpError::new(502, err.to_string()))?;

    let ws = clean_remote_path(&identity.workspace_root);
    if ws.is_empty() {
        return Err(BridgeHttpError::new(
            503,
            "remote ChemSSH identity did not include workspace_root",
        ));
    }
    if !state.sessions.is_same_session(sess).await || !sess.is_forwarding().await {
        return Err(BridgeHttpError::new(
            503,
            "active ChemSSH session changed while reading identity",
        ));
    }
    sess.set_identity(identity.pid, ws.clone()).await;
    state
        .sessions
        .push_log(
            &profile.id,
            &profile.name,
            &state.launcher_logs,
            format!("ChemSSH bridge identity OK: workspace {ws}"),
        )
        .await;
    Ok((profile, ws))
}

fn clean_bridge_workspace(profile: &Profile, value: &str) -> String {
    if profile.is_local() {
        clean_local_workspace(value)
    } else {
        clean_remote_path(value)
    }
}

fn clean_local_workspace(value: &str) -> String {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    let cleaned = lexical_clean_path(Path::new(trimmed));
    let s = cleaned.to_string_lossy().into_owned();
    if s == "." {
        String::new()
    } else {
        s
    }
}

pub fn clean_remote_path(value: &str) -> String {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    let rooted = trimmed.starts_with('/');
    let mut stack: Vec<&str> = Vec::new();
    for seg in trimmed.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                if rooted {
                    stack.pop();
                } else if matches!(stack.last(), Some(&last) if last != "..") {
                    stack.pop();
                } else {
                    stack.push("..");
                }
            }
            other => stack.push(other),
        }
    }
    if rooted {
        if stack.is_empty() {
            "/".to_string()
        } else {
            format!("/{}", stack.join("/"))
        }
    } else if stack.is_empty() {
        String::new()
    } else {
        stack.join("/")
    }
}

pub fn validate_bridge_remote_path(
    remote_path: &str,
    workspace_root: &str,
) -> Result<String, BridgeHttpError> {
    let cleaned = clean_remote_path(remote_path);
    let root = clean_remote_path(workspace_root);
    if cleaned.is_empty() {
        return Err(BridgeHttpError::new(400, "missing remote path"));
    }
    if root.is_empty() {
        return Err(BridgeHttpError::new(
            503,
            "ChemSSH workspace root is not available",
        ));
    }
    if !cleaned.starts_with('/') {
        return Err(BridgeHttpError::new(400, "remote path must be absolute"));
    }
    if !root.starts_with('/') {
        return Err(BridgeHttpError::new(
            503,
            "ChemSSH workspace root must be absolute",
        ));
    }
    if root == "/" || cleaned == root || cleaned.starts_with(&format!("{root}/")) {
        return Ok(cleaned);
    }
    Err(BridgeHttpError::new(
        403,
        "remote path is outside the active ChemSSH workspace",
    ))
}

pub fn validate_bridge_local_path(
    value: &str,
    workspace_root: &str,
) -> Result<PathBuf, BridgeHttpError> {
    let ws_trimmed = workspace_root.trim();
    if ws_trimmed.is_empty() {
        return Err(BridgeHttpError::new(
            503,
            "ChemSSH workspace root is not available",
        ));
    }
    let val_trimmed = value.trim();
    if val_trimmed.is_empty() {
        return Err(BridgeHttpError::new(400, "missing local path"));
    }

    let ws_raw = PathBuf::from(ws_trimmed);
    let ws_abs = if ws_raw.is_absolute() {
        lexical_clean_path(&ws_raw)
    } else {
        let cwd = std::env::current_dir()
            .map_err(|e| BridgeHttpError::new(500, format!("resolve cwd: {e}")))?;
        lexical_clean_path(&cwd.join(ws_raw))
    };

    let candidate_raw = PathBuf::from(val_trimmed);
    let candidate_abs = if candidate_raw.is_absolute() {
        lexical_clean_path(&candidate_raw)
    } else {
        lexical_clean_path(&ws_abs.join(candidate_raw))
    };

    if is_path_within(&candidate_abs, &ws_abs) {
        Ok(candidate_abs)
    } else {
        Err(BridgeHttpError::new(
            403,
            "local path is outside the active ChemSSH workspace",
        ))
    }
}

fn lexical_clean_path(path: &Path) -> PathBuf {
    let mut parts: Vec<Component<'_>> = Vec::new();
    for comp in path.components() {
        match comp {
            Component::Prefix(_) | Component::RootDir => parts.push(comp),
            Component::CurDir => {}
            Component::ParentDir => {
                if matches!(parts.last(), Some(Component::Normal(_))) {
                    parts.pop();
                } else if !matches!(
                    parts.last(),
                    Some(Component::RootDir) | Some(Component::Prefix(_))
                ) {
                    parts.push(comp);
                }
            }
            Component::Normal(_) => parts.push(comp),
        }
    }
    if parts.is_empty() {
        PathBuf::from(".")
    } else {
        parts.into_iter().collect()
    }
}

fn is_path_within(candidate: &Path, root: &Path) -> bool {
    let cand_comps: Vec<Component<'_>> = candidate.components().collect();
    let root_comps: Vec<Component<'_>> = root.components().collect();
    if cand_comps.len() < root_comps.len() {
        return false;
    }
    root_comps
        .iter()
        .zip(cand_comps.iter())
        .all(|(r, c)| components_match(r, c))
}

fn components_match(a: &Component<'_>, b: &Component<'_>) -> bool {
    #[cfg(windows)]
    {
        match (a, b) {
            (Component::Normal(sa), Component::Normal(sb)) => sa
                .to_string_lossy()
                .eq_ignore_ascii_case(&sb.to_string_lossy()),
            _ => a == b,
        }
    }
    #[cfg(not(windows))]
    {
        a == b
    }
}

fn is_sftp_transport_failure(err: &str) -> bool {
    let lower = err.to_ascii_lowercase();
    lower.contains("sftp session not connected")
        || lower.contains("closed")
        || lower.contains("eof")
        || lower.contains("broken pipe")
        || lower.contains("connection reset")
}

fn sftp_error_status(err: &str) -> u16 {
    let lower = err.to_ascii_lowercase();
    if lower.contains("exceeds") || lower.contains("too large") {
        413
    } else if lower.contains("outside the chemssh workspace") {
        403
    } else if lower.contains("directory") || lower.contains("not a regular file") {
        400
    } else if lower.contains("not connected") || lower.contains("session was disconnected") {
        503
    } else {
        502
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::AppState;
    use core_secret::MemoryStore;
    use tempfile::TempDir;

    #[test]
    fn validates_remote_paths_within_workspace() {
        assert_eq!(
            validate_bridge_remote_path("/home/user/project", "/home/user/project").unwrap(),
            "/home/user/project"
        );
        assert_eq!(
            validate_bridge_remote_path("/home/user/project/input.inp", "/home/user/project/")
                .unwrap(),
            "/home/user/project/input.inp"
        );
        assert_eq!(
            validate_bridge_remote_path("/etc/hosts", "/").unwrap(),
            "/etc/hosts"
        );

        let sibling =
            validate_bridge_remote_path("/home/user/project-old/input.inp", "/home/user/project")
                .unwrap_err();
        assert_eq!(sibling.status, 403);

        let traversal =
            validate_bridge_remote_path("/home/user/project/../secret.txt", "/home/user/project")
                .unwrap_err();
        assert_eq!(traversal.status, 403);

        let relative = validate_bridge_remote_path("input.inp", "/home/user/project").unwrap_err();
        assert_eq!(relative.status, 400);

        let missing_root =
            validate_bridge_remote_path("/home/user/project/input.inp", "").unwrap_err();
        assert_eq!(missing_root.status, 503);
    }

    #[test]
    fn validates_local_paths_within_workspace() {
        let dir = TempDir::new().unwrap();
        let root = dir.path().join("ws");
        let root_str = root.to_string_lossy().into_owned();

        let child = root.join("sub").join("input.inp");
        let ok = validate_bridge_local_path(&child.to_string_lossy(), &root_str).unwrap();
        assert_eq!(ok, child);

        let rel_ok = validate_bridge_local_path("input.inp", &root_str).unwrap();
        assert_eq!(rel_ok, root.join("input.inp"));

        let escape = root.join("..").join("outside.inp");
        let err = validate_bridge_local_path(&escape.to_string_lossy(), &root_str).unwrap_err();
        assert_eq!(err.status, 403);
    }

    #[test]
    fn local_capabilities_keep_open_and_hide_sync_events() {
        let dir = TempDir::new().unwrap();
        let root = dir.path().join("local-ws");
        let with_sep = format!("{}{}", root.display(), std::path::MAIN_SEPARATOR);
        let caps = build_capabilities(true, true, false, "local-1", &with_sep, true);
        assert!(caps.enabled);
        assert_eq!(caps.session_id, "local-1");
        assert_eq!(caps.workspace_root, root.to_string_lossy().as_ref());
        assert!(caps.features.system_icons);
        assert!(caps.features.open_default);
        assert!(caps.features.open_text);
        assert!(!caps.features.open_sync_events);
        assert_eq!(caps.endpoints.icon, "/api/file-icon");
        assert_eq!(caps.endpoints.open, "/api/chemssh-bridge/open");
        assert_eq!(caps.endpoints.open_text, "/api/chemssh-bridge/open-text");
        assert!(caps.endpoints.sync_events.is_empty());
    }

    #[tokio::test]
    async fn bridge_open_sync_events_filters_to_bridge_session_and_omits_session_field() {
        let dir = TempDir::new().unwrap();
        let state = AppState::new_custom(
            dir.path().to_path_buf(),
            dir.path().join("profiles.json"),
            dir.path().join("settings.json"),
            dir.path().join("cache"),
            Arc::new(MemoryStore::new()),
        );

        state
            .sftp_sync
            .push_event(
                "sftp-tab-1",
                "/home/user/project/other.txt",
                "/cache/other.txt",
                "done",
                None,
            )
            .await;
        state
            .sftp_sync
            .push_event(
                "bridge-sftp-1",
                "/home/user/project/input.inp",
                "/cache/input.inp",
                "done",
                None,
            )
            .await;

        {
            let mut inner = state.chemssh_bridge.inner.lock().await;
            inner.by_profile.insert(
                "remote-1".into(),
                ProfileBridgeState::Connected {
                    sftp_session_id: "bridge-sftp-1".into(),
                    workspace_root: "/home/user/project".into(),
                },
            );
            inner.last_profile_id = Some("remote-1".into());
        }

        let events = state.chemssh_bridge.open_sync_events(&state, None, 0).await;
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].remote_path, "/home/user/project/input.inp");
        let serialized = serde_json::to_string(&events[0]).unwrap();
        assert!(!serialized.contains("\"session\""));
    }
}
