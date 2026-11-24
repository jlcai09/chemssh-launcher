use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use core_config::{new_id, Profile, ProfileStore};
use core_ssh::{shell_quote, HostKeyPolicy, KnownHostsVerifier, SshConnection};
use core_transfer::{
    build_compress_step, build_extract_step, copy_remote_to_remote, delete_remote_path,
    detect_remote_tools, download_file_resumable, list_remote_dir, run_remote_step,
    upload_file_resumable, ArchiveTools, FsEntry,
};
use russh_sftp::client::fs::File as SftpRemoteFile;
use russh_sftp::client::SftpSession;
use russh_sftp::protocol::OpenFlags;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tauri::State;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::Mutex;

use crate::commands::backend::{open_path_in_system, open_text_file_in_system};
use crate::commands::sessions::{format_ssh_error, load_ssh_credentials};
use crate::commands::sftp_sync::{OpenSyncEvent, MAX_SYNC_FILE_BYTES};
use crate::state::AppState;

const DEFAULT_DOWNLOAD_BYTES_LIMIT: u64 = 256 * 1024 * 1024;
const DOWNLOAD_CHUNK_SIZE: usize = 256 * 1024;

#[derive(Debug, Clone, Serialize)]
pub struct SftpConnectResponse {
    pub id: String,
    pub path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct SftpOpenFileResponse {
    pub path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct OpenSyncEventsResponse {
    pub events: Vec<OpenSyncEvent>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopySupportResponse {
    pub supported: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerCopyItem {
    pub source_path: String,
    pub relative_path: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopyItemResult {
    pub source_path: String,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ServerCopyResponse {
    pub supported: bool,
    pub results: Vec<ServerCopyItemResult>,
}

#[derive(Debug, Clone, Serialize)]
pub struct UploadBytesBeginResponse {
    pub upload_id: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct DownloadBytesResponse {
    pub name: String,
    pub data_b64: String,
    pub size: u64,
}

#[derive(Clone)]
pub(crate) struct SftpEntry {
    pub connection: Arc<SshConnection>,
    pub sftp: Arc<SftpSession>,
    pub profile_id: String,
    pub ssh_host: String,
    pub ssh_port: u16,
    pub ssh_user: String,
}

pub struct ActiveByteUpload {
    file: SftpRemoteFile,
    transfer_id: String,
}

#[derive(Default)]
pub struct SftpManager {
    sessions: Mutex<HashMap<String, SftpEntry>>,
    by_profile: Mutex<HashMap<String, String>>,
    uploads: Mutex<HashMap<String, Arc<Mutex<ActiveByteUpload>>>>,
}

impl SftpManager {
    pub(crate) async fn get_entry_with_id(
        &self,
        id: &str,
    ) -> Result<(String, SftpEntry), String> {
        let sessions = self.sessions.lock().await;
        if let Some(entry) = sessions.get(id) {
            return Ok((id.to_string(), entry.clone()));
        }
        drop(sessions);

        let mapped_id = self.by_profile.lock().await.get(id).cloned();
        if let Some(sid) = mapped_id {
            let sessions = self.sessions.lock().await;
            if let Some(entry) = sessions.get(&sid) {
                return Ok((sid, entry.clone()));
            }
        }
        Err(format!("sftp session not connected: {id}"))
    }

    pub(crate) async fn get_entry(
        &self,
        id: &str,
    ) -> Result<(Arc<SshConnection>, Arc<SftpSession>), String> {
        let (_, entry) = self.get_entry_with_id(id).await?;
        Ok((entry.connection, entry.sftp))
    }

    pub(crate) async fn connect_dedicated(
        &self,
        state: &AppState,
        profile: &Profile,
    ) -> Result<String, String> {
        let (entry, _) =
            connect_sftp_for_profile(state, profile, Some(HostKeyPolicy::Strict)).await?;
        let session_id = new_id();
        // Dedicated bridge sessions are never registered in `by_profile` so
        // user-opened SFTP tabs keep their own session mapping untouched.
        self.sessions
            .lock()
            .await
            .insert(session_id.clone(), entry);
        Ok(session_id)
    }

    pub(crate) async fn is_connected(&self, session_id: &str) -> bool {
        if session_id.is_empty() {
            return false;
        }
        self.sessions.lock().await.contains_key(session_id)
    }

    pub(crate) async fn disconnect_session(&self, session_id: &str) {
        if session_id.is_empty() {
            return;
        }
        let entry = self.sessions.lock().await.remove(session_id);
        let mut by_profile = self.by_profile.lock().await;
        by_profile.retain(|_, v| v != session_id);
        drop(by_profile);
        if let Some(entry) = entry {
            entry.connection.disconnect().await;
        }
    }

    pub async fn disconnect_all(&self) {
        let entries: Vec<SftpEntry> = {
            let mut sessions = self.sessions.lock().await;
            let mut by_profile = self.by_profile.lock().await;
            by_profile.clear();
            sessions.drain().map(|(_, v)| v).collect()
        };
        let _ = self.uploads.lock().await.drain();
        for entry in entries {
            entry.connection.disconnect().await;
        }
    }
}

async fn connect_sftp_for_profile(
    state: &AppState,
    profile: &Profile,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<(SftpEntry, String), String> {
    if profile.is_local() {
        return Err("local profiles do not support SFTP".into());
    }
    let creds = load_ssh_credentials(state, profile)?;
    let verifier = KnownHostsVerifier::new(
        &state.known_hosts_path,
        &profile.ssh_host,
        profile.ssh_port,
        host_key_policy.unwrap_or(HostKeyPolicy::Strict),
    );
    let connection = Arc::new(
        SshConnection::connect(profile, &creds, verifier)
            .await
            .map_err(format_ssh_error)?,
    );
    let sftp = Arc::new(connection.open_sftp().await.map_err(|e| e.to_string())?);
    let home = sftp
        .canonicalize(".")
        .await
        .unwrap_or_else(|_| "/".to_string());

    Ok((
        SftpEntry {
            connection,
            sftp,
            profile_id: profile.id.clone(),
            ssh_host: profile.ssh_host.trim().to_ascii_lowercase(),
            ssh_port: profile.ssh_port,
            ssh_user: profile.ssh_user.trim().to_string(),
        },
        home,
    ))
}

#[tauri::command]
pub async fn sftp_connect(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SftpConnectResponse, String> {
    let profile = state
        .profiles
        .get(&profile_id)
        .map_err(|e| e.to_string())?;
    let (entry, home) = connect_sftp_for_profile(&state, &profile, host_key_policy).await?;

    let session_id = new_id();
    state
        .sftp
        .sessions
        .lock()
        .await
        .insert(session_id.clone(), entry);
    state
        .sftp
        .by_profile
        .lock()
        .await
        .insert(profile.id.clone(), session_id.clone());

    Ok(SftpConnectResponse {
        id: session_id,
        path: home,
    })
}

#[tauri::command]
pub async fn sftp_reconnect(
    state: State<'_, AppState>,
    profile_id: String,
    host_key_policy: Option<HostKeyPolicy>,
) -> Result<SftpConnectResponse, String> {
    let existing = state.sftp.get_entry_with_id(&profile_id).await.ok();
    let (session_id, resolved_profile_id, prev_entry) = match existing {
        Some((sid, entry)) => {
            let pid = entry.profile_id.clone();
            (sid, pid, Some(entry))
        }
        None => (profile_id.clone(), profile_id.clone(), None),
    };

    let profile = state
        .profiles
        .get(&resolved_profile_id)
        .map_err(|e| e.to_string())?;
    let (entry, home) = connect_sftp_for_profile(&state, &profile, host_key_policy).await?;

    state
        .sftp
        .sessions
        .lock()
        .await
        .insert(session_id.clone(), entry);
    state
        .sftp
        .by_profile
        .lock()
        .await
        .insert(profile.id.clone(), session_id.clone());

    if let Some(old) = prev_entry {
        tokio::spawn(async move {
            old.connection.disconnect().await;
        });
    }

    Ok(SftpConnectResponse {
        id: session_id,
        path: home,
    })
}

#[tauri::command]
pub async fn sftp_disconnect(state: State<'_, AppState>, profile_id: String) -> Result<(), String> {
    let sid = {
        let sessions = state.sftp.sessions.lock().await;
        if sessions.contains_key(&profile_id) {
            profile_id.clone()
        } else {
            state
                .sftp
                .by_profile
                .lock()
                .await
                .get(&profile_id)
                .cloned()
                .unwrap_or_else(|| profile_id.clone())
        }
    };

    state.sftp_sync.unregister_session(&sid).await;
    let removed = state.sftp.sessions.lock().await.remove(&sid);
    if let Some(entry) = removed {
        let mut by_profile = state.sftp.by_profile.lock().await;
        if by_profile.get(&entry.profile_id).map(|v| v == &sid).unwrap_or(false) {
            by_profile.remove(&entry.profile_id);
        }
        drop(by_profile);
        entry.connection.disconnect().await;
    }
    Ok(())
}

#[tauri::command]
pub async fn sftp_list(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<Vec<FsEntry>, String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    list_remote_dir(&sftp, &path)
        .await
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_realpath(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<String, String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    sftp.canonicalize(&path).await.map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_mkdir(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    mkdir_remote_recursive(&sftp, &path).await
}

#[tauri::command]
pub async fn sftp_mkdirs(
    state: State<'_, AppState>,
    profile_id: String,
    paths: Vec<String>,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    for path in paths {
        mkdir_remote_recursive(&sftp, &path).await?;
    }
    Ok(())
}

async fn mkdir_remote_recursive(sftp: &SftpSession, path: &str) -> Result<(), String> {
    let trimmed = path.trim();
    if trimmed.is_empty() || trimmed == "/" || trimmed == "." {
        return Ok(());
    }
    if sftp.try_exists(trimmed).await.unwrap_or(false) {
        return Ok(());
    }
    let is_abs = trimmed.starts_with('/');
    let mut cur = String::new();
    for seg in trimmed.split('/').filter(|s| !s.is_empty()) {
        if is_abs || !cur.is_empty() {
            cur.push('/');
        }
        cur.push_str(seg);
        if !sftp.try_exists(&cur).await.unwrap_or(false) {
            let _ = sftp.create_dir(&cur).await;
        }
    }
    Ok(())
}

#[tauri::command]
pub async fn sftp_create_file(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    let _file = sftp
        .open_with_flags(
            &path,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::EXCLUDE,
        )
        .await
        .map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
pub async fn sftp_rename(
    state: State<'_, AppState>,
    profile_id: String,
    from: String,
    to: String,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    sftp.rename(&from, &to).await.map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_delete(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
    is_dir: Option<bool>,
) -> Result<(), String> {
    let _ = is_dir;
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    delete_remote_path(&sftp, &path)
        .await
        .map_err(|e| e.to_string())
}

pub(crate) fn sftp_open_cache_path(
    cache_root: &Path,
    session_id: &str,
    remote_path: &str,
) -> Result<PathBuf, String> {
    let mut hasher = Sha256::new();
    hasher.update(session_id.as_bytes());
    hasher.update(b"\n");
    hasher.update(remote_path.as_bytes());
    let digest = hasher.finalize();
    let bucket: String = digest[..8].iter().map(|b| format!("{b:02x}")).collect();

    let raw_name = remote_path
        .rsplit('/')
        .next()
        .unwrap_or("")
        .trim();
    let safe_name = if raw_name.is_empty() || raw_name == "." || raw_name == ".." {
        "remote-file".to_string()
    } else {
        sanitize_filename_segment(raw_name)
    };
    Ok(cache_root.join(bucket).join(safe_name))
}

pub(crate) async fn cache_sftp_file_for_open(
    state: &AppState,
    session_id: &str,
    remote_path: &str,
) -> Result<PathBuf, String> {
    let (resolved_sid, entry) = state.sftp.get_entry_with_id(session_id).await?;
    let meta = entry
        .sftp
        .metadata(remote_path)
        .await
        .map_err(|e| format!("stat {remote_path}: {e}"))?;
    if meta.is_dir() {
        return Err("cannot open a directory as a file".into());
    }
    let size = meta.size.unwrap_or(0);
    if size > MAX_SYNC_FILE_BYTES {
        return Err(format!(
            "file is too large to open with sync (limit 100 MB): {size} bytes"
        ));
    }

    let local_path = sftp_open_cache_path(&state.sftp_cache_dir, &resolved_sid, remote_path)?;
    if let Some(parent) = local_path.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| format!("create cache dir: {e}"))?;
    }

    let transfer_id = format!("open-{}", new_id());
    download_file_resumable(
        &entry.sftp,
        remote_path,
        &local_path,
        &transfer_id,
        false,
        &state.transfers,
    )
    .await
    .map_err(|e| e.to_string())?;

    Ok(local_path)
}

#[tauri::command]
pub async fn sftp_open_file(
    state: State<'_, AppState>,
    profile_id: String,
    path: String,
    as_text: Option<bool>,
) -> Result<SftpOpenFileResponse, String> {
    let (session_id, _) = state.sftp.get_entry_with_id(&profile_id).await?;
    let local_path = cache_sftp_file_for_open(&state, &session_id, &path).await?;

    state
        .sftp_sync
        .register(
            Arc::clone(&state.sftp),
            Arc::clone(&state.launcher_logs),
            session_id,
            path,
            local_path.clone(),
        )
        .await;

    if as_text.unwrap_or(false) {
        open_text_file_in_system(&local_path)?;
    } else {
        open_path_in_system(&local_path)?;
    }

    Ok(SftpOpenFileResponse {
        path: local_path.display().to_string(),
    })
}

#[tauri::command]
pub async fn sftp_open_sync_events(
    state: State<'_, AppState>,
    after: Option<u64>,
) -> Result<OpenSyncEventsResponse, String> {
    let events = state.sftp_sync.events_after(after.unwrap_or(0)).await;
    Ok(OpenSyncEventsResponse { events })
}

#[tauri::command]
pub async fn sftp_server_copy_support(
    state: State<'_, AppState>,
    source: String,
    target: String,
) -> Result<ServerCopySupportResponse, String> {
    let supported = match (
        state.sftp.get_entry_with_id(&source).await,
        state.sftp.get_entry_with_id(&target).await,
    ) {
        (Ok((_, s)), Ok((_, t))) => is_same_remote_host(&s, &t),
        _ => false,
    };
    Ok(ServerCopySupportResponse { supported })
}

#[tauri::command]
pub async fn sftp_copy_on_server(
    state: State<'_, AppState>,
    source_session: String,
    target_session: String,
    target_path: String,
    items: Vec<ServerCopyItem>,
) -> Result<ServerCopyResponse, String> {
    let src_entry = state.sftp.get_entry_with_id(&source_session).await;
    let dst_entry = state.sftp.get_entry_with_id(&target_session).await;
    let (Ok((_, src)), Ok((_, dst))) = (src_entry, dst_entry) else {
        return Ok(ServerCopyResponse {
            supported: false,
            results: Vec::new(),
        });
    };
    if !is_same_remote_host(&src, &dst) {
        return Ok(ServerCopyResponse {
            supported: false,
            results: Vec::new(),
        });
    }

    let mut results = Vec::with_capacity(items.len());
    let base = target_path.trim_end_matches('/');
    for item in items {
        let rel = item.relative_path.trim_start_matches('/');
        if rel.is_empty()
            || rel.split('/').any(|s| s.is_empty() || s == "." || s == "..")
        {
            results.push(ServerCopyItemResult {
                source_path: item.source_path,
                ok: false,
                error: Some("unsafe relative target path".into()),
            });
            continue;
        }
        let dst_item_path = if base.is_empty() {
            format!("/{rel}")
        } else {
            format!("{base}/{rel}")
        };
        let parent_dir = dst_item_path
            .rsplit_once('/')
            .map(|(p, _)| if p.is_empty() { "/" } else { p })
            .unwrap_or("/");
        let cmd = format!(
            "mkdir -p -- {} && cp -R -P -- {} {}",
            shell_quote(parent_dir),
            shell_quote(&item.source_path),
            shell_quote(&dst_item_path)
        );
        match dst.connection.exec_command(&cmd).await {
            Ok(out) if out.exit_code == Some(0) => {
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: true,
                    error: None,
                });
            }
            Ok(out) => {
                let err = out.stderr.trim();
                let msg = if err.is_empty() {
                    format!("cp exited with code {:?}", out.exit_code)
                } else {
                    err.to_string()
                };
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: false,
                    error: Some(msg),
                });
            }
            Err(err) => {
                results.push(ServerCopyItemResult {
                    source_path: item.source_path,
                    ok: false,
                    error: Some(err.to_string()),
                });
            }
        }
    }

    Ok(ServerCopyResponse {
        supported: true,
        results,
    })
}

fn is_same_remote_host(a: &SftpEntry, b: &SftpEntry) -> bool {
    a.profile_id == b.profile_id
        || (a.ssh_host == b.ssh_host && a.ssh_port == b.ssh_port && a.ssh_user == b.ssh_user)
}

fn sanitize_filename_segment(seg: &str) -> String {
    seg.chars()
        .map(|c| {
            if matches!(c, '<' | '>' | ':' | '"' | '/' | '\\' | '|' | '?' | '*') {
                '_'
            } else {
                c
            }
        })
        .collect()
}

#[tauri::command]
pub async fn sftp_upload_local(
    state: State<'_, AppState>,
    profile_id: String,
    local_path: String,
    remote_path: String,
    transfer_id: String,
    resume: Option<bool>,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    upload_file_resumable(
        &sftp,
        &PathBuf::from(local_path),
        &remote_path,
        &transfer_id,
        resume.unwrap_or(false),
        &state.transfers,
    )
    .await
    .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_download_local(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    local_path: String,
    transfer_id: String,
    resume: Option<bool>,
) -> Result<(), String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    download_file_resumable(
        &sftp,
        &remote_path,
        &PathBuf::from(local_path),
        &transfer_id,
        resume.unwrap_or(false),
        &state.transfers,
    )
    .await
    .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_upload_bytes_begin(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    transfer_id: Option<String>,
    total_size: Option<u64>,
) -> Result<UploadBytesBeginResponse, String> {
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    if let Some((parent, _)) = remote_path.rsplit_once('/') {
        if !parent.is_empty() {
            let _ = mkdir_remote_recursive(&sftp, parent).await;
        }
    }
    let tid = transfer_id
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| format!("upload-{}", new_id()));
    let _ = state
        .transfers
        .begin(&tid, total_size.unwrap_or(0), false);

    let file = match sftp
        .open_with_flags(
            &remote_path,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
        )
        .await
    {
        Ok(f) => f,
        Err(err) => {
            state.transfers.finish(&tid, Some(err.to_string()));
            return Err(format!("open remote {remote_path}: {err}"));
        }
    };

    let upload_id = new_id();
    state.sftp.uploads.lock().await.insert(
        upload_id.clone(),
        Arc::new(Mutex::new(ActiveByteUpload {
            file,
            transfer_id: tid,
        })),
    );
    Ok(UploadBytesBeginResponse { upload_id })
}

#[tauri::command]
pub async fn sftp_upload_bytes_chunk(
    state: State<'_, AppState>,
    upload_id: String,
    chunk_b64: String,
) -> Result<(), String> {
    let bytes = BASE64
        .decode(chunk_b64.as_bytes())
        .map_err(|e| format!("invalid base64 chunk: {e}"))?;
    let handle = state
        .sftp
        .uploads
        .lock()
        .await
        .get(&upload_id)
        .cloned()
        .ok_or_else(|| format!("upload handle not found: {upload_id}"))?;

    let mut guard = handle.lock().await;
    if let Err(err) = state.transfers.wait_if_paused(&guard.transfer_id).await {
        state
            .transfers
            .finish(&guard.transfer_id, Some(err.to_string()));
        return Err(err.to_string());
    }
    if let Err(err) = guard.file.write_all(&bytes).await {
        state
            .transfers
            .finish(&guard.transfer_id, Some(err.to_string()));
        return Err(format!("write remote chunk: {err}"));
    }
    state
        .transfers
        .add_loaded(&guard.transfer_id, bytes.len() as u64);
    Ok(())
}

#[tauri::command]
pub async fn sftp_upload_bytes_finish(
    state: State<'_, AppState>,
    upload_id: String,
    abort: Option<bool>,
) -> Result<(), String> {
    let handle = state
        .sftp
        .uploads
        .lock()
        .await
        .remove(&upload_id)
        .ok_or_else(|| format!("upload handle not found: {upload_id}"))?;

    let mut guard = handle.lock().await;
    if abort.unwrap_or(false) {
        state
            .transfers
            .finish(&guard.transfer_id, Some("upload aborted".into()));
        return Ok(());
    }
    if let Err(err) = guard.file.shutdown().await {
        let msg = err.to_string();
        state.transfers.finish(&guard.transfer_id, Some(msg.clone()));
        return Err(format!("finalize remote file: {msg}"));
    }
    state.transfers.finish(&guard.transfer_id, None);
    Ok(())
}

#[tauri::command]
pub async fn sftp_download_bytes(
    state: State<'_, AppState>,
    profile_id: String,
    remote_path: String,
    max_bytes: Option<u64>,
) -> Result<DownloadBytesResponse, String> {
    let limit = max_bytes.unwrap_or(DEFAULT_DOWNLOAD_BYTES_LIMIT);
    let (_, sftp) = state.sftp.get_entry(&profile_id).await?;
    let meta = sftp
        .metadata(&remote_path)
        .await
        .map_err(|e| format!("stat {remote_path}: {e}"))?;
    if meta.is_dir() {
        return Err("cannot download a directory directly as bytes".into());
    }
    let expected_size = meta.size.unwrap_or(0);
    if expected_size > limit {
        return Err(format!(
            "remote file size ({expected_size} bytes) exceeds browser download limit ({limit} bytes); please transfer into a local folder instead"
        ));
    }

    let mut file = sftp
        .open(&remote_path)
        .await
        .map_err(|e| format!("open {remote_path}: {e}"))?;
    let mut data = Vec::with_capacity(expected_size.min(4 * 1024 * 1024) as usize);
    let mut buf = vec![0u8; DOWNLOAD_CHUNK_SIZE];
    loop {
        let n = file
            .read(&mut buf)
            .await
            .map_err(|e| format!("read {remote_path}: {e}"))?;
        if n == 0 {
            break;
        }
        if (data.len() as u64).saturating_add(n as u64) > limit {
            return Err(format!(
                "remote file exceeded browser download limit ({limit} bytes)"
            ));
        }
        data.extend_from_slice(&buf[..n]);
    }

    let name = remote_path
        .rsplit('/')
        .next()
        .filter(|s| !s.is_empty())
        .unwrap_or("download")
        .to_string();
    let size = data.len() as u64;
    Ok(DownloadBytesResponse {
        name,
        data_b64: BASE64.encode(&data),
        size,
    })
}

#[tauri::command]
pub async fn sftp_copy_remote(
    state: State<'_, AppState>,
    src_profile_id: String,
    src_path: String,
    dst_profile_id: String,
    dst_path: String,
    transfer_id: String,
) -> Result<(), String> {
    let (_, src_sftp) = state.sftp.get_entry(&src_profile_id).await?;
    let (_, dst_sftp) = state.sftp.get_entry(&dst_profile_id).await?;
    copy_remote_to_remote(
        &src_sftp,
        &src_path,
        &dst_sftp,
        &dst_path,
        &transfer_id,
        &state.transfers,
    )
    .await
    .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_archive_tools(
    state: State<'_, AppState>,
    profile_id: String,
) -> Result<ArchiveTools, String> {
    let (conn, _) = state.sftp.get_entry(&profile_id).await?;
    detect_remote_tools(&conn).await.map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_compress(
    state: State<'_, AppState>,
    profile_id: String,
    cwd: String,
    format: String,
    archive_name: String,
    entries: Vec<String>,
) -> Result<(), String> {
    let (conn, _) = state.sftp.get_entry(&profile_id).await?;
    let step = build_compress_step(&format, &archive_name, &entries).map_err(|e| e.to_string())?;
    run_remote_step(&conn, &cwd, &step)
        .await
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn sftp_extract(
    state: State<'_, AppState>,
    profile_id: String,
    cwd: String,
    archive_name: String,
    dest_dir: String,
) -> Result<(), String> {
    let (conn, _) = state.sftp.get_entry(&profile_id).await?;
    let step = build_extract_step(&archive_name, &dest_dir).map_err(|e| e.to_string())?;
    run_remote_step(&conn, &cwd, &step)
        .await
        .map_err(|e| e.to_string())
}
