use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex as StdMutex, RwLock};
use std::time::{Duration, Instant, SystemTime};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use core_config::{ProfileStore, new_id, proxy_env_pairs};
use core_ssh::{
    HostKeyPolicy, KnownHostsVerifier, PtyShellSession, ReverseProxyChannel, SendInputResult,
    ShellIntegration, SshConnection, TerminalEvent, prepare_local_x_server,
    proxy_login_shell_command,
};
use serde::{Deserialize, Serialize};
use tauri::State;
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore, mpsc, oneshot};
use tokio::task::JoinSet;

use crate::commands::sessions::{format_ssh_error, load_ssh_credentials};
use crate::state::{AppState, LauncherLogStore};

const MAX_TERMINAL_EVENTS: usize = 4096;
const MAX_TERMINAL_BUFFER_BYTES: usize = 2 * 1024 * 1024;
const EXITED_SESSION_GRACE: Duration = Duration::from_secs(15);
const OUTPUT_COALESCE_CAP: usize = 32 * 1024;
const WS_QUEUE_CAPACITY: usize = 256;
const WS_QUEUE_BYTES: usize = 2 * 1024 * 1024;
const IPC_REPLAY_EVENTS: usize = 256;
const IPC_REPLAY_BYTES: usize = 512 * 1024;
const WAKE_PROBE_TIMEOUT: Duration = Duration::from_secs(4);
const WAKE_CHECK_INTERVAL: Duration = Duration::from_secs(2);
const WAKE_JUMP_THRESHOLD: Duration = Duration::from_secs(10);

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
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ws_url: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct TerminalCwdResponse {
    pub path: String,
}

#[derive(Debug, Clone)]
pub(crate) enum RawTerminalEvent {
    Output {
        seq: u64,
        data: Arc<[u8]>,
    },
    Exit {
        seq: u64,
        status: Option<u32>,
        reason: String,
    },
}

impl RawTerminalEvent {
    pub(crate) fn seq(&self) -> u64 {
        match self {
            Self::Output { seq, .. } | Self::Exit { seq, .. } => *seq,
        }
    }

    fn payload_len(&self) -> usize {
        match self {
            Self::Output { data, .. } => data.len(),
            Self::Exit { reason, .. } => reason.len(),
        }
    }

    fn to_wire(&self) -> WireTerminalEvent {
        match self {
            Self::Output { seq, data } => WireTerminalEvent {
                kind: "output",
                seq: *seq,
                data_b64: Some(BASE64.encode(data)),
                exit_code: None,
                reason: None,
            },
            Self::Exit {
                seq,
                status,
                reason,
            } => WireTerminalEvent {
                kind: "exit",
                seq: *seq,
                data_b64: None,
                exit_code: *status,
                reason: Some(reason.clone()),
            },
        }
    }
}

#[derive(Debug, Serialize)]
pub struct WireTerminalReplay {
    lost_through: u64,
    latest_seq: u64,
    events: Vec<WireTerminalEvent>,
}

pub(crate) struct WsQueuedEvent {
    pub(crate) event: RawTerminalEvent,
    // Count queued payload even while it is being written to the transport.
    _permit: OwnedSemaphorePermit,
}

struct WsSubscriber {
    attempt: u64,
    tx: mpsc::Sender<WsQueuedEvent>,
    bytes: Arc<Semaphore>,
    // Dropping this aborts an overflowing or superseded socket, including replay.
    _live: oneshot::Sender<()>,
}

pub(crate) struct WsTerminalReplay {
    pub(crate) lost_through: u64,
    pub(crate) latest_seq: u64,
    pub(crate) events: Vec<RawTerminalEvent>,
    pub(crate) rx: mpsc::Receiver<WsQueuedEvent>,
    pub(crate) dropped: oneshot::Receiver<()>,
    pub(crate) _guard: WsDetachGuard,
}

struct RawTerminalHub {
    events: VecDeque<RawTerminalEvent>,
    next_seq: u64,
    total_bytes: usize,
    latest_ws_attempt: u64,
    exited: bool,
    subscriber: Option<WsSubscriber>,
}

impl Default for RawTerminalHub {
    fn default() -> Self {
        Self {
            events: VecDeque::new(),
            next_seq: 1,
            total_bytes: 0,
            latest_ws_attempt: 0,
            exited: false,
            subscriber: None,
        }
    }
}

impl RawTerminalHub {
    fn push_event(&mut self, ev: RawTerminalEvent) -> RawTerminalEvent {
        self.next_seq = self.next_seq.saturating_add(1);
        self.total_bytes = self.total_bytes.saturating_add(ev.payload_len());
        self.events.push_back(ev.clone());

        while self.events.len() > MAX_TERMINAL_EVENTS
            || self.total_bytes > MAX_TERMINAL_BUFFER_BYTES
        {
            if self.events.len() <= 1 {
                break;
            }
            if let Some(evicted) = self.events.pop_front() {
                self.total_bytes = self.total_bytes.saturating_sub(evicted.payload_len());
            }
        }

        if let Some(subscriber) = &self.subscriber {
            let queued = Arc::clone(&subscriber.bytes)
                .try_acquire_many_owned(ev.payload_len() as u32)
                .is_ok_and(|permit| {
                    subscriber
                        .tx
                        .try_send(WsQueuedEvent {
                            event: ev.clone(),
                            _permit: permit,
                        })
                        .is_ok()
                });
            if !queued {
                self.subscriber = None;
            }
        }
        ev
    }

    fn push_output(&mut self, data: Vec<u8>) {
        if self.exited {
            return;
        }
        for chunk in data.chunks(OUTPUT_COALESCE_CAP) {
            self.push_event(RawTerminalEvent::Output {
                seq: self.next_seq,
                data: Arc::from(chunk),
            });
        }
    }

    fn push_exit(&mut self, status: Option<u32>, reason: String) -> Option<RawTerminalEvent> {
        if self.exited {
            return None;
        }
        self.exited = true;
        let seq = self.next_seq;
        Some(self.push_event(RawTerminalEvent::Exit {
            seq,
            status,
            reason,
        }))
    }

    fn events_after(&self, cursor: u64) -> Vec<RawTerminalEvent> {
        self.events
            .iter()
            .filter(|ev| ev.seq() > cursor)
            .cloned()
            .collect()
    }

    fn replay_bounds(&self) -> (u64, u64) {
        let latest = self.next_seq - 1;
        let lost_through = self.events.front().map_or(latest, |ev| ev.seq() - 1);
        (lost_through, latest)
    }

    fn replay_page(&self, cursor: u64, metadata_only: bool) -> (u64, u64, Vec<RawTerminalEvent>) {
        let (lost_through, latest) = self.replay_bounds();
        let mut events = Vec::new();
        if !metadata_only {
            let mut bytes = 0;
            for ev in self.events.iter().filter(|ev| ev.seq() > cursor) {
                bytes += ev.payload_len();
                if events.len() == IPC_REPLAY_EVENTS || bytes > IPC_REPLAY_BYTES {
                    break;
                }
                events.push(ev.clone());
            }
        }
        (lost_through, latest, events)
    }

    fn attach(
        &mut self,
        attempt: u64,
    ) -> Option<(mpsc::Receiver<WsQueuedEvent>, oneshot::Receiver<()>)> {
        if attempt <= self.latest_ws_attempt {
            return None;
        }
        self.latest_ws_attempt = attempt;
        let (tx, rx) = mpsc::channel(WS_QUEUE_CAPACITY);
        let (live, dropped) = oneshot::channel();
        self.subscriber = Some(WsSubscriber {
            attempt,
            tx,
            bytes: Arc::new(Semaphore::new(WS_QUEUE_BYTES)),
            _live: live,
        });
        Some((rx, dropped))
    }

    fn detach(&mut self, attempt: u64) {
        if self
            .subscriber
            .as_ref()
            .is_some_and(|sub| sub.attempt == attempt)
        {
            self.subscriber = None;
        }
    }
}

enum TerminalEndpoint {
    Pty {
        connection: Arc<SshConnection>,
        shell: PtyShellSession,
        integration: Option<ShellIntegration>,
        reverse_proxy: Mutex<Option<ReverseProxyChannel>>,
    },
    #[allow(dead_code)]
    Mock {
        input_tx: mpsc::UnboundedSender<Vec<u8>>,
        resize_tx: mpsc::UnboundedSender<(u32, u32)>,
    },
}

pub(crate) struct ActiveTerminal {
    endpoint: TerminalEndpoint,
    hub: StdMutex<RawTerminalHub>,
}

pub(crate) struct WsDetachGuard {
    active: Arc<ActiveTerminal>,
    sub_id: u64,
}

impl Drop for WsDetachGuard {
    fn drop(&mut self) {
        self.active.detach_ws(self.sub_id);
    }
}

impl ActiveTerminal {
    fn push_output(&self, data: Vec<u8>) {
        let mut guard = self.hub.lock().expect("terminal hub lock poisoned");
        guard.push_output(data);
    }

    pub(crate) fn push_exit(
        &self,
        status: Option<u32>,
        reason: String,
    ) -> Option<RawTerminalEvent> {
        let mut guard = self.hub.lock().expect("terminal hub lock poisoned");
        guard.push_exit(status, reason)
    }

    pub(crate) fn has_exited(&self) -> bool {
        self.hub.lock().expect("terminal hub lock poisoned").exited
    }

    async fn probe_alive(&self, timeout: Duration) -> bool {
        if self.has_exited() {
            return false;
        }
        match &self.endpoint {
            TerminalEndpoint::Pty { connection, .. } => connection.probe_alive(timeout).await,
            TerminalEndpoint::Mock { input_tx, .. } => !input_tx.is_closed(),
        }
    }

    async fn mark_dead_and_disconnect(&self, reason: String) {
        let _ = self.push_exit(None, reason);
        if let TerminalEndpoint::Pty {
            shell, connection, ..
        } = &self.endpoint
        {
            shell.close();
            connection.disconnect().await;
        }
    }

    #[cfg(test)]
    pub(crate) fn push_raw_output(&self, data: Vec<u8>) {
        self.push_output(data);
    }

    #[cfg(test)]
    pub(crate) fn push_raw_exit(&self, status: Option<u32>, reason: String) {
        self.push_exit(status, reason);
    }

    fn wire_replay_after(&self, cursor: u64, metadata_only: bool) -> WireTerminalReplay {
        let (lost_through, latest_seq, events) = self
            .hub
            .lock()
            .expect("terminal hub lock poisoned")
            .replay_page(cursor, metadata_only);
        WireTerminalReplay {
            lost_through,
            latest_seq,
            events: events.into_iter().map(|ev| ev.to_wire()).collect(),
        }
    }

    pub(crate) fn attach_ws(
        self: &Arc<Self>,
        since: u64,
        attempt: u64,
    ) -> Option<WsTerminalReplay> {
        let mut guard = self.hub.lock().expect("terminal hub lock poisoned");
        let (rx, dropped) = guard.attach(attempt)?;
        let (lost_through, latest_seq) = guard.replay_bounds();
        Some(WsTerminalReplay {
            lost_through,
            latest_seq,
            events: guard.events_after(since),
            rx,
            dropped,
            _guard: WsDetachGuard {
                active: Arc::clone(self),
                sub_id: attempt,
            },
        })
    }

    fn detach_ws(&self, sub_id: u64) {
        let mut guard = self.hub.lock().expect("terminal hub lock poisoned");
        guard.detach(sub_id);
    }

    pub(crate) fn send_input(&self, data: Vec<u8>) -> SendInputResult {
        match &self.endpoint {
            TerminalEndpoint::Pty { shell, .. } => shell.send_input(data),
            TerminalEndpoint::Mock { input_tx, .. } => {
                if input_tx.send(data).is_ok() {
                    SendInputResult::Sent
                } else {
                    SendInputResult::Closed
                }
            }
        }
    }

    pub(crate) fn resize(&self, cols: u32, rows: u32) -> Result<(), String> {
        match &self.endpoint {
            TerminalEndpoint::Pty { shell, .. } => {
                shell.resize(cols, rows).map_err(|e| e.to_string())
            }
            TerminalEndpoint::Mock { resize_tx, .. } => resize_tx
                .send((cols, rows))
                .map_err(|_| "mock terminal closed".to_string()),
        }
    }

    async fn cleanup_pty_aux(&self) {
        if let TerminalEndpoint::Pty {
            connection,
            integration,
            reverse_proxy,
            ..
        } = &self.endpoint
        {
            if let Some(ref integ) = integration {
                connection.cleanup_shell_integration(integ).await;
            }
            if let Some(mut rp) = reverse_proxy.lock().await.take() {
                rp.close().await;
            }
        }
    }

    async fn disconnect_connection(&self) {
        if let TerminalEndpoint::Pty { connection, .. } = &self.endpoint {
            connection.disconnect().await;
        }
    }

    async fn cleanup_resources(&self) {
        if let TerminalEndpoint::Pty { shell, .. } = &self.endpoint {
            shell.close();
        }
        self.cleanup_pty_aux().await;
        self.disconnect_connection().await;
    }
}

#[derive(Default)]
pub struct TerminalManager {
    terminals: RwLock<HashMap<String, Arc<ActiveTerminal>>>,
}

impl TerminalManager {
    pub(crate) fn get(&self, id: &str) -> Option<Arc<ActiveTerminal>> {
        self.terminals.read().ok().and_then(|g| g.get(id).cloned())
    }

    fn insert(&self, id: String, active: Arc<ActiveTerminal>) {
        if let Ok(mut g) = self.terminals.write() {
            g.insert(id, active);
        }
    }

    #[cfg(test)]
    pub(crate) fn insert_mock(
        &self,
        id: String,
    ) -> (
        Arc<ActiveTerminal>,
        mpsc::UnboundedReceiver<Vec<u8>>,
        mpsc::UnboundedReceiver<(u32, u32)>,
    ) {
        let (input_tx, input_rx) = mpsc::unbounded_channel();
        let (resize_tx, resize_rx) = mpsc::unbounded_channel();
        let active = Arc::new(ActiveTerminal {
            endpoint: TerminalEndpoint::Mock {
                input_tx,
                resize_tx,
            },
            hub: StdMutex::new(RawTerminalHub::default()),
        });
        self.insert(id, Arc::clone(&active));
        (active, input_rx, resize_rx)
    }

    fn remove(&self, id: &str) -> Option<Arc<ActiveTerminal>> {
        self.terminals.write().ok().and_then(|mut g| g.remove(id))
    }

    /// Probes active terminal SSH connections concurrently and immediately marks
    /// dead/half-open sessions (such as after OS sleep/wake) as exited.
    pub async fn probe_and_prune_dead(
        &self,
        session_ids: Option<Vec<String>>,
        timeout: Duration,
    ) -> Vec<String> {
        let mut dead_ids = Vec::new();
        let targets: Vec<(String, Arc<ActiveTerminal>)> = {
            let Ok(guard) = self.terminals.read() else {
                return dead_ids;
            };
            match session_ids {
                Some(ids) => {
                    let mut found = Vec::with_capacity(ids.len());
                    for id in ids {
                        if let Some(active) = guard.get(&id) {
                            found.push((id, Arc::clone(active)));
                        } else {
                            dead_ids.push(id);
                        }
                    }
                    found
                }
                None => guard
                    .iter()
                    .map(|(id, active)| (id.clone(), Arc::clone(active)))
                    .collect(),
            }
        };

        let mut join_set = JoinSet::new();
        for (id, active) in targets {
            join_set.spawn(async move {
                let alive = active.probe_alive(timeout).await;
                if !alive {
                    active
                        .mark_dead_and_disconnect(
                            "SSH connection lost (keepalive timeout)".to_string(),
                        )
                        .await;
                    Some(id)
                } else {
                    None
                }
            });
        }

        while let Some(res) = join_set.join_next().await {
            if let Ok(Some(dead_id)) = res {
                dead_ids.push(dead_id);
            }
        }
        dead_ids
    }

    /// Watches wall-clock jumps to detect OS sleep/wake cycles and proactively
    /// probe active SSH terminals so half-open sockets fail fast instead of
    /// hanging for a full keepalive cycle.
    pub fn spawn_sleep_wake_monitor(self: Arc<Self>, launcher_logs: Arc<LauncherLogStore>) {
        // Must use `tauri::async_runtime::spawn` because `.setup()` runs on the
        // main thread outside a Tokio runtime context (where `tokio::spawn` panics).
        tauri::async_runtime::spawn(async move {
            let mut last_wall = SystemTime::now();
            loop {
                tokio::time::sleep(WAKE_CHECK_INTERVAL).await;
                let now_wall = SystemTime::now();
                let jumped = now_wall.duration_since(last_wall).unwrap_or(Duration::ZERO);
                last_wall = now_wall;

                if jumped >= WAKE_JUMP_THRESHOLD {
                    let has_any = self
                        .terminals
                        .read()
                        .map(|g| !g.is_empty())
                        .unwrap_or(false);
                    if !has_any {
                        continue;
                    }
                    launcher_logs.push(format!(
                        "[terminal] system wake detected (wall-clock jump {}s); probing active SSH terminals",
                        jumped.as_secs()
                    ));
                    let dead = self.probe_and_prune_dead(None, WAKE_PROBE_TIMEOUT).await;
                    if !dead.is_empty() {
                        launcher_logs.push(format!(
                            "[terminal] pruned {} dead SSH terminal(s) after wake",
                            dead.len()
                        ));
                    }
                }
            }
        });
    }

    pub async fn close_all(&self) {
        let active_list: Vec<Arc<ActiveTerminal>> = match self.terminals.write() {
            Ok(mut guard) => guard.drain().map(|(_, v)| v).collect(),
            Err(_) => Vec::new(),
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
) -> Result<ShellOpenResponse, String> {
    let profile = state.profiles.get(&profile_id).map_err(|e| e.to_string())?;
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
    let (connect_res, x11_res) =
        tokio::join!(SshConnection::connect(&profile, &creds, verifier), async {
            if profile.x11_active() {
                Some(prepare_local_x_server(&profile.x11).await)
            } else {
                None
            }
        });
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
                    state.launcher_logs.push(format!(
                        "[terminal:{}] reverse proxy warn: {msg}",
                        profile.name
                    ));
                    proxy_error = Some(msg);
                }
            }
        }
        let integration = connection.setup_shell_integration(&proxy_env).await;
        (
            proxy_url,
            proxy_error,
            proxy_env,
            reverse_proxy,
            integration,
        )
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
    let ws_url = if state.proxy_base_url.is_empty() {
        None
    } else {
        let base = state.proxy_base_url.trim_end_matches('/');
        let ws_base = base
            .strip_prefix("http://")
            .map(|rest| format!("ws://{rest}"))
            .unwrap_or_else(|| base.replacen("https://", "wss://", 1));
        Some(format!(
            "{ws_base}/api/terminal-ws?session={session_id}&token={}",
            state.api_token
        ))
    };

    let active = Arc::new(ActiveTerminal {
        endpoint: TerminalEndpoint::Pty {
            connection: Arc::clone(&connection),
            shell,
            integration,
            reverse_proxy: Mutex::new(reverse_proxy),
        },
        hub: StdMutex::new(RawTerminalHub::default()),
    });
    state
        .terminals
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
            match ev {
                TerminalEvent::Output(mut bytes) => {
                    let mut deferred_exit = None;
                    while bytes.len() < OUTPUT_COALESCE_CAP {
                        match event_rx.try_recv() {
                            Ok(TerminalEvent::Output(more)) => bytes.extend_from_slice(&more),
                            Ok(TerminalEvent::Exit { status, reason }) => {
                                deferred_exit = Some((status, reason));
                                break;
                            }
                            Err(_) => break,
                        }
                    }
                    active_for_task.push_output(bytes);
                    if let Some((status, reason)) = deferred_exit {
                        active_for_task.push_exit(status, reason);
                        break;
                    }
                }
                TerminalEvent::Exit { status, reason } => {
                    active_for_task.push_exit(status, reason);
                    break;
                }
            }
        }
        active_for_task.push_exit(None, "terminal session closed".into());

        // Release remote shell integration and reverse proxy cleanly, then retain
        // the exit event briefly so late pollers observe EOF before cleanup.
        active_for_task.cleanup_pty_aux().await;

        tokio::time::sleep(EXITED_SESSION_GRACE).await;
        if let Some(removed) = terminals_ref.remove(&sid_for_task) {
            removed.disconnect_connection().await;
        }
    });

    Ok(ShellOpenResponse {
        id: session_id,
        proxy_url,
        proxy_error,
        x11_enabled,
        x11_error,
        ws_url,
    })
}

#[derive(Deserialize)]
pub struct TerminalReplayCursor {
    since: u64,
    #[serde(default)]
    metadata_only: bool,
}

#[tauri::command]
pub async fn poll_terminal_events(
    state: State<'_, AppState>,
    cursors: HashMap<String, TerminalReplayCursor>,
) -> Result<HashMap<String, Option<WireTerminalReplay>>, String> {
    Ok(cursors
        .into_iter()
        .map(|(sid, cursor)| {
            let events = state
                .terminals
                .get(&sid)
                .map(|active| active.wire_replay_after(cursor.since, cursor.metadata_only));
            (sid, events)
        })
        .collect())
}

#[tauri::command]
pub async fn get_terminal_cwd(
    state: State<'_, AppState>,
    session_id: String,
) -> Result<TerminalCwdResponse, String> {
    let active = state
        .terminals
        .get(&session_id)
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;

    let TerminalEndpoint::Pty {
        connection,
        integration,
        ..
    } = &active.endpoint
    else {
        return Ok(TerminalCwdResponse { path: "/".into() });
    };

    if let Some(ref integ) = integration {
        if let Some(live_cwd) = connection.query_shell_directory(integ).await {
            return Ok(TerminalCwdResponse { path: live_cwd });
        }
    }

    let home = match connection.open_sftp().await {
        Ok(sftp) => sftp.canonicalize(".").await.unwrap_or_else(|_| "/".into()),
        Err(_) => "/".into(),
    };
    Ok(TerminalCwdResponse { path: home })
}

#[tauri::command]
pub fn send_terminal_input(
    state: State<'_, AppState>,
    session_id: String,
    data: Option<String>,
    data_b64: Option<String>,
) -> Result<SendInputResult, String> {
    let bytes = if let Some(text) = data {
        text.into_bytes()
    } else if let Some(b64) = data_b64 {
        BASE64
            .decode(b64.as_bytes())
            .map_err(|e| format!("invalid base64 terminal input: {e}"))?
    } else {
        Vec::new()
    };
    let active = state
        .terminals
        .get(&session_id)
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;
    // The output task records EOF after draining preceding terminal output.
    Ok(active.send_input(bytes))
}

#[tauri::command]
pub fn resize_terminal(
    state: State<'_, AppState>,
    session_id: String,
    cols: u32,
    rows: u32,
) -> Result<(), String> {
    let active = state
        .terminals
        .get(&session_id)
        .ok_or_else(|| format!("terminal session not found: {session_id}"))?;
    active.resize(cols, rows)
}

#[tauri::command]
pub async fn close_terminal(state: State<'_, AppState>, session_id: String) -> Result<(), String> {
    if let Some(active) = state.terminals.remove(&session_id) {
        active.push_exit(None, "terminal session closed".into());
        active.cleanup_resources().await;
    }
    Ok(())
}

#[tauri::command]
pub async fn probe_terminal_sessions(
    state: State<'_, AppState>,
    session_ids: Option<Vec<String>>,
) -> Result<Vec<String>, String> {
    Ok(state
        .terminals
        .probe_and_prune_dead(session_ids, WAKE_PROBE_TIMEOUT)
        .await)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn websocket_overflow_keeps_the_ring_replayable_and_fences_older_attempts() {
        let mgr = TerminalManager::default();
        let (active, _input, _resize) = mgr.insert_mock("term".into());
        let mut first = active.attach_ws(0, 1).unwrap();
        for _ in 0..=WS_QUEUE_CAPACITY {
            active.push_raw_output(vec![b'a']);
        }
        assert!(matches!(
            first.dropped.try_recv(),
            Err(oneshot::error::TryRecvError::Closed)
        ));

        let second = active.attach_ws(42, 3).unwrap();
        assert_eq!(second.lost_through, 0);
        assert_eq!(second.events.first().unwrap().seq(), 43);
        assert_eq!(second.latest_seq, WS_QUEUE_CAPACITY as u64 + 1);
        assert!(active.attach_ws(42, 2).is_none());
        drop(first);
        active.push_raw_exit(Some(0), "done".into());
        let mut second = second;
        assert!(matches!(
            second.rx.try_recv().unwrap().event,
            RawTerminalEvent::Exit { .. }
        ));
    }

    #[test]
    fn terminal_replay_has_explicit_loss_bounds_and_bounded_pages() {
        let mut hub = RawTerminalHub::default();
        let (_rx, mut dropped) = hub.attach(1).unwrap();
        hub.push_output(vec![b'a'; WS_QUEUE_BYTES + 1]);
        assert!(matches!(
            dropped.try_recv(),
            Err(oneshot::error::TryRecvError::Closed)
        ));
        let (lost_through, latest, page) = hub.replay_page(0, false);
        assert_eq!(lost_through, 1);
        assert_eq!(latest, 65);
        assert_eq!(page.first().unwrap().seq(), 2);
        assert_eq!(page.len(), IPC_REPLAY_BYTES / OUTPUT_COALESCE_CAP);
        assert!(
            page.iter()
                .all(|ev| ev.payload_len() <= OUTPUT_COALESCE_CAP)
        );
        assert_eq!(hub.replay_page(latest, true).0, lost_through);
        assert!(hub.replay_page(0, true).2.is_empty());
    }

    #[tokio::test]
    async fn probe_and_prune_dead_marks_closed_and_missing_sessions_and_keeps_live_ones() {
        let mgr = TerminalManager::default();
        let (live, _live_in_rx, _live_rz_rx) = mgr.insert_mock("live-1".to_string());
        let (dead, dead_in_rx, _dead_rz_rx) = mgr.insert_mock("dead-1".to_string());
        drop(dead_in_rx);

        let mut pruned = mgr
            .probe_and_prune_dead(
                Some(vec![
                    "live-1".to_string(),
                    "dead-1".to_string(),
                    "missing-1".to_string(),
                ]),
                Duration::from_millis(200),
            )
            .await;
        pruned.sort();
        assert_eq!(pruned, vec!["dead-1".to_string(), "missing-1".to_string()]);
        assert!(!live.has_exited());
        assert!(dead.has_exited());

        // Second exit push is idempotent and does not duplicate Exit events.
        assert!(dead.push_exit(None, "duplicate".into()).is_none());
        dead.push_raw_output(b"too late".to_vec());
        let replay = dead.wire_replay_after(0, false);
        assert_eq!(replay.events.len(), 1);
        assert_eq!(replay.events[0].kind, "exit");
    }

    #[test]
    fn spawn_sleep_wake_monitor_works_outside_tokio_runtime() {
        let mgr = Arc::new(TerminalManager::default());
        let logs = Arc::new(LauncherLogStore::default());
        // Regression test: `.setup()` invokes this synchronously on the main thread.
        mgr.spawn_sleep_wake_monitor(logs);
    }
}
