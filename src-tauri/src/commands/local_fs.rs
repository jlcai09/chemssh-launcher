use std::path::PathBuf;

use core_transfer::{
    build_compress_step, build_extract_step, detect_local_tools, list_local_dir, local_copy,
    local_create_file, local_delete, local_home, local_mkdir, local_rename, next_archive_name,
    run_local_step, ArchiveTools, FsEntry,
};
use tauri::State;

use crate::commands::backend::{open_path_in_system, open_text_file_in_system};
use crate::state::AppState;

#[tauri::command]
pub fn get_local_home() -> Result<String, String> {
    local_home()
        .map(|p| p.display().to_string())
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub fn local_list(path: String) -> Result<Vec<FsEntry>, String> {
    list_local_dir(&PathBuf::from(path)).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn local_mkdir_cmd(path: String) -> Result<(), String> {
    local_mkdir(&PathBuf::from(path)).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn local_create_file_cmd(path: String) -> Result<(), String> {
    local_create_file(&PathBuf::from(path)).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn local_rename_cmd(from: String, to: String) -> Result<(), String> {
    local_rename(&PathBuf::from(from), &PathBuf::from(to)).map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn local_delete_cmd(path: String) -> Result<(), String> {
    tokio::task::spawn_blocking(move || {
        let target = PathBuf::from(path);
        #[cfg(target_os = "windows")]
        {
            if win_native::move_to_trash(&target).is_ok() {
                return Ok(());
            }
        }
        local_delete(&target).map_err(|e| e.to_string())
    })
    .await
    .map_err(|e| format!("local delete task join error: {e}"))?
}

#[tauri::command]
pub async fn local_copy_cmd(
    state: State<'_, AppState>,
    src: String,
    dst: String,
    transfer_id: Option<String>,
) -> Result<(), String> {
    let transfers = std::sync::Arc::clone(&state.transfers);
    tokio::task::spawn_blocking(move || {
        let src_path = PathBuf::from(&src);
        let dst_path = PathBuf::from(&dst);
        let tid = transfer_id.unwrap_or_default();
        let total = std::fs::metadata(&src_path).map(|m| m.len()).unwrap_or(0);
        if !tid.is_empty() {
            let _ = transfers.begin(&tid, total, false);
        }
        match local_copy(&src_path, &dst_path) {
            Ok(()) => {
                if !tid.is_empty() {
                    transfers.finish(&tid, None);
                }
                Ok(())
            }
            Err(err) => {
                if !tid.is_empty() {
                    transfers.finish(&tid, Some(err.to_string()));
                }
                Err(err.to_string())
            }
        }
    })
    .await
    .map_err(|e| format!("local copy task join error: {e}"))?
}

#[tauri::command]
pub fn local_open_cmd(path: String) -> Result<(), String> {
    open_path_in_system(&PathBuf::from(path))
}

#[tauri::command]
pub fn local_open_text_cmd(path: String) -> Result<(), String> {
    open_text_file_in_system(&PathBuf::from(path))
}

#[tauri::command]
pub async fn local_archive_tools() -> ArchiveTools {
    detect_local_tools().await
}

#[tauri::command]
pub async fn local_compress(
    cwd: String,
    format: String,
    archive_name: Option<String>,
    entries: Vec<String>,
) -> Result<String, String> {
    let cwd_path = PathBuf::from(&cwd);
    let final_name = next_archive_name(&format, &entries, archive_name.as_deref(), |candidate| {
        cwd_path.join(candidate).symlink_metadata().is_ok()
    })
    .map_err(|e| e.to_string())?;
    let step = build_compress_step(&format, &final_name, &entries).map_err(|e| e.to_string())?;
    run_local_step(&cwd_path, &step)
        .await
        .map_err(|e| e.to_string())?;
    Ok(final_name)
}

#[tauri::command]
pub async fn local_extract(
    cwd: String,
    archive_name: String,
    dest_dir: String,
) -> Result<(), String> {
    let step = build_extract_step(&archive_name, &dest_dir).map_err(|e| e.to_string())?;
    run_local_step(&PathBuf::from(cwd), &step)
        .await
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn local_compress_appends_new_suffix_instead_of_overwriting() {
        let dir = tempfile::tempdir().unwrap();
        let cwd = dir.path().display().to_string();
        std::fs::write(dir.path().join("a.txt"), b"alpha").unwrap();
        std::fs::write(dir.path().join("b.txt"), b"beta").unwrap();

        let entries = vec!["a.txt".to_string(), "b.txt".to_string()];
        let first = local_compress(cwd.clone(), "tar".into(), None, entries.clone())
            .await
            .unwrap();
        assert_eq!(first, "archive.tar");
        let original_bytes = std::fs::read(dir.path().join(&first)).unwrap();

        // Modify a.txt before creating the second archive; the first archive must remain intact.
        std::fs::write(dir.path().join("a.txt"), b"alpha-v2").unwrap();
        let second = local_compress(cwd.clone(), "tar".into(), None, entries.clone())
            .await
            .unwrap();
        assert_eq!(second, "archive_new.tar");
        assert_eq!(
            std::fs::read(dir.path().join(&first)).unwrap(),
            original_bytes,
            "existing archive.tar must never be overwritten"
        );

        let third = local_compress(
            cwd,
            "tar".into(),
            Some("archive.tar".into()),
            entries,
        )
        .await
        .unwrap();
        assert_eq!(third, "archive_new_new.tar");
        assert!(dir.path().join(&third).is_file());
    }
}
