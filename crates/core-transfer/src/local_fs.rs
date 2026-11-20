use std::fs;
use std::path::{Path, PathBuf};
use std::time::UNIX_EPOCH;

use serde::Serialize;

use crate::TransferError;

#[derive(Debug, Clone, Serialize)]
pub struct FsEntry {
    pub name: String,
    pub path: String,
    pub is_dir: bool,
    pub size: u64,
    pub mod_time_ms: i64,
}

pub fn local_home() -> Result<PathBuf, TransferError> {
    dirs::home_dir().ok_or_else(|| TransferError::InvalidInput("cannot resolve home directory".into()))
}

pub fn list_local_dir(path: &Path) -> Result<Vec<FsEntry>, TransferError> {
    let mut entries = Vec::new();
    for item in fs::read_dir(path)? {
        let item = item?;
        let meta = item.metadata()?;
        let name = item.file_name().to_string_lossy().into_owned();
        let mod_time_ms = meta
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map(|d| d.as_millis() as i64)
            .unwrap_or(0);
        entries.push(FsEntry {
            name,
            path: item.path().display().to_string(),
            is_dir: meta.is_dir(),
            size: if meta.is_dir() { 0 } else { meta.len() },
            mod_time_ms,
        });
    }
    entries.sort_by(|a, b| {
        b.is_dir
            .cmp(&a.is_dir)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
    Ok(entries)
}

pub fn local_mkdir(path: &Path) -> Result<(), TransferError> {
    fs::create_dir_all(path)?;
    Ok(())
}

pub fn local_create_file(path: &Path) -> Result<(), TransferError> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::OpenOptions::new().create_new(true).write(true).open(path)?;
    Ok(())
}

pub fn local_rename(from: &Path, to: &Path) -> Result<(), TransferError> {
    fs::rename(from, to)?;
    Ok(())
}

pub fn local_delete(path: &Path) -> Result<(), TransferError> {
    let meta = fs::symlink_metadata(path)?;
    if meta.is_dir() {
        fs::remove_dir_all(path)?;
    } else {
        fs::remove_file(path)?;
    }
    Ok(())
}

pub fn local_copy(src: &Path, dst: &Path) -> Result<(), TransferError> {
    let meta = fs::metadata(src)?;
    if meta.is_dir() {
        copy_dir_recursive(src, dst)
    } else {
        if let Some(parent) = dst.parent() {
            fs::create_dir_all(parent)?;
        }
        fs::copy(src, dst)?;
        Ok(())
    }
}

fn copy_dir_recursive(src: &Path, dst: &Path) -> Result<(), TransferError> {
    fs::create_dir_all(dst)?;
    for entry in fs::read_dir(src)? {
        let entry = entry?;
        let target = dst.join(entry.file_name());
        if entry.file_type()?.is_dir() {
            copy_dir_recursive(&entry.path(), &target)?;
        } else {
            fs::copy(entry.path(), target)?;
        }
    }
    Ok(())
}
