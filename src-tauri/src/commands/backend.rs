use core_config::{load_ui_settings_file, save_ui_settings_file, Profile, UISettings};
use serde::Serialize;
use tauri::State;

use crate::commands::APP_VERSION;
use crate::state::AppState;

#[derive(Debug, Clone, Serialize)]
pub struct BackendInfoResponse {
    pub version: String,
    pub config_dir: String,
    pub profiles_path: String,
    pub settings_path: String,
    pub sftp_cache_dir: String,
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
    BackendInfoResponse {
        version: APP_VERSION.to_string(),
        config_dir: state.config_dir.display().to_string(),
        profiles_path: state.profiles.path.display().to_string(),
        settings_path: state.settings_path.display().to_string(),
        sftp_cache_dir: state.sftp_cache_dir.display().to_string(),
    }
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
