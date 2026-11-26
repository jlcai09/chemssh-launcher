use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use tokio::sync::Notify;

use crate::WinNativeError;

const MAX_HISTORY_JOBS: usize = 50;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FileProgress {
    pub path: String,
    pub loaded: u64,
    pub total: u64,
    pub done: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DragJob {
    pub id: String,
    pub session: String,
    pub paths: Vec<String>,
    pub state: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    pub revision: u64,
    pub created_at: i64,
    pub loaded: u64,
    pub total: u64,
    pub paused: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ended_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub files: Vec<FileProgress>,
}

struct DragJobEntry {
    job: DragJob,
    notify: Arc<Notify>,
    file_index: HashMap<String, usize>,
}

pub fn is_terminal_state(state: &str) -> bool {
    matches!(state, "done" | "error" | "cancelled")
}

fn now_millis() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[derive(Default)]
pub struct DragJobRegistry {
    inner: Mutex<RegistryInner>,
}

#[derive(Default)]
struct RegistryInner {
    jobs: HashMap<String, DragJobEntry>,
    revision: u64,
    closed: bool,
}

impl DragJobRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn add(
        &self,
        id: impl Into<String>,
        session: impl Into<String>,
        paths: Vec<String>,
    ) -> Result<DragJob, WinNativeError> {
        let id = id.into();
        let mut guard = self
            .inner
            .lock()
            .map_err(|_| WinNativeError::Internal("drag job registry lock poisoned".into()))?;
        if guard.closed {
            return Err(WinNativeError::Internal("drag job registry closed".into()));
        }
        if guard.jobs.contains_key(&id) {
            return Err(WinNativeError::DuplicateJob(id));
        }

        guard.revision += 1;
        let job = DragJob {
            id: id.clone(),
            session: session.into(),
            paths,
            state: "queued".into(),
            error: String::new(),
            revision: guard.revision,
            created_at: now_millis(),
            loaded: 0,
            total: 0,
            paused: false,
            ended_at: None,
            files: Vec::new(),
        };
        guard.jobs.insert(
            id,
            DragJobEntry {
                job: job.clone(),
                notify: Arc::new(Notify::new()),
                file_index: HashMap::new(),
            },
        );
        trim_history_locked(&mut guard);
        Ok(job)
    }

    pub fn update_state(
        &self,
        id: &str,
        next_state: &str,
        err_msg: Option<&str>,
    ) -> Option<DragJob> {
        let mut guard = self.inner.lock().ok()?;
        if guard.closed {
            return None;
        }
        let next_rev = guard.revision + 1;
        let entry = guard.jobs.get_mut(id)?;
        if is_terminal_state(&entry.job.state) {
            return None;
        }
        if next_state == "delivering" && entry.job.paused {
            return None;
        }
        if next_state == "accepted" && entry.job.state != "queued" {
            return None;
        }
        if next_state == "caching" && entry.job.state == "delivering" {
            return None;
        }

        entry.job.state = next_state.to_string();
        entry.job.revision = next_rev;
        if let Some(msg) = err_msg {
            entry.job.error = msg.to_string();
        }
        if is_terminal_state(next_state) {
            entry.job.paused = false;
            entry.job.ended_at = Some(now_millis());
        }
        entry.notify.notify_waiters();
        let snapshot = entry.job.clone();
        guard.revision = next_rev;
        Some(snapshot)
    }

    pub fn add_progress(&self, id: &str, delta_total: u64, delta_loaded: u64) -> Option<DragJob> {
        let mut guard = self.inner.lock().ok()?;
        let next_rev = guard.revision + 1;
        let entry = guard.jobs.get_mut(id)?;
        entry.job.total = entry.job.total.saturating_add(delta_total);
        entry.job.loaded = entry.job.loaded.saturating_add(delta_loaded);
        if entry.job.total > 0 && entry.job.loaded > entry.job.total {
            entry.job.loaded = entry.job.total;
        }
        entry.job.revision = next_rev;
        let snapshot = entry.job.clone();
        guard.revision = next_rev;
        Some(snapshot)
    }

    pub fn update_file_progress(&self, id: &str, file: FileProgress) -> Option<DragJob> {
        let mut guard = self.inner.lock().ok()?;
        let next_rev = guard.revision + 1;
        let entry = guard.jobs.get_mut(id)?;
        if let Some(&idx) = entry.file_index.get(&file.path) {
            entry.job.files[idx] = file;
        } else {
            let idx = entry.job.files.len();
            entry.file_index.insert(file.path.clone(), idx);
            entry.job.files.push(file);
        }
        entry.job.revision = next_rev;
        let snapshot = entry.job.clone();
        guard.revision = next_rev;
        Some(snapshot)
    }

    pub fn pause(&self, id: &str) -> Result<DragJob, WinNativeError> {
        let mut guard = self
            .inner
            .lock()
            .map_err(|_| WinNativeError::Internal("drag job registry lock poisoned".into()))?;
        let next_rev = guard.revision + 1;
        let entry = guard
            .jobs
            .get_mut(id)
            .ok_or_else(|| WinNativeError::JobNotFound(id.to_string()))?;
        if !is_terminal_state(&entry.job.state) {
            entry.job.paused = true;
            entry.job.revision = next_rev;
            entry.notify.notify_waiters();
            let snapshot = entry.job.clone();
            guard.revision = next_rev;
            return Ok(snapshot);
        }
        Ok(entry.job.clone())
    }

    pub fn resume(&self, id: &str) -> Result<DragJob, WinNativeError> {
        let mut guard = self
            .inner
            .lock()
            .map_err(|_| WinNativeError::Internal("drag job registry lock poisoned".into()))?;
        let next_rev = guard.revision + 1;
        let entry = guard
            .jobs
            .get_mut(id)
            .ok_or_else(|| WinNativeError::JobNotFound(id.to_string()))?;
        if entry.job.state == "cancelled" {
            return Err(WinNativeError::Cancelled);
        }
        if !is_terminal_state(&entry.job.state) {
            entry.job.paused = false;
            entry.job.revision = next_rev;
            entry.notify.notify_waiters();
            let snapshot = entry.job.clone();
            guard.revision = next_rev;
            return Ok(snapshot);
        }
        Ok(entry.job.clone())
    }

    pub fn cancel(&self, id: &str) -> Result<DragJob, WinNativeError> {
        let mut guard = self
            .inner
            .lock()
            .map_err(|_| WinNativeError::Internal("drag job registry lock poisoned".into()))?;
        let next_rev = guard.revision + 1;
        let entry = guard
            .jobs
            .get_mut(id)
            .ok_or_else(|| WinNativeError::JobNotFound(id.to_string()))?;
        if !is_terminal_state(&entry.job.state) {
            entry.job.state = "cancelled".into();
            entry.job.paused = false;
            entry.job.error = "drag cancelled".into();
            entry.job.ended_at = Some(now_millis());
            entry.job.revision = next_rev;
            entry.notify.notify_waiters();
            let snapshot = entry.job.clone();
            guard.revision = next_rev;
            return Ok(snapshot);
        }
        Ok(entry.job.clone())
    }

    pub async fn wait_if_paused(&self, id: &str) -> Result<(), WinNativeError> {
        loop {
            let notify = {
                let guard = self
                    .inner
                    .lock()
                    .map_err(|_| WinNativeError::Internal("drag job registry lock poisoned".into()))?;
                let Some(entry) = guard.jobs.get(id) else {
                    return Ok(());
                };
                if entry.job.state == "cancelled" {
                    return Err(WinNativeError::Cancelled);
                }
                if !entry.job.paused {
                    return Ok(());
                }
                Arc::clone(&entry.notify)
            };
            let _ = tokio::time::timeout(Duration::from_millis(250), notify.notified()).await;
        }
    }

    pub fn get(&self, id: &str) -> Option<DragJob> {
        let guard = self.inner.lock().ok()?;
        guard.jobs.get(id).map(|e| e.job.clone())
    }

    pub fn list(&self) -> Vec<DragJob> {
        let Ok(guard) = self.inner.lock() else {
            return Vec::new();
        };
        let mut jobs: Vec<DragJob> = guard.jobs.values().map(|e| e.job.clone()).collect();
        jobs.sort_by(|a, b| b.created_at.cmp(&a.created_at).then_with(|| b.revision.cmp(&a.revision)));
        jobs
    }

    pub fn active_count(&self) -> usize {
        self.inner
            .lock()
            .ok()
            .map(|g| {
                g.jobs
                    .values()
                    .filter(|e| !is_terminal_state(&e.job.state))
                    .count()
            })
            .unwrap_or(0)
    }

    /// Cancels all non-terminal drag jobs and wakes any paused waiters.
    pub fn cancel_all(&self, reason: Option<&str>) -> usize {
        let Ok(mut guard) = self.inner.lock() else {
            return 0;
        };
        let msg = reason.unwrap_or("drag cancelled");
        let now = now_millis();
        let mut count = 0;
        let mut next_rev = guard.revision;
        for entry in guard.jobs.values_mut() {
            if !is_terminal_state(&entry.job.state) {
                next_rev += 1;
                entry.job.state = "cancelled".into();
                entry.job.paused = false;
                entry.job.error = msg.to_string();
                entry.job.ended_at = Some(now);
                entry.job.revision = next_rev;
                entry.notify.notify_waiters();
                count += 1;
            }
        }
        guard.revision = next_rev;
        count
    }
}

fn trim_history_locked(inner: &mut RegistryInner) {
    while inner.jobs.len() > MAX_HISTORY_JOBS {
        let oldest_terminal = inner
            .jobs
            .values()
            .filter(|e| is_terminal_state(&e.job.state))
            .min_by_key(|e| e.job.revision)
            .map(|e| e.job.id.clone());
        let Some(remove_id) = oldest_terminal else {
            break;
        };
        inner.jobs.remove(&remove_id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cancel_all_cancels_active_jobs_and_leaves_terminal_jobs() {
        let reg = DragJobRegistry::new();
        reg.add("j1", "s1", vec!["/a".into()]).unwrap();
        reg.add("j2", "s1", vec!["/b".into()]).unwrap();
        reg.update_state("j2", "done", None);
        assert_eq!(reg.active_count(), 1);

        let cancelled = reg.cancel_all(Some("application closing"));
        assert_eq!(cancelled, 1);
        assert_eq!(reg.active_count(), 0);
        let j1 = reg.get("j1").unwrap();
        assert_eq!(j1.state, "cancelled");
        assert_eq!(j1.error, "application closing");
        let j2 = reg.get("j2").unwrap();
        assert_eq!(j2.state, "done");
    }
}
