use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use core_config::ProfileStore;
use core_ssh::{HostKeyPolicy, KnownHostsVerifier, SshConnection};
use core_transfer::{
    build_compress_step, build_extract_step, copy_remote_to_remote, detect_remote_tools,
    download_file_resumable, list_remote_dir, run_remote_step, upload_file_resumable, ArchiveTools,
    FsEntry,
};
use russh_sftp::client::SftpSession;
use russh_sftp::protocol::OpenFlags;
use serde::Serialize;
use tauri::State;
use tokio::sync::Mutex;

use crate::commands::sessions::{format_ssh_error, load_ssh_credentials};
use crate::state::AppState;

#[derive(Debug, Clone, Serialize)]
pub struct SftpConnectResponse {
    pub id: String,
    pub path: String,
}

struct SftpEntry {
    connection: Arc<SshConnection>,
    sftp: Arc<SftpSession>,
}

#[derive(Default)]
pub struct SftpManager {
    clients: Mutex<HashMap<String, SftpEntry>>,
}

impl SftpManager {
    async fn get_entry(
        &self,
        profile_id: &str,
    ) -> Result<(Arc<SshConnection>, Arc<SftpSession>), String> {
        let guard = self.clients.lock().await;
        let entry = guard
            .get(profile_id)
            .ok_or_else(|| format!("sftp session not connected for profile: {profile_id}"))?;
        Ok((Arc::clone(&entry.connection), Arc::clone(&entry.sftp)))
    }
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
    if profile.is_local() {
        return Err("local profiles do not support SFTP".into());
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
    let sftp = Arc::new(connection.open_sftp().await.map_err(|e| e.to_string())?);
    let home = sftp
        .canonicalize(".")
        .await
        .unwrap_or_else(|_| "/".to_string());

    let id = profile.id.clone();
    state.sftp.clients.lock().await.insert(
        id.clone(),
        SftpEntry {
            connection,
            sftp,
        },
    );
    Ok(SftpConnectResponse { id, path: home })
}

#[tauri::command]
pub async fn sftp_disconnect(state: State<'_, AppState>, profile_id: String) -> Result<(), String> {
    let removed = state.sftp.clients.lock().await.remove(&profile_id);
    if let Some(entry) = removed {
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
    let (conn, sftp) = state.sftp.get_entry(&profile_id).await?;
    let dir_flag = match is_dir {
        Some(v) => v,
        None => sftp
            .metadata(&path)
            .await
            .map(|m| m.is_dir())
            .unwrap_or(false),
    };
    if dir_flag {
        if sftp.remove_dir(&path).await.is_err() {
            let cmd = format!("rm -rf -- {}", core_ssh::shell_quote(&path));
            let _ = conn.exec_command(&cmd).await.map_err(|e| e.to_string())?;
        }
        Ok(())
    } else {
        sftp.remove_file(&path).await.map_err(|e| e.to_string())
    }
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
