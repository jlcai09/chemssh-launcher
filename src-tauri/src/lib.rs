pub mod chemssh_bridge;
pub mod chemssh_proxy;
pub mod client_identity;
pub mod commands;
pub mod state;
pub mod terminal_ws;

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tauri::{Emitter, Manager, RunEvent, WindowEvent};
use url::Url;

use crate::commands::WindowRevealState;
use crate::state::AppState;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app_state = AppState::new_default().expect("failed to initialize launcher state");
    let reveal_state = WindowRevealState::default();
    let fallback_flag = Arc::clone(&reveal_state.revealed);

    let app = tauri::Builder::default()
        .manage(app_state)
        .manage(reveal_state)
        .register_uri_scheme_protocol("fileicon", |ctx, request| {
            if request.method() == tauri::http::Method::OPTIONS {
                return tauri::http::Response::builder()
                    .status(204)
                    .header("Access-Control-Allow-Origin", "*")
                    .header("Access-Control-Allow-Methods", "GET, OPTIONS")
                    .header("Access-Control-Allow-Headers", "*")
                    .body(Vec::new())
                    .unwrap_or_default();
            }

            let uri = request.uri().to_string();
            let parsed = Url::parse(&uri).ok();
            let mut name = String::new();
            let mut is_dir = false;
            let mut size = 16u32;

            if let Some(url) = parsed {
                for (k, v) in url.query_pairs() {
                    match k.as_ref() {
                        "name" => name = v.into_owned(),
                        "is_dir" => {
                            let val = v.trim().to_ascii_lowercase();
                            is_dir = val == "1" || val == "true";
                        }
                        "size" => {
                            if let Ok(parsed_size) = v.trim().parse::<u32>() {
                                size = parsed_size;
                            }
                        }
                        _ => {}
                    }
                }
            }

            let state = ctx.app_handle().state::<AppState>();
            let icon = state.file_icons.get_icon(&name, is_dir, size);
            tauri::http::Response::builder()
                .status(200)
                .header("Content-Type", icon.content_type)
                .header("Cache-Control", "public, max-age=86400")
                .header("Access-Control-Allow-Origin", "*")
                .body(icon.bytes.to_vec())
                .unwrap_or_default()
        })
        .setup(move |app| {
            let proxy_state = app.state::<AppState>().inner().clone();
            Arc::clone(&proxy_state.terminals)
                .spawn_sleep_wake_monitor(Arc::clone(&proxy_state.launcher_logs));
            if let Some(listener) = proxy_state
                .proxy_listener
                .lock()
                .ok()
                .and_then(|mut guard| guard.take())
            {
                chemssh_proxy::spawn_proxy_server(listener, Arc::new(proxy_state));
            }

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
            commands::backend::get_launcher_logs,
            commands::backend::open_config_dir,
            commands::backend::open_downloads_panel,
            commands::backend::close_downloads_panel,
            commands::backend::open_sftp_cache_dir,
            commands::backend::clear_cache,
            commands::backend::export_profiles,
            commands::backend::import_profiles,
            commands::sessions::test_profile_connection,
            commands::installer::installer_check,
            commands::installer::installer_run,
            commands::installer::installer_status,
            commands::installer::installer_apply_profile,
            commands::sessions::start_session,
            commands::sessions::activate_session,
            commands::sessions::pause_forwarding,
            commands::sessions::stop_session,
            commands::sessions::get_session_status,
            commands::sessions::list_session_statuses,
            commands::sessions::get_session_logs,
            commands::sessions::poll_session_logs,
            commands::sessions::stream_session_logs,
            commands::terminal::open_terminal,
            commands::terminal::poll_terminal_events,
            commands::terminal::get_terminal_cwd,
            commands::terminal::send_terminal_input,
            commands::terminal::resize_terminal,
            commands::terminal::close_terminal,
            commands::terminal::probe_terminal_sessions,
            commands::local_fs::get_local_home,
            commands::local_fs::local_list,
            commands::local_fs::local_mkdir_cmd,
            commands::local_fs::local_create_file_cmd,
            commands::local_fs::local_rename_cmd,
            commands::local_fs::local_delete_cmd,
            commands::local_fs::local_copy_cmd,
            commands::local_fs::local_open_cmd,
            commands::local_fs::local_open_text_cmd,
            commands::local_fs::local_archive_tools,
            commands::local_fs::local_compress,
            commands::local_fs::local_extract,
            commands::transfer::get_transfer_progress,
            commands::transfer::list_transfers,
            commands::transfer::get_transfer_count,
            commands::transfer::control_transfer,
            commands::transfer::stream_transfer_progress,
            commands::sftp::sftp_connect,
            commands::sftp::sftp_reconnect,
            commands::sftp::sftp_disconnect,
            commands::sftp::sftp_list,
            commands::sftp::sftp_realpath,
            commands::sftp::sftp_mkdir,
            commands::sftp::sftp_mkdirs,
            commands::sftp::sftp_create_file,
            commands::sftp::sftp_rename,
            commands::sftp::sftp_delete,
            commands::sftp::sftp_open_file,
            commands::sftp::sftp_open_sync_events,
            commands::sftp::sftp_server_copy_support,
            commands::sftp::sftp_copy_on_server,
            commands::sftp::sftp_upload_local,
            commands::sftp::sftp_download_local,
            commands::sftp::sftp_upload_bytes_begin,
            commands::sftp::sftp_upload_bytes_chunk,
            commands::sftp::sftp_upload_bytes_finish,
            commands::sftp::sftp_download_bytes,
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
        .build(tauri::generate_context!())
        .expect("error while building ChemSSH Launcher Tauri application");

    app.run(|app_handle, event| {
        if let RunEvent::Exit = event {
            let state = app_handle.state::<AppState>();
            let bridge = Arc::clone(&state.chemssh_bridge);
            let sessions = Arc::clone(&state.sessions);
            let terminals = Arc::clone(&state.terminals);
            let sftp = Arc::clone(&state.sftp);
            let sftp_sync = Arc::clone(&state.sftp_sync);
            let launcher_logs = Arc::clone(&state.launcher_logs);
            tauri::async_runtime::block_on(async move {
                let _ = tokio::time::timeout(Duration::from_secs(3), async move {
                    bridge.close_all(&sftp, &sftp_sync, &launcher_logs).await;
                    tokio::join!(
                        sessions.stop_all_on_exit(),
                        terminals.close_all(),
                        sftp.disconnect_all(),
                    );
                })
                .await;
            });
        }
    });
}
