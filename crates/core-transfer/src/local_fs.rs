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
    pub mod_time: String,
    pub mode: String,
    pub is_symlink: bool,
    pub link_target: String,
    pub link_broken: bool,
}

pub fn format_iso_utc(ms: i64) -> String {
    if ms <= 0 {
        return String::new();
    }
    let secs = (ms / 1000).max(0) as u64;
    let days = (secs / 86400) as i64;
    let rem = secs % 86400;
    let hour = rem / 3600;
    let min = (rem % 3600) / 60;
    let sec = rem % 60;

    // Civil date from days since 1970-01-01 (Howard Hinnant's algorithm)
    let z = days + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = (z - era * 146097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = if m <= 2 { y + 1 } else { y };

    format!("{year:04}-{m:02}-{d:02}T{hour:02}:{min:02}:{sec:02}Z")
}

pub fn format_unix_mode(is_dir: bool, perm: Option<u32>) -> String {
    let p = perm.unwrap_or(if is_dir { 0o755 } else { 0o644 });
    let mut s = String::with_capacity(10);
    s.push(if is_dir { 'd' } else { '-' });
    for shift in [6, 3, 0] {
        let bits = (p >> shift) & 0b111;
        s.push(if bits & 4 != 0 { 'r' } else { '-' });
        s.push(if bits & 2 != 0 { 'w' } else { '-' });
        s.push(if bits & 1 != 0 { 'x' } else { '-' });
    }
    s
}

pub fn local_home() -> Result<PathBuf, TransferError> {
    let home = dirs::home_dir()
        .ok_or_else(|| TransferError::InvalidInput("cannot resolve home directory".into()))?;
    let desktop = home.join("Desktop");
    if desktop.is_dir() {
        Ok(desktop)
    } else {
        Ok(home)
    }
}

pub fn list_local_dir(path: &Path) -> Result<Vec<FsEntry>, TransferError> {
    let mut entries = Vec::new();
    for item in fs::read_dir(path)? {
        let item = item?;
        let item_path = item.path();
        let lmeta = match fs::symlink_metadata(&item_path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        let is_symlink = lmeta.file_type().is_symlink();
        let link_target = if is_symlink {
            fs::read_link(&item_path)
                .map(|p| p.display().to_string())
                .unwrap_or_default()
        } else {
            String::new()
        };

        let (is_dir, effective_meta, link_broken) = if is_symlink {
            match fs::metadata(&item_path) {
                Ok(target_meta) => (target_meta.is_dir(), target_meta, false),
                Err(_) => (false, lmeta, true),
            }
        } else {
            (lmeta.is_dir(), lmeta, false)
        };

        let name = item.file_name().to_string_lossy().into_owned();
        let mod_time_ms = effective_meta
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map(|d| d.as_millis() as i64)
            .unwrap_or(0);
        #[cfg(unix)]
        let perm = {
            use std::os::unix::fs::PermissionsExt;
            Some(effective_meta.permissions().mode())
        };
        #[cfg(not(unix))]
        let perm = None;

        entries.push(FsEntry {
            name,
            path: item_path.display().to_string(),
            is_dir,
            size: if is_dir { 0 } else { effective_meta.len() },
            mod_time_ms,
            mod_time: format_iso_utc(mod_time_ms),
            mode: format_unix_mode(is_dir, perm),
            is_symlink,
            link_target,
            link_broken,
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
    fs::OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(path)?;
    Ok(())
}

pub fn local_rename(from: &Path, to: &Path) -> Result<(), TransferError> {
    fs::rename(from, to)?;
    Ok(())
}

pub fn local_delete(path: &Path) -> Result<(), TransferError> {
    let meta = fs::symlink_metadata(path)?;
    if meta.file_type().is_symlink() {
        fs::remove_file(path).or_else(|_| fs::remove_dir(path))?;
    } else if meta.is_dir() {
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

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    #[test]
    fn list_local_dir_resolves_file_dir_and_broken_symlinks() {
        let dir = tempfile::tempdir().unwrap();
        let target_dir = dir.path().join("real_dir");
        fs::create_dir_all(&target_dir).unwrap();
        let target_file = dir.path().join("real_file.txt");
        fs::write(&target_file, b"hello").unwrap();

        symlink(&target_dir, dir.path().join("link_to_dir")).unwrap();
        symlink(&target_file, dir.path().join("link_to_file")).unwrap();
        symlink(dir.path().join("does_not_exist"), dir.path().join("broken_link")).unwrap();

        let entries = list_local_dir(dir.path()).unwrap();
        let dir_link = entries.iter().find(|e| e.name == "link_to_dir").unwrap();
        assert!(dir_link.is_symlink);
        assert!(dir_link.is_dir);
        assert!(!dir_link.link_broken);
        assert!(!dir_link.link_target.is_empty());

        let file_link = entries.iter().find(|e| e.name == "link_to_file").unwrap();
        assert!(file_link.is_symlink);
        assert!(!file_link.is_dir);
        assert!(!file_link.link_broken);
        assert_eq!(file_link.size, 5);

        let broken = entries.iter().find(|e| e.name == "broken_link").unwrap();
        assert!(broken.is_symlink);
        assert!(!broken.is_dir);
        assert!(broken.link_broken);

        // Deleting a directory symlink removes only the link, leaving target_dir intact.
        local_delete(&dir.path().join("link_to_dir")).unwrap();
        assert!(target_dir.exists());
    }
}
