use std::path::Path;
use std::process::Command;

use core_config::{load_ui_settings_file, save_ui_settings_file, Profile, UISettings};
use serde::Serialize;
use tauri::State;

use crate::commands::APP_VERSION;
use crate::state::AppState;

#[derive(Debug, Clone, Serialize)]
pub struct CacheEntryInfo {
    pub kind: String,
    pub path: String,
    pub exists: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct BackendInfoResponse {
    pub version: String,
    pub config_dir: String,
    pub profiles_path: String,
    pub settings_path: String,
    pub sftp_cache_dir: String,
    pub sftp_open_cache_dir: String,
    pub clear_pending: bool,
    pub cache_entries: Vec<CacheEntryInfo>,
}

fn build_backend_info(state: &AppState) -> BackendInfoResponse {
    let sftp_cache = state.sftp_cache_dir.display().to_string();
    let exists = state.sftp_cache_dir.exists();
    BackendInfoResponse {
        version: APP_VERSION.to_string(),
        config_dir: state.config_dir.display().to_string(),
        profiles_path: state.profiles.path.display().to_string(),
        settings_path: state.settings_path.display().to_string(),
        sftp_cache_dir: sftp_cache.clone(),
        sftp_open_cache_dir: sftp_cache.clone(),
        clear_pending: false,
        cache_entries: vec![CacheEntryInfo {
            kind: "sftp-open".into(),
            path: sftp_cache,
            exists,
        }],
    }
}

pub(crate) fn open_path_in_system(path: &Path) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        if !path.exists() {
            let _ = std::fs::create_dir_all(if path.extension().is_none() {
                path
            } else {
                parent
            });
        }
    }
    #[cfg(target_os = "windows")]
    let status = {
        use std::os::windows::process::CommandExt;
        Command::new("explorer")
            .arg(path)
            .creation_flags(0x0800_0000)
            .spawn()
    };
    #[cfg(target_os = "macos")]
    let status = Command::new("open").arg(path).spawn();
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    let status = Command::new("xdg-open").arg(path).spawn();

    status.map(|_| ()).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn get_version() -> String {
    APP_VERSION.to_string()
}

#[tauri::command]
pub fn get_ui_settings(state: State<'_, AppState>) -> Result<UISettings, String> {
    load_ui_settings_file(&state.settings_path).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn save_ui_settings(
    state: State<'_, AppState>,
    settings: UISettings,
) -> Result<UISettings, String> {
    save_ui_settings_file(&state.settings_path, &settings).map_err(|e| e.to_string())?;
    load_ui_settings_file(&state.settings_path).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn get_backend_info(state: State<'_, AppState>) -> BackendInfoResponse {
    build_backend_info(&state)
}

#[tauri::command]
pub fn open_config_dir(state: State<'_, AppState>) -> Result<(), String> {
    let _ = std::fs::create_dir_all(&state.config_dir);
    open_path_in_system(&state.config_dir)
}

#[tauri::command]
pub fn open_sftp_cache_dir(state: State<'_, AppState>) -> Result<(), String> {
    let _ = std::fs::create_dir_all(&state.sftp_cache_dir);
    open_path_in_system(&state.sftp_cache_dir)
}

#[derive(Debug, Clone, Serialize)]
pub struct ClearCacheResponse {
    pub info: BackendInfoResponse,
    pub messages: Vec<String>,
}

#[tauri::command]
pub fn clear_cache(state: State<'_, AppState>) -> Result<ClearCacheResponse, String> {
    let mut messages = Vec::new();
    if state.sftp_cache_dir.exists() {
        std::fs::remove_dir_all(&state.sftp_cache_dir).map_err(|e| e.to_string())?;
        messages.push(format!("Removed {}", state.sftp_cache_dir.display()));
    }
    Ok(ClearCacheResponse {
        info: build_backend_info(&state),
        messages,
    })
}

#[tauri::command]
pub fn export_profiles(state: State<'_, AppState>) -> Result<String, String> {
    state.profiles.export_json().map_err(|e| e.to_string())
}

#[tauri::command]
pub fn import_profiles(
    state: State<'_, AppState>,
    raw_json: String,
) -> Result<Vec<Profile>, String> {
    state
        .profiles
        .import_json(&raw_json)
        .map_err(|e| e.to_string())
}
