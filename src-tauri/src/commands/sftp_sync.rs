use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use chrono::{SecondsFormat, Utc};
use core_transfer::{remote_io_err, sftp_err, TransferError};
use russh_sftp::protocol::OpenFlags;
use serde::Serialize;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::Mutex;

use crate::commands::sftp::SftpManager;
use crate::state::LauncherLogStore;

const POLL_INTERVAL: Duration = Duration::from_millis(500);
const DEBOUNCE_DURATION: Duration = Duration::from_millis(500);
const ERROR_RETRY_BACKOFF: Duration = Duration::from_secs(3);
const MISSING_REMOVE_THRESHOLD: u8 = 3;
pub const MAX_SYNC_FILE_BYTES: u64 = 100 * 1024 * 1024;
const MAX_SYNC_EVENTS: usize = 500;
const CHUNK_SIZE: usize = 256 * 1024;

#[derive(Debug, Clone, Serialize)]
pub struct OpenSyncEvent {
    pub seq: u64,
    pub time: String,
    pub session: String,
    pub remote_path: String,
    pub local_path: String,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

struct WatchedRemoteFile {
    session_id: String,
    remote_path: String,
    local_path: PathBuf,
    last_synced_mtime: Option<SystemTime>,
    last_synced_len: u64,
    last_seen_mtime: Option<SystemTime>,
    last_seen_len: u64,
    pending_since: Option<Instant>,
    next_retry_at: Option<Instant>,
    missing_polls: u8,
}

#[derive(Default)]
pub struct SftpOpenSyncManager {
    watches: Mutex<HashMap<String, WatchedRemoteFile>>,
    events: Mutex<VecDeque<OpenSyncEvent>>,
    next_seq: AtomicU64,
    worker_started: AtomicBool,
}

impl SftpOpenSyncManager {
    pub fn new() -> Self {
        Self {
            watches: Mutex::new(HashMap::new()),
            events: Mutex::new(VecDeque::new()),
            next_seq: AtomicU64::new(1),
            worker_started: AtomicBool::new(false),
        }
    }

    pub async fn register(
        self: &Arc<Self>,
        sftp_manager: Arc<SftpManager>,
        launcher_logs: Arc<LauncherLogStore>,
        session_id: String,
        remote_path: String,
        local_path: PathBuf,
    ) {
        let meta = tokio::fs::metadata(&local_path).await.ok();
        let mtime = meta.as_ref().and_then(|m| m.modified().ok());
        let len = meta.as_ref().map(|m| m.len()).unwrap_or(0);
        let key = format!("{session_id}\0{remote_path}");

        {
            let mut guard = self.watches.lock().await;
            guard.insert(
                key,
                WatchedRemoteFile {
                    session_id,
                    remote_path,
                    local_path,
                    last_synced_mtime: mtime,
                    last_synced_len: len,
                    last_seen_mtime: mtime,
                    last_seen_len: len,
                    pending_since: None,
                    next_retry_at: None,
                    missing_polls: 0,
                },
            );
        }

        if !self.worker_started.swap(true, Ordering::SeqCst) {
            let mgr = Arc::clone(self);
            tokio::spawn(async move {
                mgr.run_loop(sftp_manager, launcher_logs).await;
            });
        }
    }

    pub async fn unregister_session(&self, session_id: &str) {
        let mut guard = self.watches.lock().await;
        guard.retain(|_, watch| watch.session_id != session_id);
    }

    pub async fn events_after(&self, after_seq: u64) -> Vec<OpenSyncEvent> {
        let guard = self.events.lock().await;
        guard
            .iter()
            .filter(|ev| ev.seq > after_seq)
            .cloned()
            .collect()
    }

    pub async fn events_after_for_session(
        &self,
        session_id: &str,
        after_seq: u64,
    ) -> Vec<OpenSyncEvent> {
        if session_id.is_empty() {
            return Vec::new();
        }
        let guard = self.events.lock().await;
        guard
            .iter()
            .filter(|ev| ev.seq > after_seq && ev.session == session_id)
            .cloned()
            .collect()
    }

    pub(crate) async fn push_event(
        &self,
        session: &str,
        remote_path: &str,
        local_path: &str,
        status: &str,
        error: Option<String>,
    ) {
        let seq = self.next_seq.fetch_add(1, Ordering::SeqCst);
        let event = OpenSyncEvent {
            seq,
            time: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
            session: session.to_string(),
            remote_path: remote_path.to_string(),
            local_path: local_path.to_string(),
            status: status.to_string(),
            error,
        };
        let mut guard = self.events.lock().await;
        while guard.len() >= MAX_SYNC_EVENTS {
            guard.pop_front();
        }
        guard.push_back(event);
    }

    async fn run_loop(
        self: Arc<Self>,
        sftp_manager: Arc<SftpManager>,
        launcher_logs: Arc<LauncherLogStore>,
    ) {
        let mut ticker = tokio::time::interval(POLL_INTERVAL);
        loop {
            ticker.tick().await;
            self.poll_once(&sftp_manager, &launcher_logs).await;
        }
    }

    async fn poll_once(&self, sftp_manager: &SftpManager, launcher_logs: &LauncherLogStore) {
        let now = Instant::now();
        let mut ready_uploads: Vec<(String, String, String, PathBuf, Option<SystemTime>, u64)> =
            Vec::new();
        let mut oversized_errors: Vec<(String, String, String, String)> = Vec::new();
        let mut remove_keys: Vec<String> = Vec::new();

        {
            let mut guard = self.watches.lock().await;
            for (key, watch) in guard.iter_mut() {
                let meta = match std::fs::metadata(&watch.local_path) {
                    Ok(m) => {
                        watch.missing_polls = 0;
                        m
                    }
                    Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                        // Tolerate brief disappearance from atomic-save editors.
                        watch.missing_polls = watch.missing_polls.saturating_add(1);
                        if watch.missing_polls >= MISSING_REMOVE_THRESHOLD {
                            remove_keys.push(key.clone());
                        }
                        continue;
                    }
                    Err(_) => continue,
                };
                let mtime = meta.modified().ok();
                let len = meta.len();

                if mtime == watch.last_synced_mtime && len == watch.last_synced_len {
                    watch.pending_since = None;
                    continue;
                }

                if mtime != watch.last_seen_mtime || len != watch.last_seen_len {
                    watch.last_seen_mtime = mtime;
                    watch.last_seen_len = len;
                    watch.pending_since = Some(now);
                    continue;
                }

                let Some(pending_since) = watch.pending_since else {
                    watch.pending_since = Some(now);
                    continue;
                };
                if now.duration_since(pending_since) < DEBOUNCE_DURATION {
                    continue;
                }
                if let Some(retry_at) = watch.next_retry_at {
                    if now < retry_at {
                        continue;
                    }
                }

                if len > MAX_SYNC_FILE_BYTES {
                    watch.last_synced_mtime = mtime;
                    watch.last_synced_len = len;
                    watch.pending_since = None;
                    oversized_errors.push((
                        watch.session_id.clone(),
                        watch.remote_path.clone(),
                        watch.local_path.display().to_string(),
                        format!(
                            "file size {} exceeds auto-sync limit ({MAX_SYNC_FILE_BYTES} bytes)",
                            len
                        ),
                    ));
                    continue;
                }

                ready_uploads.push((
                    key.clone(),
                    watch.session_id.clone(),
                    watch.remote_path.clone(),
                    watch.local_path.clone(),
                    mtime,
                    len,
                ));
            }
            for key in &remove_keys {
                guard.remove(key);
            }
        }

        for (sid, rpath, lpath, err) in oversized_errors {
            launcher_logs.push(format!("[sftp-sync] skipped {rpath}: {err}"));
            self.push_event(&sid, &rpath, &lpath, "error", Some(err))
                .await;
        }

        for (key, session_id, remote_path, local_path, target_mtime, target_len) in ready_uploads {
            let local_str = local_path.display().to_string();
            match upload_cached_file(sftp_manager, &session_id, &remote_path, &local_path).await {
                Ok(()) => {
                    let mut guard = self.watches.lock().await;
                    if let Some(watch) = guard.get_mut(&key) {
                        watch.last_synced_mtime = target_mtime;
                        watch.last_synced_len = target_len;
                        watch.pending_since = None;
                        watch.next_retry_at = None;
                    }
                    launcher_logs.push(format!("[sftp-sync] synced {remote_path}"));
                    self.push_event(&session_id, &remote_path, &local_str, "done", None)
                        .await;
                }
                Err(err) => {
                    let is_disconnected = err.contains("sftp session not connected")
                        || err.contains("sftp session disconnected");
                    {
                        let mut guard = self.watches.lock().await;
                        if is_disconnected {
                            guard.remove(&key);
                        } else if let Some(watch) = guard.get_mut(&key) {
                            watch.next_retry_at = Some(Instant::now() + ERROR_RETRY_BACKOFF);
                        }
                    }
                    launcher_logs.push(format!("[sftp-sync] failed {remote_path}: {err}"));
                    self.push_event(&session_id, &remote_path, &local_str, "error", Some(err))
                        .await;
                }
            }
        }
    }
}

async fn upload_cached_file(
    sftp_manager: &SftpManager,
    session_id: &str,
    remote_path: &str,
    local_path: &PathBuf,
) -> Result<(), String> {
    let remote_path_owned = remote_path.to_string();
    let local_path_owned = local_path.clone();

    sftp_manager
        .run_sftp_op(session_id, move |sftp| {
            let rpath = remote_path_owned.clone();
            let lpath = local_path_owned.clone();
            async move {
                let mut local_file = tokio::fs::File::open(&lpath)
                    .await
                    .map_err(TransferError::Io)?;
                let mut remote_file = sftp
                    .open_with_flags(
                        &rpath,
                        OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
                    )
                    .await
                    .map_err(|e| sftp_err(format!("open remote file {rpath}"), e))?;

                let mut buf = vec![0u8; CHUNK_SIZE];
                loop {
                    let n = local_file
                        .read(&mut buf)
                        .await
                        .map_err(TransferError::Io)?;
                    if n == 0 {
                        break;
                    }
                    remote_file
                        .write_all(&buf[..n])
                        .await
                        .map_err(|e| remote_io_err(format!("write remote file {rpath}"), e))?;
                }
                remote_file
                    .shutdown()
                    .await
                    .map_err(|e| remote_io_err(format!("flush remote file {rpath}"), e))?;
                Ok(())
            }
        })
        .await
}
