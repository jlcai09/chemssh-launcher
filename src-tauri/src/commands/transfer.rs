use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use core_transfer::TransferProgress;
use tauri::ipc::Channel;
use tauri::State;

use crate::state::AppState;

const PROGRESS_THROTTLE_MS: u64 = 80;

#[derive(Default)]
pub(crate) struct DownloadWorkers {
    state: Mutex<(bool, usize)>, // shutting down, active command count
    changed: tokio::sync::Notify,
}

pub(crate) struct DownloadWorker<'a>(&'a DownloadWorkers);

impl DownloadWorkers {
    // Register the progress row under the same lock used to close admission:
    // shutdown's cancel_all cannot miss a worker still about to call begin.
    pub(crate) fn begin(
        &self,
        store: &core_transfer::TransferProgressStore,
        id: &str,
    ) -> Result<DownloadWorker<'_>, String> {
        let mut state = self.state.lock().unwrap_or_else(|err| err.into_inner());
        if state.0 {
            return Err("application closing".into());
        }
        store.begin(id, 0, false).map_err(|err| err.to_string())?;
        state.1 += 1;
        Ok(DownloadWorker(self))
    }

    pub(crate) fn close(&self) {
        self.state.lock().unwrap_or_else(|err| err.into_inner()).0 = true;
    }

    pub(crate) async fn drain(&self) {
        loop {
            let changed = self.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            if self.state.lock().unwrap_or_else(|err| err.into_inner()).1 == 0 {
                return;
            }
            changed.await;
        }
    }
}

impl Drop for DownloadWorker<'_> {
    fn drop(&mut self) {
        self.0.state.lock().unwrap_or_else(|err| err.into_inner()).1 -= 1;
        self.0.changed.notify_waiters();
    }
}

#[derive(Default)]
pub(crate) struct ProgressSubscriptions {
    entries: Mutex<HashMap<String, tokio::sync::watch::Sender<bool>>>,
}

impl ProgressSubscriptions {
    fn start(&self, id: &str) -> Result<tokio::sync::watch::Receiver<bool>, String> {
        if id.is_empty() || id.len() > 128 {
            return Err("invalid progress subscription ID".into());
        }
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        if entries.contains_key(id) {
            return Err("progress subscription already exists".into());
        }
        let (tx, rx) = tokio::sync::watch::channel(false);
        entries.insert(id.to_owned(), tx);
        Ok(rx)
    }

    fn stop(&self, id: &str) {
        if let Some(tx) = self
            .entries
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(id)
        {
            let _ = tx.send(true);
        }
    }

    fn finish(&self, id: &str, rx: &tokio::sync::watch::Receiver<bool>) {
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        if entries
            .get(id)
            .is_some_and(|tx| tx.subscribe().same_channel(rx))
        {
            entries.remove(id);
        }
    }
}

#[tauri::command]
pub fn get_transfer_progress(
    state: State<'_, AppState>,
    id: String,
) -> Result<Option<TransferProgress>, String> {
    Ok(state.transfers.get(&id))
}

#[tauri::command]
pub fn list_transfers(state: State<'_, AppState>) -> Result<Vec<TransferProgress>, String> {
    Ok(state.transfers.list_all())
}

#[tauri::command]
pub fn get_transfer_count(state: State<'_, AppState>) -> usize {
    state.transfers.active_count()
}

#[tauri::command]
pub fn control_transfer(
    state: State<'_, AppState>,
    id: String,
    action: String,
) -> Result<TransferProgress, String> {
    match action.as_str() {
        "pause" => state.transfers.pause(&id).map_err(|e| e.to_string()),
        "resume" => state.transfers.resume(&id).map_err(|e| e.to_string()),
        "cancel" => state.transfers.cancel(&id, None).map_err(|e| e.to_string()),
        "reset" => state.transfers.reset(&id).map_err(|e| e.to_string()),
        other => Err(format!("unsupported transfer action: {other}")),
    }
}

/// The user chooses the destination; the transfer itself never passes through the WebView.
#[tauri::command]
pub async fn pick_sftp_download_path(
    app: tauri::AppHandle,
    suggested_name: String,
) -> Result<Option<String>, String> {
    use tauri_plugin_dialog::DialogExt;

    let name = safe_download_name(&suggested_name);
    let (tx, rx) = tokio::sync::oneshot::channel();
    app.dialog()
        .file()
        .set_file_name(name)
        .save_file(move |path| {
            let _ = tx.send(path);
        });
    let path = rx
        .await
        .map_err(|_| "save dialog closed unexpectedly".to_owned())?;
    path.map(|path| {
        path.into_path()
            .map_err(|e| e.to_string())?
            .into_os_string()
            .into_string()
            .map_err(|_| "download path must be valid UTF-8".to_owned())
    })
    .transpose()
}

#[derive(serde::Serialize)]
pub struct DownloadTarget {
    local_path: String,
    renamed: bool,
}

/// One folder dialog for a batch; keep all existing files and allocate distinct
/// names. The downloader must still publish with no-clobber to close the race.
#[tauri::command]
pub async fn pick_sftp_download_dir(
    app: tauri::AppHandle,
    suggested_names: Vec<String>,
) -> Result<Option<Vec<DownloadTarget>>, String> {
    use tauri_plugin_dialog::DialogExt;

    let (tx, rx) = tokio::sync::oneshot::channel();
    app.dialog().file().pick_folder(move |path| {
        let _ = tx.send(path);
    });
    let Some(path) = rx.await.map_err(|_| "folder dialog closed unexpectedly")? else {
        return Ok(None);
    };
    let directory = path.into_path().map_err(|e| e.to_string())?;
    tokio::task::spawn_blocking(move || plan_download_targets(&directory, &suggested_names))
        .await
        .map_err(|err| err.to_string())?
        .map(Some)
}

fn safe_download_name(suggested: &str) -> String {
    let clean: String = suggested
        .chars()
        .map(|c| {
            if c.is_control() || matches!(c, '/' | '\\' | '<' | '>' | ':' | '"' | '|' | '?' | '*') {
                '_'
            } else {
                c
            }
        })
        .collect();
    let clean = clean.trim().trim_end_matches(['.', ' ']);
    if clean.is_empty() || clean == "." || clean == ".." {
        return "download".into();
    }
    // Prevent DOS device aliases, even when the remote name has an extension.
    let base = clean
        .split('.')
        .next()
        .unwrap_or_default()
        .trim_end()
        .to_ascii_uppercase();
    if matches!(
        base.as_str(),
        "CON" | "PRN" | "AUX" | "NUL" | "CONIN$" | "CONOUT$"
    ) || ["COM", "LPT"].iter().any(|prefix| {
        base.strip_prefix(prefix).is_some_and(|n| {
            matches!(
                n,
                "0" | "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9" | "¹" | "²" | "³"
            )
        })
    }) {
        return format!("_{clean}");
    }
    clean.into()
}

fn plan_download_targets(
    directory: &std::path::Path,
    suggested_names: &[String],
) -> Result<Vec<DownloadTarget>, String> {
    use std::{collections::HashSet, io::ErrorKind, path::Path};

    let mut reserved = HashSet::new();
    suggested_names
        .iter()
        .map(|suggested| {
            let base = safe_download_name(suggested);
            let stem = Path::new(&base)
                .file_stem()
                .and_then(|s| s.to_str())
                .unwrap_or(&base);
            let extension = Path::new(&base).extension().and_then(|s| s.to_str());
            for n in 0..10_000 {
                let name = match (n, extension) {
                    (0, _) => base.clone(),
                    (n, Some(ext)) => format!("{stem} ({n}).{ext}"),
                    (n, None) => format!("{stem} ({n})"),
                };
                let key = name.to_lowercase();
                if reserved.contains(&key) {
                    continue;
                }
                let path = directory.join(&name);
                match std::fs::symlink_metadata(&path) {
                    Ok(_) => continue,
                    Err(err) if err.kind() == ErrorKind::NotFound => {}
                    Err(err) => return Err(format!("could not check download destination: {err}")),
                }
                reserved.insert(key);
                let local_path = path
                    .into_os_string()
                    .into_string()
                    .map_err(|_| "download path must be valid UTF-8")?;
                return Ok(DownloadTarget {
                    local_path,
                    renamed: name != *suggested,
                });
            }
            Err("could not allocate a unique download name".into())
        })
        .collect()
}

/// Streams throttled transfer progress snapshots every 80ms over a Tauri v2 Channel.
#[tauri::command]
pub async fn stream_transfer_progress(
    state: State<'_, AppState>,
    on_event: Channel<Vec<TransferProgress>>,
    subscription_id: String,
) -> Result<(), String> {
    let store = Arc::clone(&state.transfers);
    let subscriptions = Arc::clone(&state.progress_subscriptions);
    let mut stop = subscriptions.start(&subscription_id)?;
    let initial = store.list_all();
    if let Err(err) = on_event.send(initial.clone()) {
        subscriptions.finish(&subscription_id, &stop);
        return Err(err.to_string());
    }

    tokio::spawn(async move {
        let mut last = initial;
        let mut tick = tokio::time::interval(Duration::from_millis(PROGRESS_THROTTLE_MS));
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                biased;
                _ = stop.changed() => break,
                _ = tick.tick() => {
                    let current = store.list_all();
                    if current != last {
                        if on_event.send(current.clone()).is_err() {
                            break;
                        }
                        last = current;
                    }
                }
            }
        }
        subscriptions.finish(&subscription_id, &stop);
    });
    Ok(())
}

#[tauri::command]
pub fn stop_transfer_progress(state: State<'_, AppState>, subscription_id: String) {
    state.progress_subscriptions.stop(&subscription_id);
}

#[cfg(test)]
mod tests {
    use super::{
        plan_download_targets, safe_download_name, DownloadWorkers, ProgressSubscriptions,
    };
    use core_transfer::TransferProgressStore;

    #[test]
    fn batch_names_keep_existing_files_and_allocate_ordered_unique_targets() {
        let dir = tempfile::tempdir().unwrap();
        let existing = dir.path().join("results.txt");
        std::fs::write(&existing, b"existing").unwrap();
        let targets = plan_download_targets(
            dir.path(),
            &[
                "results.txt".into(),
                "results.txt".into(),
                "Other.txt".into(),
                "other.txt".into(),
            ],
        )
        .unwrap();
        let names: Vec<_> = targets
            .iter()
            .map(|target| {
                std::path::Path::new(&target.local_path)
                    .file_name()
                    .unwrap()
                    .to_str()
                    .unwrap()
            })
            .collect();
        assert_eq!(
            names,
            [
                "results (1).txt",
                "results (2).txt",
                "Other.txt",
                "other (1).txt"
            ]
        );
        assert_eq!(
            targets
                .iter()
                .map(|target| target.renamed)
                .collect::<Vec<_>>(),
            [true, true, false, true]
        );
        assert_eq!(std::fs::read(existing).unwrap(), b"existing");
        assert_eq!(safe_download_name("../CON.txt"), ".._CON.txt");
        assert_eq!(safe_download_name("CON.txt"), "_CON.txt");
        assert_eq!(safe_download_name(".."), "download");
    }

    #[tokio::test]
    async fn download_shutdown_stops_admission_and_waits_for_worker_cleanup() {
        let workers = DownloadWorkers::default();
        let store = TransferProgressStore::new();
        let worker = workers.begin(&store, "download").unwrap();
        workers.close();
        assert!(workers.begin(&store, "late").is_err());
        assert_eq!(store.active_count(), 1);
        store.cancel_all(None);
        tokio::join!(workers.drain(), async {
            tokio::task::yield_now().await;
            assert!(store.is_cancelled("download"));
            store.finish("download", None);
            drop(worker);
        });
        assert_eq!(store.active_count(), 0);
    }

    #[tokio::test]
    async fn stopped_subscription_cannot_remove_its_replacement() {
        let subscriptions = ProgressSubscriptions::default();
        let mut old = subscriptions.start("view").unwrap();
        assert!(subscriptions.start("view").is_err());
        subscriptions.stop("view");
        old.changed().await.unwrap();
        assert!(*old.borrow());

        let mut replacement = subscriptions.start("view").unwrap();
        subscriptions.finish("view", &old);
        assert!(subscriptions.start("view").is_err());
        subscriptions.stop("view");
        replacement.changed().await.unwrap();
        assert!(*replacement.borrow());
    }
}
