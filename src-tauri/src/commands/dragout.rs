use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use russh_sftp::client::SftpSession;
use serde::{Deserialize, Serialize};
use tauri::ipc::Channel;
use tauri::{State, WebviewWindow};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use win_native::{
    is_windows_native_supported, start_shell_file_drag, DragDropOutcome, DragJob, DragJobRegistry,
    FileProgress,
};

use crate::state::AppState;

const DRAG_STAGE_PREFIX: &str = "chemssh-drag-";
const DRAG_STAGE_MAX_AGE: Duration = Duration::from_secs(6 * 60 * 60);
const STAGE_CHUNK_SIZE: usize = 256 * 1024;

#[derive(Debug, Clone, Deserialize)]
pub struct DragOutRequest {
    pub id: String,
    pub session: String,
    pub paths: Vec<String>,
    #[serde(default)]
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

fn sanitize_stage_name(name: &str) -> String {
    let cleaned: String = name
        .chars()
        .map(|c| {
            if matches!(c, '/' | '\\' | ':' | '*' | '?' | '"' | '<' | '>' | '|') || c.is_control() {
                '_'
            } else {
                c
            }
        })
        .collect();
    let trimmed = cleaned.trim().trim_end_matches('.');
    if trimmed.is_empty() || trimmed == "." || trimmed == ".." {
        "item".into()
    } else {
        trimmed.to_string()
    }
}

fn unique_stage_child(dir: &Path, raw_name: &str, used: &mut HashSet<String>) -> PathBuf {
    let base = sanitize_stage_name(raw_name);
    let mut candidate = base.clone();
    let mut idx = 1usize;
    while !used.insert(candidate.to_ascii_lowercase()) {
        candidate = format!("{base} ({idx})");
        idx += 1;
    }
    dir.join(candidate)
}

fn prune_old_drag_stages(root: &Path) {
    let Ok(entries) = std::fs::read_dir(root) else {
        return;
    };
    let now = SystemTime::now();
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(name_str) = name.to_str() else {
            continue;
        };
        if !name_str.starts_with(DRAG_STAGE_PREFIX) {
            continue;
        }
        let Ok(meta) = entry.metadata() else {
            continue;
        };
        let Ok(modified) = meta.modified() else {
            continue;
        };
        if now.duration_since(modified).unwrap_or_default() > DRAG_STAGE_MAX_AGE {
            let _ = std::fs::remove_dir_all(entry.path());
        }
    }
}

async fn stage_remote_file(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    job_id: &str,
    registry: &Arc<DragJobRegistry>,
    on_event: &Channel<DragJob>,
) -> Result<(), String> {
    registry
        .wait_if_paused(job_id)
        .await
        .map_err(|e| e.to_string())?;
    let meta = sftp
        .metadata(remote_path)
        .await
        .map_err(|e| format!("stat {remote_path}: {e}"))?;
    let total = meta.size.unwrap_or(0);
    registry.add_progress(job_id, total, 0);
    registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_path.to_string(),
            loaded: 0,
            total,
            done: false,
            error: String::new(),
        },
    );
    if let Some(snap) = registry.get(job_id) {
        let _ = on_event.send(snap);
    }

    let mut remote_file = sftp
        .open(remote_path)
        .await
        .map_err(|e| format!("open {remote_path}: {e}"))?;
    let mut local_file = tokio::fs::File::create(local_path)
        .await
        .map_err(|e| format!("create {}: {e}", local_path.display()))?;

    let mut buf = vec![0u8; STAGE_CHUNK_SIZE];
    let mut loaded: u64 = 0;
    loop {
        registry
            .wait_if_paused(job_id)
            .await
            .map_err(|e| e.to_string())?;
        let n = remote_file
            .read(&mut buf)
            .await
            .map_err(|e| format!("read {remote_path}: {e}"))?;
        if n == 0 {
            break;
        }
        local_file
            .write_all(&buf[..n])
            .await
            .map_err(|e| format!("write {}: {e}", local_path.display()))?;
        loaded = loaded.saturating_add(n as u64);
        registry.add_progress(job_id, 0, n as u64);
        registry.update_file_progress(
            job_id,
            FileProgress {
                path: remote_path.to_string(),
                loaded,
                total: total.max(loaded),
                done: false,
                error: String::new(),
            },
        );
    }
    local_file
        .flush()
        .await
        .map_err(|e| format!("flush {}: {e}", local_path.display()))?;
    registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_path.to_string(),
            loaded,
            total: total.max(loaded),
            done: true,
            error: String::new(),
        },
    );
    if let Some(snap) = registry.get(job_id) {
        let _ = on_event.send(snap);
    }
    Ok(())
}

async fn stage_remote_tree(
    sftp: &SftpSession,
    remote_root: &str,
    local_root: &Path,
    job_id: &str,
    registry: &Arc<DragJobRegistry>,
    on_event: &Channel<DragJob>,
) -> Result<(), String> {
    let mut queue = vec![(remote_root.to_string(), local_root.to_path_buf())];
    while let Some((rdir, ldir)) = queue.pop() {
        registry
            .wait_if_paused(job_id)
            .await
            .map_err(|e| e.to_string())?;
        tokio::fs::create_dir_all(&ldir)
            .await
            .map_err(|e| format!("mkdir {}: {e}", ldir.display()))?;
        let entries = sftp
            .read_dir(&rdir)
            .await
            .map_err(|e| format!("list {rdir}: {e}"))?;
        let mut used = HashSet::new();
        for entry in entries {
            let name = entry.file_name();
            if name == "." || name == ".." {
                continue;
            }
            let rchild = if rdir == "/" {
                format!("/{name}")
            } else {
                format!("{}/{name}", rdir.trim_end_matches('/'))
            };
            let lchild = unique_stage_child(&ldir, &name, &mut used);
            let is_dir = entry.file_type().is_dir();
            if is_dir {
                queue.push((rchild, lchild));
            } else {
                stage_remote_file(sftp, &rchild, &lchild, job_id, registry, on_event).await?;
            }
        }
    }
    Ok(())
}

async fn stage_remote_paths(
    state: &AppState,
    session_id: &str,
    remote_paths: &[String],
    job_id: &str,
    on_event: &Channel<DragJob>,
) -> Result<(PathBuf, Vec<PathBuf>), String> {
    let temp_root = std::env::temp_dir();
    prune_old_drag_stages(&temp_root);
    let safe_id = sanitize_stage_name(job_id);
    let stage_dir = temp_root.join(format!("{DRAG_STAGE_PREFIX}{safe_id}"));
    tokio::fs::create_dir_all(&stage_dir)
        .await
        .map_err(|e| format!("create drag stage dir: {e}"))?;

    let sftp_session = state.sftp.get_session(session_id).await?;
    let (transport, _) = sftp_session.ensure_live().await?;

    let mut staged = Vec::with_capacity(remote_paths.len());
    let mut used = HashSet::new();
    for raw_path in remote_paths {
        let base_name = raw_path
            .trim_end_matches('/')
            .rsplit('/')
            .next()
            .filter(|s| !s.is_empty())
            .unwrap_or("item");
        let local_target = unique_stage_child(&stage_dir, base_name, &mut used);
        let meta = transport
            .sftp
            .metadata(raw_path)
            .await
            .map_err(|e| format!("stat {raw_path}: {e}"))?;
        let res = if meta.is_dir() {
            stage_remote_tree(
                &transport.sftp,
                raw_path,
                &local_target,
                job_id,
                &state.drag_jobs,
                on_event,
            )
            .await
        } else {
            stage_remote_file(
                &transport.sftp,
                raw_path,
                &local_target,
                job_id,
                &state.drag_jobs,
                on_event,
            )
            .await
        };
        if let Err(err) = res {
            let _ = std::fs::remove_dir_all(&stage_dir);
            return Err(err);
        }
        staged.push(local_target);
    }
    Ok((stage_dir, staged))
}

#[tauri::command]
pub async fn start_native_drag_out(
    window: WebviewWindow,
    state: State<'_, AppState>,
    request: DragOutRequest,
    on_event: Channel<DragJob>,
) -> Result<DragDropOutcome, String> {
    if !is_windows_native_supported() {
        return Err("native Windows OLE drag-out is not supported on this platform".into());
    }

    let initial = state
        .drag_jobs
        .add(request.id.clone(), request.session.clone(), request.paths.clone())
        .map_err(|e| e.to_string())?;
    let _ = on_event.send(initial);

    if let Some(updated) = state.drag_jobs.update_state(&request.id, "caching", None) {
        let _ = on_event.send(updated);
    }

    let (stage_dir_opt, staged): (Option<PathBuf>, Vec<PathBuf>) =
        if !request.staged_local_paths.is_empty() {
            (
                None,
                request
                    .staged_local_paths
                    .into_iter()
                    .map(PathBuf::from)
                    .collect(),
            )
        } else {
            match stage_remote_paths(
                &state,
                &request.session,
                &request.paths,
                &request.id,
                &on_event,
            )
            .await
            {
                Ok((dir, files)) => (Some(dir), files),
                Err(err) => {
                    let is_cancel = err.to_ascii_lowercase().contains("cancel");
                    let next_state = if is_cancel { "cancelled" } else { "error" };
                    if let Some(term) = state
                        .drag_jobs
                        .update_state(&request.id, next_state, Some(&err))
                    {
                        let _ = on_event.send(term);
                    }
                    return Err(err);
                }
            }
        };

    if let Some(delivering) = state.drag_jobs.update_state(&request.id, "delivering", None) {
        let _ = on_event.send(delivering);
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
            if let Some(dir) = stage_dir_opt {
                let _ = std::fs::remove_dir_all(dir);
            }
            if let Some(cancelled) =
                registry.update_state(&job_id, "cancelled", Some("drag cancelled"))
            {
                let _ = on_event.send(cancelled);
            }
            Ok(DragDropOutcome::Cancelled)
        }
        Err(err) => {
            if let Some(dir) = stage_dir_opt {
                let _ = std::fs::remove_dir_all(dir);
            }
            let msg = err.to_string();
            if let Some(failed) = registry.update_state(&job_id, "error", Some(&msg)) {
                let _ = on_event.send(failed);
            }
            Err(msg)
        }
    }
}
