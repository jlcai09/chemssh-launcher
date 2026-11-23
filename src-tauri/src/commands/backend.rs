use std::path::Path;
use std::process::Command;

use core_config::{load_ui_settings_file, save_ui_settings_file, Profile, UISettings};
use serde::Serialize;
#[cfg(target_os = "windows")]
use tauri::Manager;
use tauri::{AppHandle, State};

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
        if path.is_dir() {
            Command::new("explorer")
                .arg(path)
                .creation_flags(0x0800_0000)
                .spawn()
        } else {
            Command::new("rundll32.exe")
                .arg("url.dll,FileProtocolHandler")
                .arg(path)
                .creation_flags(0x0800_0000)
                .spawn()
        }
    };
    #[cfg(target_os = "macos")]
    let status = Command::new("open").arg(path).spawn();
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    let status = Command::new("xdg-open").arg(path).spawn();

    status.map(|_| ()).map_err(|e| e.to_string())
}

pub(crate) fn open_text_file_in_system(path: &Path) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    {
        use std::os::windows::process::CommandExt;
        return Command::new("notepad.exe")
            .arg(path)
            .creation_flags(0x0800_0000)
            .spawn()
            .map(|_| ())
            .map_err(|e| e.to_string());
    }
    #[cfg(target_os = "macos")]
    {
        if Command::new("open").args(["-t"]).arg(path).spawn().is_ok() {
            return Ok(());
        }
        return Command::new("open")
            .arg(path)
            .spawn()
            .map(|_| ())
            .map_err(|e| e.to_string());
    }
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    {
        Command::new("xdg-open")
            .arg(path)
            .spawn()
            .map(|_| ())
            .map_err(|e| e.to_string())
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct LauncherLogsResponse {
    pub lines: Vec<String>,
}

#[tauri::command]
pub fn get_launcher_logs(state: State<'_, AppState>) -> LauncherLogsResponse {
    LauncherLogsResponse {
        lines: state.launcher_logs.snapshot(),
    }
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

fn downloads_folder() -> Option<std::path::PathBuf> {
    let home = std::env::var_os("USERPROFILE").or_else(|| std::env::var_os("HOME"))?;
    Some(std::path::PathBuf::from(home).join("Downloads"))
}

fn open_downloads_folder() -> Result<String, String> {
    let dir = downloads_folder().ok_or_else(|| "downloads folder is unknown".to_string())?;
    open_path_in_system(&dir)?;
    Ok("folder".into())
}

/// Drives the WebView2 download list: `Some(true)` opens or toggles it,
/// `Some(false)` closes it. Returns false when the native dialog is unavailable.
#[cfg(target_os = "windows")]
async fn webview_download_dialog(app: &AppHandle, open: bool) -> bool {
    let Some(window) = app
        .get_webview_window("main")
        .or_else(|| app.webview_windows().into_values().next())
    else {
        return false;
    };
    let (tx, rx) = std::sync::mpsc::channel::<bool>();
    let queued = window.with_webview(move |webview| {
        use webview2_com::Microsoft::Web::WebView2::Win32::ICoreWebView2_9;
        use windows_core::Interface;
        let ok = unsafe {
            webview
                .controller()
                .CoreWebView2()
                .ok()
                .and_then(|core| core.cast::<ICoreWebView2_9>().ok())
                .map(|core| {
                    let mut shown = windows_core::BOOL(0);
                    let _ = core.IsDefaultDownloadDialogOpen(&mut shown);
                    if shown.as_bool() || !open {
                        core.CloseDefaultDownloadDialog().is_ok()
                    } else {
                        core.OpenDefaultDownloadDialog().is_ok()
                    }
                })
                .unwrap_or(false)
        };
        let _ = tx.send(ok);
    });
    if queued.is_err() {
        return false;
    }
    tokio::task::spawn_blocking(move || {
        rx.recv_timeout(std::time::Duration::from_secs(2)).unwrap_or(false)
    })
    .await
    .unwrap_or(false)
}

/// Opens (or toggles) the WebView2 download list; falls back to the Downloads
/// folder where the native dialog is unavailable. Returns "dialog" or "folder".
#[tauri::command]
pub async fn open_downloads_panel(app: AppHandle) -> Result<String, String> {
    #[cfg(target_os = "windows")]
    if webview_download_dialog(&app, true).await {
        return Ok("dialog".into());
    }
    #[cfg(not(target_os = "windows"))]
    let _ = &app;
    open_downloads_folder()
}

#[tauri::command]
pub async fn close_downloads_panel(app: AppHandle) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    {
        let _ = webview_download_dialog(&app, false).await;
    }
    #[cfg(not(target_os = "windows"))]
    let _ = &app;
    Ok(())
}
