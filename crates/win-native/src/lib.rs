pub mod file_icon;
pub mod file_icon_fallback;
#[cfg(target_os = "windows")]
pub mod file_icon_windows;
pub mod jobs;
#[cfg(target_os = "windows")]
pub mod ole_windows;
#[cfg(not(target_os = "windows"))]
pub mod platform_stub;
pub mod trash_windows;

use serde::{Deserialize, Serialize};
use thiserror::Error;

pub use file_icon::{FileIconData, FileIconService};
pub use jobs::{is_terminal_state, DragJob, DragJobRegistry, FileProgress};
pub use trash_windows::move_to_trash;

#[cfg(target_os = "windows")]
pub use ole_windows::{
    start_shell_file_drag, start_shell_file_drag_async, start_shell_file_drag_delayed,
};
#[cfg(not(target_os = "windows"))]
pub use platform_stub::{
    start_shell_file_drag, start_shell_file_drag_async, start_shell_file_drag_delayed,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DragDropOutcome {
    Dropped,
    Cancelled,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AsyncDragEvent {
    Started,
    Done { effect: u32 },
    Cancelled { reason: Option<String> },
    Error(String),
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct NativeDragGestureResult {
    pub outcome: DragDropOutcome,
    pub pending: bool,
    pub ok: bool,
    pub status: String,
}

#[derive(Debug, Error)]
pub enum WinNativeError {
    #[error("native Windows OLE drag-out is not supported on this platform")]
    UnsupportedPlatform,
    #[error("drag job already exists: {0}")]
    DuplicateJob(String),
    #[error("drag job not found: {0}")]
    JobNotFound(String),
    #[error("drag job cancelled")]
    Cancelled,
    #[error("platform error: {0}")]
    Platform(String),
    #[error("internal error: {0}")]
    Internal(String),
}

pub fn is_windows_native_supported() -> bool {
    cfg!(target_os = "windows")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    #[test]
    fn platform_flag_matches_target() {
        assert_eq!(is_windows_native_supported(), cfg!(target_os = "windows"));
    }

    #[test]
    fn drag_job_state_machine_and_history_trim_keeps_active_jobs() {
        let reg = DragJobRegistry::new();
        let job = reg.add("j1", "s1", vec!["/home/user/a.txt".into()]).unwrap();
        assert_eq!(job.state, "queued");
        assert_eq!(job.revision, 1);

        let accepted = reg.update_state("j1", "accepted", None).unwrap();
        assert_eq!(accepted.state, "accepted");
        assert_eq!(accepted.revision, 2);

        // Late accepted notification cannot regress an active job.
        assert!(reg.update_state("j1", "accepted", None).is_none());

        let caching = reg.update_state("j1", "caching", None).unwrap();
        assert_eq!(caching.state, "caching");

        reg.add_progress("j1", 1024, 512);
        reg.update_file_progress(
            "j1",
            FileProgress {
                path: "/home/user/a.txt".into(),
                kind: "file".into(),
                loaded: 512,
                total: 1024,
                state: "caching".into(),
                done: false,
                error: String::new(),
            },
        );

        // Fill > 50 completed jobs; active job j1 must never be pruned.
        for i in 0..55 {
            let id = format!("done-{i}");
            reg.add(&id, "s1", vec!["/home/user/x".into()]).unwrap();
            reg.update_state(&id, "done", None).unwrap();
        }
        assert_eq!(reg.active_count(), 1);
        let list = reg.list();
        assert!(list.iter().any(|j| j.id == "j1"));

        // Terminal state cannot be overwritten.
        reg.update_state("j1", "done", None).unwrap();
        assert!(reg.update_state("j1", "error", Some("late err")).is_none());
    }

    #[tokio::test]
    async fn drag_job_pause_resume_and_cancel() {
        let reg = Arc::new(DragJobRegistry::new());
        reg.add("j2", "s1", vec!["/home/user/b.bin".into()]).unwrap();
        reg.pause("j2").unwrap();

        let bg = Arc::clone(&reg);
        tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_millis(30)).await;
            bg.resume("j2").unwrap();
        });
        reg.wait_if_paused("j2").await.unwrap();

        reg.cancel("j2").unwrap();
        assert!(matches!(
            reg.wait_if_paused("j2").await,
            Err(WinNativeError::Cancelled)
        ));
    }
}
