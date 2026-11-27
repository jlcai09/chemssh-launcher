use std::collections::HashSet;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex as StdMutex};
use std::time::{Duration, Instant, SystemTime};

use russh_sftp::client::SftpSession;
use serde::{Deserialize, Serialize};
use tauri::ipc::Channel;
use tauri::{AppHandle, Emitter, State, WebviewWindow};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use win_native::{
    is_windows_native_supported, start_shell_file_drag_async, AsyncDragEvent, DragJob,
    DragJobRegistry, FileProgress, NativeDragGestureResult,
};

use crate::state::AppState;

const DRAG_STAGE_PREFIX: &str = "chemssh-drag-";
const DRAG_STAGE_MAX_AGE: Duration = Duration::from_secs(6 * 60 * 60);
const STAGE_CHUNK_SIZE: usize = 256 * 1024;
const STAGE_PROGRESS_EMIT_INTERVAL: Duration = Duration::from_millis(60);
const NATIVE_DRAG_JOB_EVENT: &str = "app://native-drag-job";

static DRAG_GESTURE_ACTIVE: AtomicBool = AtomicBool::new(false);

struct DragGestureGuard;

impl DragGestureGuard {
    fn try_acquire() -> Option<Self> {
        DRAG_GESTURE_ACTIVE
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .ok()
            .map(|_| Self)
    }
}

impl Drop for DragGestureGuard {
    fn drop(&mut self) {
        DRAG_GESTURE_ACTIVE.store(false, Ordering::Release);
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct DragOutRequest {
    pub id: String,
    pub session: String,
    pub paths: Vec<String>,
    #[serde(default)]
    pub marker: String,
    #[serde(default)]
    pub staged_local_paths: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ActiveWorkSummary {
    pub active_transfers: usize,
    pub active_drag_jobs: usize,
}

fn emit_drag_job(app: &AppHandle, on_event: Option<&Channel<DragJob>>, job: DragJob) {
    let _ = app.emit(NATIVE_DRAG_JOB_EVENT, &job);
    if let Some(channel) = on_event {
        let _ = channel.send(job);
    }
}

fn cleanup_stage_dir(slot: &Arc<StdMutex<Option<PathBuf>>>) {
    if let Some(dir) = slot.lock().ok().and_then(|mut g| g.take()) {
        let _ = std::fs::remove_dir_all(dir);
    }
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
    app: AppHandle,
    state: State<'_, AppState>,
    id: String,
    action: String,
) -> Result<DragJob, String> {
    let res = match action.as_str() {
        "pause" => state.drag_jobs.pause(&id).map_err(|e| e.to_string()),
        "resume" => state.drag_jobs.resume(&id).map_err(|e| e.to_string()),
        "cancel" => state.drag_jobs.cancel(&id).map_err(|e| e.to_string()),
        other => Err(format!("unsupported drag job action: {other}")),
    };
    if let Ok(ref job) = res {
        emit_drag_job(&app, None, job.clone());
    }
    res
}

#[tauri::command]
pub fn has_active_work(state: State<'_, AppState>) -> ActiveWorkSummary {
    ActiveWorkSummary {
        active_transfers: state.transfers.active_count(),
        active_drag_jobs: state.drag_jobs.active_count(),
    }
}

#[tauri::command]
pub fn force_close_app(app: tauri::AppHandle, state: State<'_, AppState>) {
    state.transfers.cancel_all(Some("application closing"));
    state.drag_jobs.cancel_all(Some("application closing"));
    app.exit(0);
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

async fn stage_remote_wait<T, F>(
    registry: &Arc<DragJobRegistry>,
    job_id: &str,
    future: F,
) -> Result<T, String>
where
    F: Future<Output = Result<T, String>>,
{
    tokio::select! {
        biased;
        _ = registry.wait_for_cancel(job_id) => Err("drag cancelled".into()),
        res = future => res,
    }
}

async fn stage_remote_file(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    job_id: &str,
    registry: &Arc<DragJobRegistry>,
    app: &AppHandle,
    on_event: &Channel<DragJob>,
) -> Result<(), String> {
    registry
        .wait_if_paused(job_id)
        .await
        .map_err(|e| e.to_string())?;
    let meta = stage_remote_wait(registry, job_id, async {
        sftp.metadata(remote_path)
            .await
            .map_err(|e| format!("stat {remote_path}: {e}"))
    })
    .await?;
    let total = meta.size.unwrap_or(0);
    registry.add_progress(job_id, total, 0);
    if let Some(snap) = registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_path.to_string(),
            kind: "file".into(),
            loaded: 0,
            total,
            state: "caching".into(),
            done: false,
            error: String::new(),
        },
    ) {
        emit_drag_job(app, Some(on_event), snap);
    }

    let mut remote_file = stage_remote_wait(registry, job_id, async {
        sftp.open(remote_path)
            .await
            .map_err(|e| format!("open {remote_path}: {e}"))
    })
    .await?;
    let mut local_file = tokio::fs::File::create(local_path)
        .await
        .map_err(|e| format!("create {}: {e}", local_path.display()))?;

    let mut buf = vec![0u8; STAGE_CHUNK_SIZE];
    let mut loaded: u64 = 0;
    let mut last_emit = Instant::now();
    loop {
        registry
            .wait_if_paused(job_id)
            .await
            .map_err(|e| e.to_string())?;
        let n = stage_remote_wait(registry, job_id, async {
            remote_file
                .read(&mut buf)
                .await
                .map_err(|e| format!("read {remote_path}: {e}"))
        })
        .await?;
        if n == 0 {
            break;
        }
        local_file
            .write_all(&buf[..n])
            .await
            .map_err(|e| format!("write {}: {e}", local_path.display()))?;
        loaded = loaded.saturating_add(n as u64);
        registry.add_progress(job_id, 0, n as u64);
        let snap = registry.update_file_progress(
            job_id,
            FileProgress {
                path: remote_path.to_string(),
                kind: "file".into(),
                loaded,
                total: total.max(loaded),
                state: "caching".into(),
                done: false,
                error: String::new(),
            },
        );
        if last_emit.elapsed() >= STAGE_PROGRESS_EMIT_INTERVAL {
            if let Some(snap) = snap {
                emit_drag_job(app, Some(on_event), snap);
                last_emit = Instant::now();
            }
        }
    }
    local_file
        .flush()
        .await
        .map_err(|e| format!("flush {}: {e}", local_path.display()))?;
    if let Some(snap) = registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_path.to_string(),
            kind: "file".into(),
            loaded,
            total: total.max(loaded),
            state: "done".into(),
            done: true,
            error: String::new(),
        },
    ) {
        emit_drag_job(app, Some(on_event), snap);
    }
    Ok(())
}

async fn stage_remote_tree(
    sftp: &SftpSession,
    remote_root: &str,
    local_root: &Path,
    job_id: &str,
    registry: &Arc<DragJobRegistry>,
    app: &AppHandle,
    on_event: &Channel<DragJob>,
) -> Result<(), String> {
    if let Some(snap) = registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_root.to_string(),
            kind: "folder".into(),
            loaded: 0,
            total: 0,
            state: "caching".into(),
            done: false,
            error: String::new(),
        },
    ) {
        emit_drag_job(app, Some(on_event), snap);
    }

    let mut queue = vec![(remote_root.to_string(), local_root.to_path_buf())];
    while let Some((rdir, ldir)) = queue.pop() {
        registry
            .wait_if_paused(job_id)
            .await
            .map_err(|e| e.to_string())?;
        tokio::fs::create_dir_all(&ldir)
            .await
            .map_err(|e| format!("mkdir {}: {e}", ldir.display()))?;
        let entries = stage_remote_wait(registry, job_id, async {
            sftp.read_dir(&rdir)
                .await
                .map_err(|e| format!("list {rdir}: {e}"))
        })
        .await?;
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
                stage_remote_file(sftp, &rchild, &lchild, job_id, registry, app, on_event).await?;
            }
        }
    }

    if let Some(snap) = registry.update_file_progress(
        job_id,
        FileProgress {
            path: remote_root.to_string(),
            kind: "folder".into(),
            loaded: 0,
            total: 0,
            state: "done".into(),
            done: true,
            error: String::new(),
        },
    ) {
        emit_drag_job(app, Some(on_event), snap);
    }
    Ok(())
}

async fn stage_remote_paths(
    state: &AppState,
    session_id: &str,
    remote_paths: &[String],
    job_id: &str,
    app: &AppHandle,
    on_event: &Channel<DragJob>,
) -> Result<(PathBuf, Vec<PathBuf>), String> {
    let temp_root = std::env::temp_dir();
    prune_old_drag_stages(&temp_root);
    let safe_id = sanitize_stage_name(job_id);
    let stage_dir = temp_root.join(format!("{DRAG_STAGE_PREFIX}{safe_id}"));
    tokio::fs::create_dir_all(&stage_dir)
        .await
        .map_err(|e| format!("create drag stage dir: {e}"))?;

    let sftp_session = stage_remote_wait(&state.drag_jobs, job_id, async {
        state.sftp.get_session(session_id).await
    })
    .await?;
    let (transport, _) = stage_remote_wait(&state.drag_jobs, job_id, async {
        sftp_session.ensure_live().await
    })
    .await?;

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
        let meta = stage_remote_wait(&state.drag_jobs, job_id, async {
            transport
                .sftp
                .metadata(raw_path)
                .await
                .map_err(|e| format!("stat {raw_path}: {e}"))
        })
        .await?;
        let res = if meta.is_dir() {
            stage_remote_tree(
                &transport.sftp,
                raw_path,
                &local_target,
                job_id,
                &state.drag_jobs,
                app,
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
                app,
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
    state
        .drag_jobs
        .wait_if_paused(job_id)
        .await
        .map_err(|e| {
            let _ = std::fs::remove_dir_all(&stage_dir);
            e.to_string()
        })?;
    Ok((stage_dir, staged))
}

#[tauri::command]
pub async fn start_native_drag_out(
    app: AppHandle,
    window: WebviewWindow,
    state: State<'_, AppState>,
    request: DragOutRequest,
    on_event: Channel<DragJob>,
) -> Result<NativeDragGestureResult, String> {
    if !is_windows_native_supported() {
        return Err("native Windows OLE drag-out is not supported on this platform".into());
    }

    let Some(_gesture_guard) = DragGestureGuard::try_acquire() else {
        return Err("another native drag is active".into());
    };

    let initial = state
        .drag_jobs
        .add(
            request.id.clone(),
            request.session.clone(),
            request.paths.clone(),
        )
        .map_err(|e| e.to_string())?;
    emit_drag_job(&app, Some(&on_event), initial);

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
    let marker = request.marker.clone();
    let prestaged: Vec<PathBuf> = request
        .staged_local_paths
        .into_iter()
        .map(PathBuf::from)
        .collect();
    let app_state = state.inner().clone();
    let session_id = request.session.clone();
    let remote_paths = request.paths.clone();
    let stage_dir_slot: Arc<StdMutex<Option<PathBuf>>> = Arc::new(StdMutex::new(None));
    let rt_handle = tokio::runtime::Handle::current();

    let stage_dir_for_stage = Arc::clone(&stage_dir_slot);
    let reg_for_stage = Arc::clone(&registry);
    let job_id_for_stage = job_id.clone();
    let app_for_stage = app.clone();
    let event_for_stage = on_event.clone();

    let stage_fn = move || {
        if reg_for_stage.is_cancelled(&job_id_for_stage) {
            return Err("drag cancelled".into());
        }
        if !prestaged.is_empty() {
            return Ok(prestaged);
        }
        if let Some(updated) = reg_for_stage.update_state(&job_id_for_stage, "caching", None) {
            emit_drag_job(&app_for_stage, Some(&event_for_stage), updated);
        }
        match rt_handle.block_on(stage_remote_paths(
            &app_state,
            &session_id,
            &remote_paths,
            &job_id_for_stage,
            &app_for_stage,
            &event_for_stage,
        )) {
            Ok((dir, files)) => {
                if let Ok(mut guard) = stage_dir_for_stage.lock() {
                    *guard = Some(dir);
                }
                if let Some(delivering) =
                    reg_for_stage.update_state(&job_id_for_stage, "delivering", None)
                {
                    emit_drag_job(&app_for_stage, Some(&event_for_stage), delivering);
                }
                Ok(files)
            }
            Err(err) => Err(err),
        }
    };

    let stage_dir_for_notify = Arc::clone(&stage_dir_slot);
    let reg_for_notify = Arc::clone(&registry);
    let job_id_for_notify = job_id.clone();
    let app_for_notify = app.clone();
    let event_for_notify = on_event.clone();

    let notify_fn = move |event: AsyncDragEvent| match event {
        AsyncDragEvent::Started => {
            if let Some(snap) = reg_for_notify.update_state(&job_id_for_notify, "accepted", None) {
                emit_drag_job(&app_for_notify, Some(&event_for_notify), snap);
            }
        }
        AsyncDragEvent::Done { .. } => {
            cleanup_stage_dir(&stage_dir_for_notify);
            if let Some(snap) = reg_for_notify.update_state(&job_id_for_notify, "done", None) {
                emit_drag_job(&app_for_notify, Some(&event_for_notify), snap);
            }
        }
        AsyncDragEvent::Cancelled { reason } => {
            cleanup_stage_dir(&stage_dir_for_notify);
            if let Some(snap) = reg_for_notify.update_state(
                &job_id_for_notify,
                "cancelled",
                reason.as_deref(),
            ) {
                emit_drag_job(&app_for_notify, Some(&event_for_notify), snap);
            }
        }
        AsyncDragEvent::Error(msg) => {
            cleanup_stage_dir(&stage_dir_for_notify);
            let state_name = if reg_for_notify.is_cancelled(&job_id_for_notify) {
                "cancelled"
            } else {
                "error"
            };
            if let Some(snap) =
                reg_for_notify.update_state(&job_id_for_notify, state_name, Some(&msg))
            {
                emit_drag_job(&app_for_notify, Some(&event_for_notify), snap);
            }
        }
    };

    let reg_for_cancel = Arc::clone(&registry);
    let job_id_for_cancel = job_id.clone();
    let is_cancelled_fn = move || reg_for_cancel.is_cancelled(&job_id_for_cancel);

    let (tx, rx) = tokio::sync::oneshot::channel();
    // Windows OLE DoDragDrop MUST run on the window's main UI STA thread so it
    // coordinates cleanly with WebView2 and Desktop/Explorer drop targets.
    // Because our IDataObject implements IDataObjectAsyncCapability on an MTA
    // apartment, DoDragDrop returns immediately on drop without blocking the UI.
    window
        .run_on_main_thread(move || {
            let res = start_shell_file_drag_async(
                hwnd_raw,
                &marker,
                stage_fn,
                notify_fn,
                is_cancelled_fn,
            );
            let _ = tx.send(res);
        })
        .map_err(|e| format!("failed to dispatch OLE drag on UI thread: {e}"))?;

    let outcome = rx.await.map_err(|e| format!("drag task join error: {e}"))?;

    match outcome {
        Ok(gesture) => {
            if !gesture.pending {
                cleanup_stage_dir(&stage_dir_slot);
                let terminal_state = if gesture.ok { "cancelled" } else { "error" };
                let err_msg = if gesture.ok {
                    None
                } else {
                    Some(gesture.status.as_str())
                };
                if let Some(snap) = registry.update_state(&job_id, terminal_state, err_msg) {
                    emit_drag_job(&app, Some(&on_event), snap);
                }
            }
            Ok(gesture)
        }
        Err(err) => {
            cleanup_stage_dir(&stage_dir_slot);
            let msg = err.to_string();
            if let Some(failed) = registry.update_state(&job_id, "error", Some(&msg)) {
                emit_drag_job(&app, Some(&on_event), failed);
            }
            Err(msg)
        }
    }
}
