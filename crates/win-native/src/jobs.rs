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
    #[serde(default)]
    pub kind: String,
    pub loaded: u64,
    pub total: u64,
    #[serde(default)]
    pub state: String,
    pub done: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

fn is_zero_u64(v: &u64) -> bool {
    *v == 0
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transfer_started_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transfer_ended_at: Option<i64>,
    #[serde(default, skip_serializing_if = "is_zero_u64")]
    pub paused_ms: u64,
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
    pause_started_at: Option<i64>,
}

impl DragJobEntry {
    fn accumulate_pause(&mut self, now: i64) {
        if let Some(pause_start) = self.pause_started_at.take() {
            if let Some(transfer_start) = self.job.transfer_started_at {
                let effective_start = pause_start.max(transfer_start);
                if now > effective_start {
                    self.job.paused_ms = self
                        .job
                        .paused_ms
                        .saturating_add((now - effective_start) as u64);
                }
            }
        }
    }
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
            transfer_started_at: None,
            transfer_ended_at: None,
            paused_ms: 0,
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
                pause_started_at: None,
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

        let now = now_millis();
        entry.job.state = next_state.to_string();
        entry.job.revision = next_rev;
        if let Some(msg) = err_msg {
            entry.job.error = msg.to_string();
        }
        if next_state == "caching" && entry.job.transfer_started_at.is_none() {
            entry.job.transfer_started_at = Some(now);
        }
        if next_state == "delivering" {
            entry.accumulate_pause(now);
            if entry.job.transfer_started_at.is_some() && entry.job.transfer_ended_at.is_none() {
                entry.job.transfer_ended_at = Some(now);
            }
        }
        if is_terminal_state(next_state) {
            entry.accumulate_pause(now);
            entry.job.paused = false;
            entry.job.ended_at = Some(now);
            if entry.job.transfer_started_at.is_some() && entry.job.transfer_ended_at.is_none() {
                entry.job.transfer_ended_at = Some(now);
            }
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
        if delta_loaded > 0 && entry.job.transfer_started_at.is_none() {
            entry.job.transfer_started_at = Some(now_millis());
        }
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
            if !entry.job.paused {
                entry.pause_started_at = Some(now_millis());
            }
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
            entry.accumulate_pause(now_millis());
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
            let now = now_millis();
            entry.accumulate_pause(now);
            entry.job.state = "cancelled".into();
            entry.job.paused = false;
            entry.job.error = "drag cancelled".into();
            entry.job.ended_at = Some(now);
            if entry.job.transfer_started_at.is_some() && entry.job.transfer_ended_at.is_none() {
                entry.job.transfer_ended_at = Some(now);
            }
            entry.job.revision = next_rev;
            entry.notify.notify_waiters();
            let snapshot = entry.job.clone();
            guard.revision = next_rev;
            return Ok(snapshot);
        }
        Ok(entry.job.clone())
    }

    pub fn is_cancelled(&self, id: &str) -> bool {
        self.inner
            .lock()
            .ok()
            .and_then(|g| g.jobs.get(id).map(|e| e.job.state == "cancelled"))
            .unwrap_or(false)
    }

    pub async fn wait_for_cancel(&self, id: &str) {
        loop {
            let notify = {
                let Ok(guard) = self.inner.lock() else {
                    return;
                };
                let Some(entry) = guard.jobs.get(id) else {
                    return;
                };
                if entry.job.state == "cancelled" {
                    return;
                }
                Arc::clone(&entry.notify)
            };
            let notified = notify.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.is_cancelled(id) {
                return;
            }
            notified.await;
        }
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
                entry.accumulate_pause(now);
                entry.job.state = "cancelled".into();
                entry.job.paused = false;
                entry.job.error = msg.to_string();
                entry.job.ended_at = Some(now);
                if entry.job.transfer_started_at.is_some() && entry.job.transfer_ended_at.is_none() {
                    entry.job.transfer_ended_at = Some(now);
                }
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

    #[test]
    fn transfer_timing_tracks_caching_bounds_and_excludes_pause_duration() {
        let reg = DragJobRegistry::new();
        let queued = reg.add("j-time", "s1", vec!["/a".into()]).unwrap();
        assert_eq!(queued.transfer_started_at, None);
        assert_eq!(queued.transfer_ended_at, None);
        assert_eq!(queued.paused_ms, 0);

        let accepted = reg.update_state("j-time", "accepted", None).unwrap();
        assert_eq!(accepted.transfer_started_at, None);

        std::thread::sleep(Duration::from_millis(15));
        let caching = reg.update_state("j-time", "caching", None).unwrap();
        let started = caching.transfer_started_at.expect("caching sets transfer_started_at");
        assert!(started >= queued.created_at);
        assert_eq!(caching.transfer_ended_at, None);

        reg.pause("j-time").unwrap();
        std::thread::sleep(Duration::from_millis(20));
        let resumed = reg.resume("j-time").unwrap();
        assert!(resumed.paused_ms >= 10, "expected paused_ms >= 10, got {}", resumed.paused_ms);

        let delivering = reg.update_state("j-time", "delivering", None).unwrap();
        let transfer_end = delivering.transfer_ended_at.expect("delivering freezes transfer_ended_at");
        assert!(transfer_end >= started);

        std::thread::sleep(Duration::from_millis(15));
        let done = reg.update_state("j-time", "done", None).unwrap();
        assert_eq!(done.transfer_ended_at, Some(transfer_end));
        assert!(done.ended_at.unwrap() >= transfer_end);
    }
}
