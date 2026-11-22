use std::collections::HashMap;
use std::sync::Arc;

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use core_config::{new_id, ProfileStore};
use core_ssh::{
    HostKeyPolicy, KnownHostsVerifier, PtyShellSession, ReverseProxyChannel, SendInputResult,
    SshConnection, TerminalEvent,
};
use serde::Serialize;
use tauri::ipc::Channel;
use tauri::State;
use tokio::sync::{mpsc, Mutex};

use crate::commands::sessions::{format_ssh_error, load_ssh_credentials};
use crate::state::AppState;

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

struct ActiveTerminal {
    connection: Arc<SshConnection>,
    shell: PtyShellSession,
    reverse_proxy: Mutex<Option<ReverseProxyChannel>>,
}

#[derive(Default)]
pub struct TerminalManager {
    terminals: Mutex<HashMap<String, Arc<ActiveTerminal>>>,
}

#[tauri::command]
pub async fn open_terminal(
    state: State<'_, AppState>,
    profile_id: String,
    cols: Option<u32>,
    rows: Option<u32>,
    host_key_policy: Option<HostKeyPolicy>,
    on_event: Channel<TerminalEvent>,
) -> Result<ShellOpenResponse, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    if profile.is_local() {
        return Err("local profiles do not support SSH terminals".into());
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
            .map_err(format_ssh_error)?,
    );

    let mut proxy_url = None;
    let mut proxy_error = None;
    let mut reverse_proxy = None;
    if profile.proxy_active() {
        let lp = profile.local_proxy.normalized();
        match connection.start_reverse_proxy(lp.address()).await {
            Ok(ch) => {
                proxy_url = Some(ch.proxy_url.clone());
                reverse_proxy = Some(ch);
            }
            Err(err) => {
                proxy_error = Some(err.to_string());
            }
        }
    }

    let (event_tx, mut event_rx) = mpsc::channel::<TerminalEvent>(256);
    let shell = connection
        .open_pty_shell(
            cols.unwrap_or(120),
            rows.unwrap_or(32),
            "xterm-256color",
            event_tx,
        )
        .await
        .map_err(|e| e.to_string())?;

    let session_id = new_id();
    let active = Arc::new(ActiveTerminal {
        connection,
        shell,
        reverse_proxy: Mutex::new(reverse_proxy),
    });
    state
        .terminals
        .terminals
        .lock()
        .await
        .insert(session_id.clone(), active);

    let terminals_ref = Arc::clone(&state.terminals);
    let sid_for_task = session_id.clone();
    tokio::spawn(async move {
        while let Some(ev) = event_rx.recv().await {
            let is_exit = matches!(ev, TerminalEvent::Exit { .. });
            if on_event.send(ev).is_err() || is_exit {
                break;
            }
        }
        if let Some(removed) = terminals_ref.terminals.lock().await.remove(&sid_for_task) {
            if let Some(mut rp) = removed.reverse_proxy.lock().await.take() {
                rp.close().await;
            }
        }
    });

    Ok(ShellOpenResponse {
        id: session_id,
        proxy_url,
        proxy_error,
        x11_enabled: profile.x11_active(),
        x11_error: String::new(),
    })
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
    active.shell.send_input(data).map_err(|e| e.to_string())
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
    active
        .shell
        .resize(cols, rows)
        .await
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn close_terminal(state: State<'_, AppState>, session_id: String) -> Result<(), String> {
    let active = state.terminals.terminals.lock().await.remove(&session_id);
    if let Some(active) = active {
        if let Some(mut rp) = active.reverse_proxy.lock().await.take() {
            rp.close().await;
        }
        active.shell.close().await;
        active.connection.disconnect().await;
    }
    Ok(())
}
