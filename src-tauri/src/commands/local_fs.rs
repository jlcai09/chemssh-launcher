use std::path::PathBuf;

use core_transfer::{
    build_compress_step, build_extract_step, detect_local_tools, list_local_dir, local_copy,
    local_create_file, local_delete, local_home, local_mkdir, local_rename, run_local_step,
    ArchiveTools, FsEntry,
};

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
pub fn local_delete_cmd(path: String) -> Result<(), String> {
    local_delete(&PathBuf::from(path)).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn local_copy_cmd(src: String, dst: String) -> Result<(), String> {
    local_copy(&PathBuf::from(src), &PathBuf::from(dst)).map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn local_archive_tools() -> ArchiveTools {
    detect_local_tools().await
}

#[tauri::command]
pub async fn local_compress(
    cwd: String,
    format: String,
    archive_name: String,
    entries: Vec<String>,
) -> Result<(), String> {
    let step = build_compress_step(&format, &archive_name, &entries).map_err(|e| e.to_string())?;
    run_local_step(&PathBuf::from(cwd), &step)
        .await
        .map_err(|e| e.to_string())
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
