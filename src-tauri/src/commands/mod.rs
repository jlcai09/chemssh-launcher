pub mod backend;
pub mod dragout;
pub mod local_fs;
pub mod profiles;
pub mod sessions;
pub mod sftp;
pub mod terminal;
pub mod transfer;

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde::Serialize;
use tauri::ipc::Channel;
use tauri::{AppHandle, Manager, State};

pub const APP_VERSION: &str = env!("CARGO_PKG_VERSION");

#[derive(Default)]
pub struct WindowRevealState {
    pub(crate) revealed: Arc<AtomicBool>,
}

impl WindowRevealState {
    fn reveal_main_window(&self, app: &AppHandle, maximize: bool) -> Result<(), String> {
        if self.revealed.swap(true, Ordering::SeqCst) {
            return Ok(());
        }
        if let Some(window) = app.get_webview_window("main") {
            if maximize {
                let _ = window.maximize();
            }
            window.show().map_err(|e| e.to_string())?;
            let _ = window.set_focus();
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct StreamTick {
    pub seq: u32,
    pub message: String,
}

#[tauri::command]
pub fn ping() -> String {
    APP_VERSION.to_string()
}

#[tauri::command]
pub fn window_ready(
    app: AppHandle,
    state: State<'_, WindowRevealState>,
    maximize: Option<bool>,
) -> Result<(), String> {
    state.reveal_main_window(&app, maximize.unwrap_or(true))
}

#[tauri::command]
pub async fn demo_channel_stream(
    on_event: Channel<StreamTick>,
    count: Option<u32>,
) -> Result<(), String> {
    let total = count.unwrap_or(3).min(20);
    for seq in 1..=total {
        on_event
            .send(StreamTick {
                seq,
                message: format!("tick-{seq}"),
            })
            .map_err(|e| e.to_string())?;
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ping_returns_package_version() {
        assert_eq!(ping(), "0.4.0");
    }
}
