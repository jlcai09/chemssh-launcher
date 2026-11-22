pub mod commands;
pub mod state;

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tauri::{Emitter, Manager, WindowEvent};

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
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                let state = window.state::<AppState>();
                let active_transfers = state.transfers.active_count();
                let active_drags = state.drag_jobs.active_count();
                if active_transfers > 0 || active_drags > 0 {
                    api.prevent_close();
                    let _ = window.emit(
                        "app://close-requested",
                        commands::dragout::ActiveWorkSummary {
                            active_transfers,
                            active_drag_jobs: active_drags,
                        },
                    );
                }
            }
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
            commands::backend::open_config_dir,
            commands::backend::open_sftp_cache_dir,
            commands::backend::clear_cache,
            commands::backend::export_profiles,
            commands::backend::import_profiles,
            commands::sessions::test_profile_connection,
            commands::sessions::start_session,
            commands::sessions::activate_session,
            commands::sessions::pause_forwarding,
            commands::sessions::stop_session,
            commands::sessions::get_session_status,
            commands::sessions::list_session_statuses,
            commands::sessions::get_session_logs,
            commands::sessions::stream_session_logs,
            commands::terminal::open_terminal,
            commands::terminal::get_terminal_cwd,
            commands::terminal::send_terminal_input,
            commands::terminal::resize_terminal,
            commands::terminal::close_terminal,
            commands::local_fs::get_local_home,
            commands::local_fs::local_list,
            commands::local_fs::local_mkdir_cmd,
            commands::local_fs::local_create_file_cmd,
            commands::local_fs::local_rename_cmd,
            commands::local_fs::local_delete_cmd,
            commands::local_fs::local_copy_cmd,
            commands::local_fs::local_open_cmd,
            commands::local_fs::local_archive_tools,
            commands::local_fs::local_compress,
            commands::local_fs::local_extract,
            commands::transfer::get_transfer_progress,
            commands::transfer::list_transfers,
            commands::transfer::get_transfer_count,
            commands::transfer::control_transfer,
            commands::transfer::stream_transfer_progress,
            commands::sftp::sftp_connect,
            commands::sftp::sftp_disconnect,
            commands::sftp::sftp_list,
            commands::sftp::sftp_realpath,
            commands::sftp::sftp_mkdir,
            commands::sftp::sftp_mkdirs,
            commands::sftp::sftp_create_file,
            commands::sftp::sftp_rename,
            commands::sftp::sftp_delete,
            commands::sftp::sftp_upload_local,
            commands::sftp::sftp_download_local,
            commands::sftp::sftp_copy_remote,
            commands::sftp::sftp_archive_tools,
            commands::sftp::sftp_compress,
            commands::sftp::sftp_extract,
            commands::dragout::is_native_drag_supported,
            commands::dragout::list_native_drag_jobs,
            commands::dragout::control_native_drag_job,
            commands::dragout::has_active_work,
            commands::dragout::start_native_drag_out,
        ])
        .run(tauri::generate_context!())
        .expect("error while running ChemSSH Launcher Tauri application");
}
