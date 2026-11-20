use std::collections::HashMap;
use std::sync::Arc;

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use core_config::{new_id, ProfileStore};
use core_ssh::{
    HostKeyPolicy, KnownHostsVerifier, PtyShellSession, SendInputResult, SshConnection,
    TerminalEvent,
};
use tauri::ipc::Channel;
use tauri::State;
use tokio::sync::{mpsc, Mutex};

use crate::commands::sessions::load_ssh_credentials;
use crate::state::AppState;

struct ActiveTerminal {
    connection: Arc<SshConnection>,
    shell: PtyShellSession,
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
) -> Result<String, String> {
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
            .map_err(|e| e.to_string())?,
    );

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
    let active = Arc::new(ActiveTerminal { connection, shell });
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
        terminals_ref.terminals.lock().await.remove(&sid_for_task);
    });

    Ok(session_id)
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
        active.shell.close().await;
        active.connection.disconnect().await;
    }
    Ok(())
}
