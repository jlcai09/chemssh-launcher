pub mod commands;

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tauri::Manager;

use crate::commands::WindowRevealState;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let reveal_state = WindowRevealState::default();
    let fallback_flag = Arc::clone(&reveal_state.revealed);

    tauri::Builder::default()
        .manage(reveal_state)
        .setup(move |app| {
            let handle = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                tokio::time::sleep(Duration::from_secs(2)).await;
                if !fallback_flag.swap(true, Ordering::SeqCst) {
                    if let Some(window) = handle.get_webview_window("main") {
                        let _ = window.show();
                        let _ = window.set_focus();
                    }
                }
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::ping,
            commands::window_ready,
            commands::demo_channel_stream
        ])
        .run(tauri::generate_context!())
        .expect("error while running ChemSSH Launcher Tauri application");
}
