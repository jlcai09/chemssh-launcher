use std::sync::Arc;
use std::time::Duration;

use core_transfer::TransferProgress;
use tauri::ipc::Channel;
use tauri::State;

use crate::state::AppState;

const PROGRESS_THROTTLE_MS: u64 = 80;

#[tauri::command]
pub fn get_transfer_progress(
    state: State<'_, AppState>,
    id: String,
) -> Result<Option<TransferProgress>, String> {
    Ok(state.transfers.get(&id))
}

#[tauri::command]
pub fn list_transfers(state: State<'_, AppState>) -> Result<Vec<TransferProgress>, String> {
    Ok(state.transfers.list_all())
}

#[tauri::command]
pub fn get_transfer_count(state: State<'_, AppState>) -> usize {
    state.transfers.active_count()
}

#[tauri::command]
pub fn control_transfer(
    state: State<'_, AppState>,
    id: String,
    action: String,
) -> Result<TransferProgress, String> {
    match action.as_str() {
        "pause" => state.transfers.pause(&id).map_err(|e| e.to_string()),
        "resume" => state.transfers.resume(&id).map_err(|e| e.to_string()),
        "cancel" => state.transfers.cancel(&id, None).map_err(|e| e.to_string()),
        other => Err(format!("unsupported transfer action: {other}")),
    }
}

/// Streams throttled transfer progress snapshots every 80ms over a Tauri v2 Channel.
#[tauri::command]
pub async fn stream_transfer_progress(
    state: State<'_, AppState>,
    on_event: Channel<Vec<TransferProgress>>,
) -> Result<(), String> {
    let store = Arc::clone(&state.transfers);
    let initial = store.list_all();
    let _ = on_event.send(initial.clone());

    tokio::spawn(async move {
        let mut last = initial;
        let mut tick = tokio::time::interval(Duration::from_millis(PROGRESS_THROTTLE_MS));
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tick.tick().await;
            let current = store.list_all();
            if current != last {
                if on_event.send(current.clone()).is_err() {
                    break;
                }
                last = current;
            }
        }
    });
    Ok(())
}
