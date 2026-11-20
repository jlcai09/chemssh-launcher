use std::path::PathBuf;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tauri::ipc::Channel;
use tauri::{State, WebviewWindow};
use win_native::{is_windows_native_supported, start_shell_file_drag, DragDropOutcome, DragJob};

use crate::state::AppState;

#[derive(Debug, Clone, Deserialize)]
pub struct DragOutRequest {
    pub id: String,
    pub session: String,
    pub paths: Vec<String>,
    pub staged_local_paths: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ActiveWorkSummary {
    pub active_transfers: usize,
    pub active_drag_jobs: usize,
}

#[tauri::command]
pub fn is_native_drag_supported() -> bool {
    is_windows_native_supported()
}

#[tauri::command]
pub fn list_native_drag_jobs(state: State<'_, AppState>) -> Vec<DragJob> {
    state.drag_jobs.list()
}

#[tauri::command]
pub fn control_native_drag_job(
    state: State<'_, AppState>,
    id: String,
    action: String,
) -> Result<DragJob, String> {
    match action.as_str() {
        "pause" => state.drag_jobs.pause(&id).map_err(|e| e.to_string()),
        "resume" => state.drag_jobs.resume(&id).map_err(|e| e.to_string()),
        "cancel" => state.drag_jobs.cancel(&id).map_err(|e| e.to_string()),
        other => Err(format!("unsupported drag job action: {other}")),
    }
}

#[tauri::command]
pub fn has_active_work(state: State<'_, AppState>) -> ActiveWorkSummary {
    ActiveWorkSummary {
        active_transfers: state.transfers.active_count(),
        active_drag_jobs: state.drag_jobs.active_count(),
    }
}

#[tauri::command]
pub async fn start_native_drag_out(
    window: WebviewWindow,
    state: State<'_, AppState>,
    request: DragOutRequest,
    on_event: Channel<DragJob>,
) -> Result<DragDropOutcome, String> {
    let initial = state
        .drag_jobs
        .add(request.id.clone(), request.session, request.paths)
        .map_err(|e| e.to_string())?;
    let _ = on_event.send(initial);

    if let Some(updated) = state.drag_jobs.update_state(&request.id, "caching", None) {
        let _ = on_event.send(updated);
    }

    let hwnd_raw: isize = {
        #[cfg(target_os = "windows")]
        {
            window.hwnd().map(|h| h.0 as isize).unwrap_or(0)
        }
        #[cfg(not(target_os = "windows"))]
        {
            let _ = &window;
            0
        }
    };

    let staged: Vec<PathBuf> = request
        .staged_local_paths
        .into_iter()
        .map(PathBuf::from)
        .collect();
    let registry = Arc::clone(&state.drag_jobs);
    let job_id = request.id.clone();

    let outcome = tokio::task::spawn_blocking(move || start_shell_file_drag(hwnd_raw, &staged))
        .await
        .map_err(|e| format!("drag task join error: {e}"))?;

    match outcome {
        Ok(DragDropOutcome::Dropped) => {
            if let Some(done) = registry.update_state(&job_id, "done", None) {
                let _ = on_event.send(done);
            }
            Ok(DragDropOutcome::Dropped)
        }
        Ok(DragDropOutcome::Cancelled) => {
            if let Some(cancelled) =
                registry.update_state(&job_id, "cancelled", Some("drag cancelled"))
            {
                let _ = on_event.send(cancelled);
            }
            Ok(DragDropOutcome::Cancelled)
        }
        Err(err) => {
            let msg = err.to_string();
            if let Some(failed) = registry.update_state(&job_id, "error", Some(&msg)) {
                let _ = on_event.send(failed);
            }
            Err(msg)
        }
    }
}
