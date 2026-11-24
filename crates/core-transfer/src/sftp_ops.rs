use std::path::Path;

use russh_sftp::client::SftpSession;
use russh_sftp::protocol::OpenFlags;
use tokio::fs::OpenOptions;
use tokio::io::{AsyncReadExt, AsyncSeekExt, AsyncWriteExt, SeekFrom};

use crate::local_fs::{format_iso_utc, format_unix_mode, FsEntry};
use crate::progress::TransferProgressStore;
use crate::{remote_io_err, sftp_err, TransferError};

const CHUNK_SIZE: usize = 256 * 1024;

pub async fn list_remote_dir(sftp: &SftpSession, path: &str) -> Result<Vec<FsEntry>, TransferError> {
    let dir = sftp
        .read_dir(path)
        .await
        .map_err(|e| sftp_err(format!("list {path}"), e))?;
    let base = path.trim_end_matches('/');
    let mut entries = Vec::new();
    let mut symlink_indices = Vec::new();

    for entry in dir {
        let name = entry.file_name();
        if name == "." || name == ".." {
            continue;
        }
        let meta = entry.metadata();
        let is_symlink = entry.file_type().is_symlink() || meta.is_symlink();
        let is_dir = meta.is_dir();
        let full_path = if base.is_empty() {
            format!("/{name}")
        } else {
            format!("{base}/{name}")
        };
        let mod_time_ms = meta.mtime.map(|m| m as i64 * 1000).unwrap_or(0);
        if is_symlink {
            symlink_indices.push((entries.len(), full_path.clone()));
        }
        entries.push(FsEntry {
            name,
            path: full_path,
            is_dir,
            size: if is_dir { 0 } else { meta.size.unwrap_or(0) },
            mod_time_ms,
            mod_time: format_iso_utc(mod_time_ms),
            mode: format_unix_mode(is_dir, meta.permissions),
            is_symlink,
            link_target: String::new(),
            link_broken: false,
        });
    }

    if !symlink_indices.is_empty() {
        let queue = tokio::sync::Mutex::new(symlink_indices);
        let results = tokio::sync::Mutex::new(Vec::new());

        let resolve_worker = || async {
            loop {
                let next = {
                    let mut guard = queue.lock().await;
                    guard.pop()
                };
                let Some((idx, full_path)) = next else {
                    break;
                };
                let link_target = sftp.read_link(&full_path).await.unwrap_or_default();
                let target_meta = sftp.metadata(&full_path).await.ok();
                let mut guard = results.lock().await;
                guard.push((idx, link_target, target_meta));
            }
        };

        tokio::join!(
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
        );

        let resolved = results.into_inner();
        for (idx, link_target, target_meta) in resolved {
            if let Some(entry) = entries.get_mut(idx) {
                entry.link_target = link_target;
                match target_meta {
                    Some(meta) => {
                        let is_dir = meta.is_dir();
                        let mod_time_ms = meta.mtime.map(|m| m as i64 * 1000).unwrap_or(entry.mod_time_ms);
                        entry.is_dir = is_dir;
                        entry.size = if is_dir { 0 } else { meta.size.unwrap_or(entry.size) };
                        entry.mod_time_ms = mod_time_ms;
                        entry.mod_time = format_iso_utc(mod_time_ms);
                        entry.mode = format_unix_mode(is_dir, meta.permissions);
                        entry.link_broken = false;
                    }
                    None => {
                        entry.is_dir = false;
                        entry.link_broken = true;
                    }
                }
            }
        }
    }

    entries.sort_by(|a, b| {
        b.is_dir
            .cmp(&a.is_dir)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
    Ok(entries)
}

pub async fn delete_remote_path(sftp: &SftpSession, path: &str) -> Result<(), TransferError> {
    let lmeta = sftp
        .symlink_metadata(path)
        .await
        .map_err(|e| sftp_err(format!("lstat {path}"), e))?;

    if lmeta.is_symlink() || !lmeta.is_dir() {
        sftp.remove_file(path)
            .await
            .map_err(|e| sftp_err(format!("remove {path}"), e))?;
        return Ok(());
    }

    let entries = sftp
        .read_dir(path)
        .await
        .map_err(|e| sftp_err(format!("read dir {path}"), e))?;
    let base = path.trim_end_matches('/');
    for entry in entries {
        let name = entry.file_name();
        if name == "." || name == ".." {
            continue;
        }
        let child = if base.is_empty() {
            format!("/{name}")
        } else {
            format!("{base}/{name}")
        };
        if entry.file_type().is_symlink() || !entry.metadata().is_dir() {
            sftp.remove_file(&child)
                .await
                .map_err(|e| sftp_err(format!("remove {child}"), e))?;
        } else {
            Box::pin(delete_remote_path(sftp, &child)).await?;
        }
    }

    sftp.remove_dir(path)
        .await
        .map_err(|e| sftp_err(format!("rmdir {path}"), e))?;
    Ok(())
}

fn finish_or_release(store: &TransferProgressStore, transfer_id: &str, err: &TransferError) {
    if err.is_transport_error() {
        store.release_for_retry(transfer_id);
    } else {
        store.finish(transfer_id, Some(err.to_string()));
    }
}

pub async fn upload_file_resumable(
    sftp: &SftpSession,
    local_path: &Path,
    remote_path: &str,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    if let Err(err) =
        upload_file_resumable_once(sftp, local_path, remote_path, transfer_id, resume, store).await
    {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
        return Err(err);
    }
    Ok(())
}

/// Runs a single upload attempt; releases progress without marking done on recoverable transport errors.
pub async fn upload_file_resumable_once(
    sftp: &SftpSession,
    local_path: &Path,
    remote_path: &str,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let meta = tokio::fs::metadata(local_path).await?;
    let total = meta.len();
    let offset = store.begin(transfer_id, total, resume)?;

    let mut local_file = match tokio::fs::File::open(local_path).await {
        Ok(f) => f,
        Err(err) => {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
    };
    if offset > 0 {
        if let Err(err) = local_file.seek(SeekFrom::Start(offset)).await {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
    }

    let flags = if resume && offset > 0 {
        OpenFlags::WRITE | OpenFlags::CREATE
    } else {
        OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE
    };
    let mut remote_file = match sftp.open_with_flags(remote_path, flags).await {
        Ok(f) => f,
        Err(err) => {
            let te = sftp_err(format!("open remote {remote_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    };
    if offset > 0 {
        if let Err(err) = remote_file.seek(SeekFrom::Start(offset)).await {
            let te = remote_io_err(format!("seek remote {remote_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    }

    let mut buf = vec![0u8; CHUNK_SIZE];
    loop {
        if let Err(err) = store.wait_if_paused(transfer_id).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err);
        }
        let n = match local_file.read(&mut buf).await {
            Ok(0) => break,
            Ok(n) => n,
            Err(err) => {
                let te: TransferError = err.into();
                store.finish(transfer_id, Some(te.to_string()));
                return Err(te);
            }
        };
        if let Err(err) = remote_file.write_all(&buf[..n]).await {
            let te = remote_io_err(format!("write remote {remote_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
        store.add_loaded(transfer_id, n as u64);
    }

    if let Err(err) = remote_file.shutdown().await {
        let te = remote_io_err(format!("finalize remote {remote_path}"), err);
        finish_or_release(store, transfer_id, &te);
        return Err(te);
    }
    store.finish(transfer_id, None);
    Ok(())
}

pub async fn download_file_resumable(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    if let Err(err) =
        download_file_resumable_once(sftp, remote_path, local_path, transfer_id, resume, store)
            .await
    {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
        return Err(err);
    }
    Ok(())
}

/// Runs a single download attempt; releases progress without marking done on recoverable transport errors.
pub async fn download_file_resumable_once(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let meta = sftp
        .metadata(remote_path)
        .await
        .map_err(|e| sftp_err(format!("stat {remote_path}"), e))?;
    let total = meta.size.unwrap_or(0);
    let offset = store.begin(transfer_id, total, resume)?;

    if let Some(parent) = local_path.parent() {
        if let Err(err) = tokio::fs::create_dir_all(parent).await {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
    }

    let mut remote_file = match sftp.open(remote_path).await {
        Ok(f) => f,
        Err(err) => {
            let te = sftp_err(format!("open remote {remote_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    };
    if offset > 0 {
        if let Err(err) = remote_file.seek(SeekFrom::Start(offset)).await {
            let te = remote_io_err(format!("seek remote {remote_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    }

    let open_res = if resume && offset > 0 {
        OpenOptions::new()
            .create(true)
            .write(true)
            .open(local_path)
            .await
    } else {
        OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(local_path)
            .await
    };
    let mut local_file = match open_res {
        Ok(f) => f,
        Err(err) => {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
    };
    if offset > 0 {
        if let Err(err) = local_file.seek(SeekFrom::Start(offset)).await {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
    }

    let mut buf = vec![0u8; CHUNK_SIZE];
    loop {
        if let Err(err) = store.wait_if_paused(transfer_id).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err);
        }
        let n = match remote_file.read(&mut buf).await {
            Ok(0) => break,
            Ok(n) => n,
            Err(err) => {
                let te = remote_io_err(format!("read remote {remote_path}"), err);
                finish_or_release(store, transfer_id, &te);
                return Err(te);
            }
        };
        if let Err(err) = local_file.write_all(&buf[..n]).await {
            let te: TransferError = err.into();
            store.finish(transfer_id, Some(te.to_string()));
            return Err(te);
        }
        store.add_loaded(transfer_id, n as u64);
    }

    if let Err(err) = local_file.flush().await {
        let te: TransferError = err.into();
        store.finish(transfer_id, Some(te.to_string()));
        return Err(te);
    }
    store.finish(transfer_id, None);
    Ok(())
}

/// Streams bytes directly between two remote SFTP sessions without staging a full local copy.
pub async fn copy_remote_to_remote(
    src_sftp: &SftpSession,
    src_path: &str,
    dst_sftp: &SftpSession,
    dst_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    if let Err(err) =
        copy_remote_to_remote_once(src_sftp, src_path, dst_sftp, dst_path, transfer_id, store).await
    {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
        return Err(err);
    }
    Ok(())
}

/// Runs a single remote-to-remote copy attempt; releases progress without marking done on transport errors.
pub async fn copy_remote_to_remote_once(
    src_sftp: &SftpSession,
    src_path: &str,
    dst_sftp: &SftpSession,
    dst_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let meta = src_sftp
        .metadata(src_path)
        .await
        .map_err(|e| sftp_err(format!("stat source {src_path}"), e))?;
    let total = meta.size.unwrap_or(0);
    let _ = store.begin(transfer_id, total, false)?;

    let mut src_file = match src_sftp.open(src_path).await {
        Ok(f) => f,
        Err(err) => {
            let te = sftp_err(format!("open source {src_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    };
    let mut dst_file = match dst_sftp
        .open_with_flags(
            dst_path,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
        )
        .await
    {
        Ok(f) => f,
        Err(err) => {
            let te = sftp_err(format!("open dest {dst_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
    };

    let mut buf = vec![0u8; CHUNK_SIZE];
    loop {
        if let Err(err) = store.wait_if_paused(transfer_id).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err);
        }
        let n = match src_file.read(&mut buf).await {
            Ok(0) => break,
            Ok(n) => n,
            Err(err) => {
                let te = remote_io_err(format!("read source {src_path}"), err);
                finish_or_release(store, transfer_id, &te);
                return Err(te);
            }
        };
        if let Err(err) = dst_file.write_all(&buf[..n]).await {
            let te = remote_io_err(format!("write dest {dst_path}"), err);
            finish_or_release(store, transfer_id, &te);
            return Err(te);
        }
        store.add_loaded(transfer_id, n as u64);
    }

    if let Err(err) = dst_file.shutdown().await {
        let te = remote_io_err(format!("finalize dest {dst_path}"), err);
        finish_or_release(store, transfer_id, &te);
        return Err(te);
    }
    store.finish(transfer_id, None);
    Ok(())
}
