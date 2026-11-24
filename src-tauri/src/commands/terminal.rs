use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use core_config::{new_id, proxy_env_pairs, ProfileStore};
use core_ssh::{
    prepare_local_x_server, proxy_login_shell_command, HostKeyPolicy, KnownHostsVerifier,
    PtyShellSession, ReverseProxyChannel, SendInputResult, ShellIntegration, SshConnection,
    TerminalEvent,
};
use serde::Serialize;
use tauri::ipc::Channel;
use tauri::State;
use tokio::sync::Mutex;

use crate::commands::sessions::{format_ssh_error, load_ssh_credentials};
use crate::state::AppState;

const MAX_TERMINAL_EVENTS: usize = 4096;
const MAX_TERMINAL_BUFFER_BYTES: usize = 2 * 1024 * 1024;
const EXITED_SESSION_GRACE: Duration = Duration::from_secs(15);

#[derive(Debug, Clone, Serialize)]
pub struct WireTerminalEvent {
    pub kind: &'static str,
    pub seq: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data_b64: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ShellOpenResponse {
    pub id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub proxy_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub proxy_error: Option<String>,
    pub x11_enabled: bool,
    pub x11_error: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct TerminalCwdResponse {
    pub path: String,
}

struct TerminalEventBuffer {
    events: VecDeque<WireTerminalEvent>,
    next_seq: u64,
    total_b64_bytes: usize,
}

impl Default for TerminalEventBuffer {
    fn default() -> Self {
        Self {
            events: VecDeque::new(),
            next_seq: 1,
            total_b64_bytes: 0,
        }
    }
}

impl TerminalEventBuffer {
    fn push(
        &mut self,
        kind: &'static str,
        data_b64: Option<String>,
        exit_code: Option<u32>,
        reason: Option<String>,
    ) -> WireTerminalEvent {
        let added_bytes = data_b64.as_ref().map(|s| s.len()).unwrap_or(0);
        let ev = WireTerminalEvent {
            kind,
            seq: self.next_seq,
            data_b64,
            exit_code,
            reason,
        };
        self.next_seq = self.next_seq.saturating_add(1);
        self.total_b64_bytes = self.total_b64_bytes.saturating_add(added_bytes);
        self.events.push_back(ev.clone());

        while self.events.len() > MAX_TERMINAL_EVENTS
            || self.total_b64_bytes > MAX_TERMINAL_BUFFER_BYTES
        {
            if self.events.len() <= 1 {
                break;
            }
            if let Some(evicted) = self.events.pop_front() {
                let sz = evicted.data_b64.as_ref().map(|s| s.len()).unwrap_or(0);
                self.total_b64_bytes = self.total_b64_bytes.saturating_sub(sz);
            }
        }
        ev
    }

    fn events_after(&self, cursor: u64) -> Vec<WireTerminalEvent> {
        self.events
            .iter()
            .filter(|ev| ev.seq > cursor)
            .cloned()
            .collect()
    }
}

struct ActiveTerminal {
    connection: Arc<SshConnection>,
    shell: PtyShellSession,
    integration: Option<ShellIntegration>,
    reverse_proxy: Mutex<Option<ReverseProxyChannel>>,
    events: Mutex<TerminalEventBuffer>,
}

impl ActiveTerminal {
    async fn push_wire_event(
        &self,
        kind: &'static str,
        data_b64: Option<String>,
        exit_code: Option<u32>,
        reason: Option<String>,
    ) -> WireTerminalEvent {
        let mut guard = self.events.lock().await;
        guard.push(kind, data_b64, exit_code, reason)
    }

    async fn events_after(&self, cursor: u64) -> Vec<WireTerminalEvent> {
        let guard = self.events.lock().await;
        guard.events_after(cursor)
    }

    async fn cleanup_resources(&self) {
        self.shell.close();
        if let Some(ref integ) = self.integration {
            self.connection.cleanup_shell_integration(integ).await;
        }
        if let Some(mut rp) = self.reverse_proxy.lock().await.take() {
            rp.close().await;
        }
        self.connection.disconnect().await;
    }
}

#[derive(Default)]
pub struct TerminalManager {
    terminals: Mutex<HashMap<String, Arc<ActiveTerminal>>>,
}

impl TerminalManager {
    pub async fn close_all(&self) {
        let active_list: Vec<Arc<ActiveTerminal>> = {
            let mut guard = self.terminals.lock().await;
            guard.drain().map(|(_, v)| v).collect()
        };
        for active in active_list {
            active.cleanup_resources().await;
        }
    }
}

#[tauri::command]
pub async fn open_terminal(
    state: State<'_, AppState>,
    profile_id: String,
    cols: Option<u32>,
    rows: Option<u32>,
    host_key_policy: Option<HostKeyPolicy>,
    on_event: Channel<WireTerminalEvent>,
) -> Result<ShellOpenResponse, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    if profile.is_local() {
        return Err("local profiles do not support SSH terminals".into());
    }

    let t_start = Instant::now();
    let creds = load_ssh_credentials(&state, &profile)?;
    let verifier = KnownHostsVerifier::new(
        &state.known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );

    // Run local X11 verification concurrently with the SSH TCP/KEX/auth handshake.
    let (connect_res, x11_res) = tokio::join!(
        SshConnection::connect(&profile, &creds, verifier),
        async {
            if profile.x11_active() {
                Some(prepare_local_x_server(&profile.x11).await)
            } else {
                None
            }
        }
    );
    let connection = Arc::new(connect_res.map_err(format_ssh_error)?);
    let ssh_ms = t_start.elapsed().as_millis();

    let mut x11_error = String::new();
    let prepared_x11 = match x11_res {
        Some(Ok(prep)) => Some(prep),
        Some(Err(err)) => {
            let msg = err.to_string();
            state
                .launcher_logs
                .push(format!("[terminal:{}] X11 warn: {msg}", profile.name));
            x11_error = msg;
            None
        }
        None => None,
    };

    let t_prep = Instant::now();
    let cols_norm = cols.unwrap_or(120);
    let rows_norm = rows.unwrap_or(32);

    // Overlap reverse-proxy + shell-integration script upload with PTY+X11 channel opening.
    let setup_branch = async {
        let mut proxy_url = None;
        let mut proxy_error = None;
        let mut proxy_env = Vec::new();
        let mut reverse_proxy = None;
        if profile.proxy_active() {
            let lp = profile.local_proxy.normalized();
            match connection.start_reverse_proxy(lp.address()).await {
                Ok(ch) => {
                    proxy_env = proxy_env_pairs(&ch.proxy_url, &lp.no_proxy);
                    proxy_url = Some(ch.proxy_url.clone());
                    reverse_proxy = Some(ch);
                }
                Err(err) => {
                    let msg = err.to_string();
                    state
                        .launcher_logs
                        .push(format!("[terminal:{}] reverse proxy warn: {msg}", profile.name));
                    proxy_error = Some(msg);
                }
            }
        }
        let integration = connection.setup_shell_integration(&proxy_env).await;
        (proxy_url, proxy_error, proxy_env, reverse_proxy, integration)
    };

    let pty_branch = connection.prepare_pty_channel_with_x11_fallback(
        cols_norm,
        rows_norm,
        "xterm-256color",
        prepared_x11.as_ref(),
    );

    let ((proxy_url, proxy_error, proxy_env, mut reverse_proxy, integration), pty_res) =
        tokio::join!(setup_branch, pty_branch);
    let prep_ms = t_prep.elapsed().as_millis();

    let (prepared, prep_x11_warn) = match pty_res {
        Ok(ok) => ok,
        Err(err) => {
            if let Some(ref integ) = integration {
                connection.cleanup_shell_integration(integ).await;
            }
            if let Some(mut rp) = reverse_proxy.take() {
                rp.close().await;
            }
            connection.disconnect().await;
            return Err(err.to_string());
        }
    };

    if let Some(warn) = prep_x11_warn {
        state
            .launcher_logs
            .push(format!("[terminal:{}] X11 warn: {warn}", profile.name));
        if x11_error.is_empty() {
            x11_error = warn;
        }
    }

    let startup_command = if let Some(ref integ) = integration {
        Some(integ.interactive_command(&proxy_env))
    } else if !proxy_env.is_empty() {
        Some(proxy_login_shell_command("/bin/sh", &proxy_env))
    } else {
        None
    };

    let t_exec = Instant::now();
    let (shell, mut event_rx) = match prepared
        .start(startup_command.as_deref(), integration.clone())
        .await
    {
        Ok(ok) => ok,
        Err(err) => {
            if let Some(ref integ) = integration {
                connection.cleanup_shell_integration(integ).await;
            }
            if let Some(mut rp) = reverse_proxy.take() {
                rp.close().await;
            }
            connection.disconnect().await;
            return Err(err.to_string());
        }
    };
    let exec_ms = t_exec.elapsed().as_millis();
    let total_ms = t_start.elapsed().as_millis();
    let x11_enabled = shell.x11_active();

    let session_id = new_id();
    let active = Arc::new(ActiveTerminal {
        connection: Arc::clone(&connection),
        shell,
        integration,
        reverse_proxy: Mutex::new(reverse_proxy),
        events: Mutex::new(TerminalEventBuffer::default()),
    });
    state
        .terminals
        .terminals
        .lock()
        .await
        .insert(session_id.clone(), Arc::clone(&active));

    state.launcher_logs.push(format!(
        "[terminal:{}] connected in {total_ms}ms (ssh={ssh_ms}ms, prep={prep_ms}ms, exec={exec_ms}ms, id={session_id}, x11={x11_enabled})",
        profile.name
    ));
    let active_for_task = Arc::clone(&active);
    let terminals_ref = Arc::clone(&state.terminals);
    let sid_for_task = session_id.clone();

    tokio::spawn(async move {
        while let Some(ev) = event_rx.recv().await {
            let (is_exit, wire) = match ev {
                TerminalEvent::Output(bytes) => {
                    let wire = active_for_task
                        .push_wire_event("output", Some(BASE64.encode(&bytes)), None, None)
                        .await;
                    (false, wire)
                }
                TerminalEvent::Exit { status, reason } => {
                    let wire = active_for_task
                        .push_wire_event("exit", None, status, Some(reason))
                        .await;
                    (true, wire)
                }
            };
            // Never kill the SSH shell if Tauri Channel delivery transiently fails;
            // the event stays in `events` so `poll_terminal_events` can drain it.
            let _ = on_event.send(wire);
            if is_exit {
                break;
            }
        }

        // Release remote shell integration and reverse proxy cleanly, then retain
        // the exit event briefly so late pollers observe EOF before cleanup.
        if let Some(ref integ) = active_for_task.integration {
            active_for_task
                .connection
                .cleanup_shell_integration(integ)
                .await;
        }
        if let Some(mut rp) = active_for_task.reverse_proxy.lock().await.take() {
            rp.close().await;
        }

        tokio::time::sleep(EXITED_SESSION_GRACE).await;
        if let Some(removed) = terminals_ref.terminals.lock().await.remove(&sid_for_task) {
            removed.connection.disconnect().await;
        }
    });

    Ok(ShellOpenResponse {
        id: session_id,
        proxy_url,
        proxy_error,
        x11_enabled,
        x11_error,
    })
}

#[tauri::command]
pub async fn poll_terminal_events(
    state: State<'_, AppState>,
    cursors: HashMap<String, u64>,
) -> Result<HashMap<String, Vec<WireTerminalEvent>>, String> {
    let entries: Vec<(String, u64, Arc<ActiveTerminal>)> = {
        let guard = state.terminals.terminals.lock().await;
        cursors
            .into_iter()
            .filter_map(|(sid, cursor)| guard.get(&sid).cloned().map(|t| (sid, cursor, t)))
            .collect()
    };

    let mut out = HashMap::with_capacity(entries.len());
    for (sid, cursor, active) in entries {
        let evs = active.events_after(cursor).await;
        if !evs.is_empty() {
            out.insert(sid, evs);
        }
    }
    Ok(out)
}

#[tauri::command]
pub async fn get_terminal_cwd(
    state: State<'_, AppState>,
    session_id: String,
) -> Result<TerminalCwdResponse, String> {
    let active = state
        .terminals
        .terminals
        .lock()
        .await
        .get(&session_id)
        .cloned()
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;

    if let Some(ref integ) = active.integration {
        if let Some(live_cwd) = active.connection.query_shell_directory(integ).await {
            return Ok(TerminalCwdResponse { path: live_cwd });
        }
    }

    let home = match active.connection.open_sftp().await {
        Ok(sftp) => sftp.canonicalize(".").await.unwrap_or_else(|_| "/".into()),
        Err(_) => "/".into(),
    };
    Ok(TerminalCwdResponse { path: home })
}

#[tauri::command]
pub async fn send_terminal_input(
    state: State<'_, AppState>,
    session_id: String,
    data_b64: String,
) -> Result<SendInputResult, String> {
    let data = BASE64
        .decode(data_b64.as_bytes())
        .map_err(|e| format!("invalid base64 terminal input: {e}"))?;
    let active = state
        .terminals
        .terminals
        .lock()
        .await
        .get(&session_id)
        .cloned()
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;
    Ok(active.shell.send_input(data))
}

#[tauri::command]
pub async fn resize_terminal(
    state: State<'_, AppState>,
    session_id: String,
    cols: u32,
    rows: u32,
) -> Result<(), String> {
    let active = state
        .terminals
        .terminals
        .lock()
        .await
        .get(&session_id)
        .cloned()
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;
    active.shell.resize(cols, rows).map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn close_terminal(state: State<'_, AppState>, session_id: String) -> Result<(), String> {
    let active = state.terminals.terminals.lock().await.remove(&session_id);
    if let Some(active) = active {
        active.cleanup_resources().await;
    }
    Ok(())
}
