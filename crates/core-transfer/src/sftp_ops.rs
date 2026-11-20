use std::path::Path;

use russh_sftp::client::SftpSession;
use russh_sftp::protocol::OpenFlags;
use tokio::fs::OpenOptions;
use tokio::io::{AsyncReadExt, AsyncSeekExt, AsyncWriteExt, SeekFrom};

use crate::local_fs::FsEntry;
use crate::progress::TransferProgressStore;
use crate::TransferError;

const CHUNK_SIZE: usize = 256 * 1024;

pub async fn list_remote_dir(sftp: &SftpSession, path: &str) -> Result<Vec<FsEntry>, TransferError> {
    let dir = sftp
        .read_dir(path)
        .await
        .map_err(|e| TransferError::Sftp(format!("list {path}: {e}")))?;
    let base = path.trim_end_matches('/');
    let mut entries = Vec::new();
    for entry in dir {
        let name = entry.file_name();
        if name == "." || name == ".." {
            continue;
        }
        let meta = entry.metadata();
        let is_dir = meta.is_dir();
        let full_path = if base.is_empty() {
            format!("/{name}")
        } else {
            format!("{base}/{name}")
        };
        entries.push(FsEntry {
            name,
            path: full_path,
            is_dir,
            size: if is_dir { 0 } else { meta.size.unwrap_or(0) },
            mod_time_ms: meta.mtime.map(|m| m as i64 * 1000).unwrap_or(0),
        });
    }
    entries.sort_by(|a, b| {
        b.is_dir
            .cmp(&a.is_dir)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
    Ok(entries)
}

pub async fn upload_file_resumable(
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

    let mut local_file = tokio::fs::File::open(local_path).await?;
    if offset > 0 {
        local_file.seek(SeekFrom::Start(offset)).await?;
    }

    let flags = if resume && offset > 0 {
        OpenFlags::WRITE | OpenFlags::CREATE
    } else {
        OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE
    };
    let mut remote_file = sftp
        .open_with_flags(remote_path, flags)
        .await
        .map_err(|e| {
            store.finish(transfer_id, Some(e.to_string()));
            TransferError::Sftp(format!("open remote {remote_path}: {e}"))
        })?;
    if offset > 0 {
        remote_file.seek(SeekFrom::Start(offset)).await?;
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
                store.finish(transfer_id, Some(err.to_string()));
                return Err(err.into());
            }
        };
        if let Err(err) = remote_file.write_all(&buf[..n]).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err.into());
        }
        store.add_loaded(transfer_id, n as u64);
    }

    if let Err(err) = remote_file.shutdown().await {
        store.finish(transfer_id, Some(err.to_string()));
        return Err(err.into());
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
    let meta = sftp
        .metadata(remote_path)
        .await
        .map_err(|e| TransferError::Sftp(format!("stat {remote_path}: {e}")))?;
    let total = meta.size.unwrap_or(0);
    let offset = store.begin(transfer_id, total, resume)?;

    if let Some(parent) = local_path.parent() {
        tokio::fs::create_dir_all(parent).await?;
    }

    let mut remote_file = sftp
        .open(remote_path)
        .await
        .map_err(|e| {
            store.finish(transfer_id, Some(e.to_string()));
            TransferError::Sftp(format!("open remote {remote_path}: {e}"))
        })?;
    if offset > 0 {
        remote_file.seek(SeekFrom::Start(offset)).await?;
    }

    let mut local_file = if resume && offset > 0 {
        OpenOptions::new()
            .create(true)
            .write(true)
            .open(local_path)
            .await?
    } else {
        OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(local_path)
            .await?
    };
    if offset > 0 {
        local_file.seek(SeekFrom::Start(offset)).await?;
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
                store.finish(transfer_id, Some(err.to_string()));
                return Err(err.into());
            }
        };
        if let Err(err) = local_file.write_all(&buf[..n]).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err.into());
        }
        store.add_loaded(transfer_id, n as u64);
    }

    local_file.flush().await?;
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
    let meta = src_sftp
        .metadata(src_path)
        .await
        .map_err(|e| TransferError::Sftp(format!("stat source {src_path}: {e}")))?;
    let total = meta.size.unwrap_or(0);
    let _ = store.begin(transfer_id, total, false)?;

    let mut src_file = src_sftp
        .open(src_path)
        .await
        .map_err(|e| {
            store.finish(transfer_id, Some(e.to_string()));
            TransferError::Sftp(format!("open source {src_path}: {e}"))
        })?;
    let mut dst_file = dst_sftp
        .open_with_flags(
            dst_path,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
        )
        .await
        .map_err(|e| {
            store.finish(transfer_id, Some(e.to_string()));
            TransferError::Sftp(format!("open dest {dst_path}: {e}"))
        })?;

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
                store.finish(transfer_id, Some(err.to_string()));
                return Err(err.into());
            }
        };
        if let Err(err) = dst_file.write_all(&buf[..n]).await {
            store.finish(transfer_id, Some(err.to_string()));
            return Err(err.into());
        }
        store.add_loaded(transfer_id, n as u64);
    }

    dst_file.shutdown().await?;
    store.finish(transfer_id, None);
    Ok(())
}
