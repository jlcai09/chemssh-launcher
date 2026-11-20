pub mod commands;
pub mod state;

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tauri::Manager;

use crate::commands::WindowRevealState;
use crate::state::AppState;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app_state = AppState::new_default().expect("failed to initialize launcher state");
    let reveal_state = WindowRevealState::default();
    let fallback_flag = Arc::clone(&reveal_state.revealed);

    tauri::Builder::default()
        .manage(app_state)
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
            commands::demo_channel_stream,
            commands::profiles::list_profiles,
            commands::profiles::get_profile,
            commands::profiles::save_profile,
            commands::profiles::delete_profile,
            commands::profiles::reorder_profiles,
            commands::profiles::duplicate_profile,
            commands::profiles::profile_defaults,
            commands::profiles::check_local_port,
            commands::backend::get_version,
            commands::backend::get_ui_settings,
            commands::backend::save_ui_settings,
            commands::backend::get_backend_info,
            commands::backend::export_profiles,
            commands::backend::import_profiles,
        ])
        .run(tauri::generate_context!())
        .expect("error while running ChemSSH Launcher Tauri application");
}
