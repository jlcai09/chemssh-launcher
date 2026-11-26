use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::sync::Notify;

use crate::TransferError;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TransferProgress {
    pub id: String,
    pub loaded: u64,
    pub total: u64,
    pub done: bool,
    pub paused: bool,
    pub cancelled: bool,
    #[serde(default)]
    pub committing: bool,
    pub error: String,
    #[serde(default)]
    pub retrying: bool,
    #[serde(default)]
    pub attempt: u32,
    #[serde(default)]
    pub max_attempts: u32,
}

impl TransferProgress {
    fn new(id: String, total: u64) -> Self {
        Self {
            id,
            loaded: 0,
            total,
            done: false,
            paused: false,
            cancelled: false,
            committing: false,
            error: String::new(),
            retrying: false,
            attempt: 0,
            max_attempts: 0,
        }
    }
}

struct TransferEntry {
    progress: TransferProgress,
    active: bool,
    notify: Arc<Notify>,
}

#[derive(Default)]
pub struct TransferProgressStore {
    items: Mutex<HashMap<String, TransferEntry>>,
}

impl TransferProgressStore {
    pub fn new() -> Self {
        Self::default()
    }

    /// Prepares a transfer and returns the byte offset to start/resume from.
    /// A cancelled transfer never resumes.
    pub fn begin(&self, id: &str, total: u64, resume: bool) -> Result<u64, TransferError> {
        if id.is_empty() {
            return Ok(0);
        }
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard
            .entry(id.to_string())
            .or_insert_with(|| TransferEntry {
                progress: TransferProgress::new(id.to_string(), total),
                active: false,
                notify: Arc::new(Notify::new()),
            });

        if entry.active {
            return Err(TransferError::AlreadyRunning(id.to_string()));
        }
        if entry.progress.cancelled {
            entry.progress.done = true;
            if entry.progress.error.is_empty() {
                entry.progress.error = "transfer cancelled".into();
            }
            return Err(TransferError::Cancelled);
        }

        let offset = if resume {
            entry.progress.loaded.min(total)
        } else {
            0
        };
        entry.active = true;
        entry.progress.committing = false;
        entry.progress.loaded = offset;
        entry.progress.total = total;
        entry.progress.done = false;
        entry.progress.error.clear();
        entry.notify.notify_waiters();
        Ok(offset)
    }

    pub fn add_loaded(&self, id: &str, delta: u64) {
        if id.is_empty() || delta == 0 {
            return;
        }
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id) {
                entry.progress.loaded = entry.progress.loaded.saturating_add(delta);
                if entry.progress.total > 0 && entry.progress.loaded > entry.progress.total {
                    entry.progress.loaded = entry.progress.total;
                }
            }
        }
    }

    pub fn set_loaded(&self, id: &str, loaded: u64) {
        if id.is_empty() {
            return;
        }
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id) {
                entry.progress.loaded = loaded;
            }
        }
    }

    /// Start the byte counter for a new staging attempt without releasing ownership.
    pub fn reset_bytes(&self, id: &str, total: u64) {
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id).filter(|entry| entry.active) {
                entry.progress.total = total;
                entry.progress.loaded = 0;
            }
        }
    }

    pub fn pause(&self, id: &str) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard
            .get_mut(id)
            .ok_or_else(|| TransferError::NotFound(id.to_string()))?;
        if !entry.progress.done && !entry.progress.cancelled && !entry.progress.committing {
            entry.progress.paused = true;
            entry.notify.notify_waiters();
        }
        Ok(entry.progress.clone())
    }

    pub fn resume(&self, id: &str) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard
            .get_mut(id)
            .ok_or_else(|| TransferError::NotFound(id.to_string()))?;
        if entry.progress.cancelled {
            return Err(TransferError::Cancelled);
        }
        entry.progress.paused = false;
        entry.notify.notify_waiters();
        Ok(entry.progress.clone())
    }

    pub fn cancel(
        &self,
        id: &str,
        reason: Option<&str>,
    ) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard
            .entry(id.to_string())
            .or_insert_with(|| TransferEntry {
                progress: TransferProgress::new(id.to_string(), 0),
                active: false,
                notify: Arc::new(Notify::new()),
            });
        if entry.progress.committing || entry.progress.done {
            return Ok(entry.progress.clone());
        }
        entry.progress.cancelled = true;
        entry.progress.paused = false;
        entry.progress.done = !entry.active;
        entry.progress.error = reason.unwrap_or("transfer cancelled").to_string();
        entry.notify.notify_waiters();
        Ok(entry.progress.clone())
    }

    /// Linearization point between cancellation and irreversible publication.
    pub async fn begin_commit(&self, id: &str) -> Result<(), TransferError> {
        if id.is_empty() {
            return Ok(());
        }
        loop {
            self.wait_if_paused(id).await?;
            let mut guard = self
                .items
                .lock()
                .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
            let entry = guard
                .get_mut(id)
                .ok_or_else(|| TransferError::NotFound(id.into()))?;
            if entry.progress.cancelled {
                return Err(TransferError::Cancelled);
            }
            if !entry.active || entry.progress.committing {
                return Err(TransferError::AlreadyRunning(id.into()));
            }
            if !entry.progress.paused {
                entry.progress.committing = true;
                return Ok(());
            }
        }
    }

    pub fn reset(&self, id: &str) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let Some(entry) = guard.get_mut(id) else {
            // A worker can fail during preflight before it calls begin(). This
            // snapshot does not reserve an ID or create a phantom transfer.
            let mut progress = TransferProgress::new(id.into(), 0);
            progress.done = true;
            return Ok(progress);
        };
        if entry.active || !entry.progress.done {
            return Err(TransferError::AlreadyRunning(id.into()));
        }
        // Retain a quiescent snapshot until the new worker takes ownership.
        entry.progress = TransferProgress::new(id.into(), 0);
        entry.progress.done = true;
        Ok(entry.progress.clone())
    }

    pub fn finish(&self, id: &str, err: Option<String>) {
        if id.is_empty() {
            return;
        }
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id) {
                entry.active = false;
                entry.progress.committing = false;
                entry.progress.done = true;
                entry.progress.paused = false;
                if !entry.progress.cancelled {
                    if let Some(msg) = err {
                        entry.progress.error = msg;
                    } else if entry.progress.total > 0 {
                        entry.progress.loaded = entry.progress.total;
                    }
                }
                entry.notify.notify_waiters();
            }
        }
    }

    /// Releases an in-flight transfer for an automatic reconnect + resume retry without
    /// marking `done = true` or emitting a terminal error snapshot.
    pub fn release_for_retry(&self, id: &str) {
        if id.is_empty() {
            return;
        }
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id) {
                entry.active = false;
                entry.progress.committing = false;
                entry.progress.done = entry.progress.cancelled;
                if !entry.progress.cancelled {
                    entry.progress.done = false;
                    entry.progress.error.clear();
                }
                entry.notify.notify_waiters();
            }
        }
    }

    pub fn is_cancelled(&self, id: &str) -> bool {
        if id.is_empty() {
            return false;
        }
        self.items
            .lock()
            .ok()
            .and_then(|g| g.get(id).map(|e| e.progress.cancelled))
            .unwrap_or(false)
    }

    /// Waits for cancellation of a registered transfer. Use alongside remote
    /// I/O so a stalled operation does not delay a user's cancel request.
    pub async fn wait_for_cancel(&self, id: &str) {
        let notify = self
            .items
            .lock()
            .ok()
            .and_then(|items| items.get(id).map(|entry| Arc::clone(&entry.notify)));
        let Some(notify) = notify else {
            return std::future::pending().await;
        };
        loop {
            let notified = notify.notified();
            tokio::pin!(notified);
            // Register before the state check; cancel uses notify_waiters().
            notified.as_mut().enable();
            if self.is_cancelled(id) {
                return;
            }
            notified.await;
        }
    }

    /// Awaits while the transfer is paused; immediately errors if cancelled.
    pub async fn wait_if_paused(&self, id: &str) -> Result<(), TransferError> {
        if id.is_empty() {
            return Ok(());
        }
        loop {
            let notify = {
                let guard = self
                    .items
                    .lock()
                    .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
                let Some(entry) = guard.get(id) else {
                    return Ok(());
                };
                if entry.progress.cancelled {
                    return Err(TransferError::Cancelled);
                }
                if !entry.progress.paused {
                    return Ok(());
                }
                Arc::clone(&entry.notify)
            };
            let _ = tokio::time::timeout(Duration::from_millis(250), notify.notified()).await;
        }
    }

    pub fn get(&self, id: &str) -> Option<TransferProgress> {
        self.items
            .lock()
            .ok()
            .and_then(|g| g.get(id).map(|e| e.progress.clone()))
    }

    pub fn list_all(&self) -> Vec<TransferProgress> {
        self.items
            .lock()
            .ok()
            .map(|g| g.values().map(|e| e.progress.clone()).collect())
            .unwrap_or_default()
    }

    pub fn active_count(&self) -> usize {
        self.items
            .lock()
            .ok()
            .map(|g| g.values().filter(|e| e.active && !e.progress.done).count())
            .unwrap_or(0)
    }

    /// Cancels all active or unfinished transfers and wakes any paused waiters.
    pub fn cancel_all(&self, reason: Option<&str>) -> usize {
        let Ok(mut guard) = self.items.lock() else {
            return 0;
        };
        let msg = reason.unwrap_or("transfer cancelled");
        let mut count = 0;
        for entry in guard.values_mut() {
            if !entry.progress.committing && !entry.progress.done && !entry.progress.cancelled {
                entry.progress.cancelled = true;
                entry.progress.paused = false;
                entry.progress.done = !entry.active;
                if entry.progress.error.is_empty() {
                    entry.progress.error = msg.to_string();
                }
                entry.notify.notify_waiters();
                count += 1;
            }
        }
        count
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reset_after_failed_preflight_does_not_insert_a_transfer() {
        let store = TransferProgressStore::new();
        let reset = store.reset("preflight").unwrap();
        let mut expected = TransferProgress::new("preflight".into(), 0);
        expected.done = true;
        assert_eq!(reset, expected);
        assert_eq!(store.reset("preflight").unwrap(), expected);
        assert!(store.get("preflight").is_none());
        assert_eq!(store.begin("preflight", 12, false).unwrap(), 0);
    }

    #[test]
    fn reset_rejects_active_and_reconnecting_then_clears_completed_entry() {
        let store = TransferProgressStore::new();
        store.begin("t", 12, false).unwrap();
        store.add_loaded("t", 6);
        assert!(matches!(
            store.reset("t"),
            Err(TransferError::AlreadyRunning(_))
        ));
        store.release_for_retry("t");
        assert!(matches!(
            store.reset("t"),
            Err(TransferError::AlreadyRunning(_))
        ));
        store.finish("t", Some("failed".into()));
        let mut expected = TransferProgress::new("t".into(), 0);
        expected.done = true;
        assert_eq!(store.reset("t").unwrap(), expected);
        assert_eq!(store.get("t"), Some(expected));
    }

    #[tokio::test]
    async fn cancellation_prevents_publication_until_a_quiescent_reset() {
        let store = TransferProgressStore::new();
        store.begin("t", 10, false).unwrap();
        store.add_loaded("t", 10);
        store.pause("t").unwrap();
        let cancelled = store.cancel("t", None).unwrap();
        assert!(cancelled.cancelled);
        assert!(!cancelled.done);
        assert!(matches!(
            store.begin_commit("t").await,
            Err(TransferError::Cancelled)
        ));
        assert!(store.reset("t").is_err());
        store.finish("t", None);
        let reset = store.reset("t").unwrap();
        assert!(reset.done);
        assert!(!reset.cancelled);
        assert_eq!(reset.loaded, 0);
    }

    #[tokio::test]
    async fn publication_prevents_late_cancellation_and_pause() {
        let store = TransferProgressStore::new();
        store.begin("t", 10, false).unwrap();
        store.begin_commit("t").await.unwrap();
        let snapshot = store.cancel("t", None).unwrap();
        assert!(snapshot.committing);
        assert!(!snapshot.cancelled);
        assert!(!store.pause("t").unwrap().paused);
        assert_eq!(store.cancel_all(None), 0);
        assert_eq!(store.active_count(), 1);
        store.finish("t", None);
        let committed = store.get("t").unwrap();
        assert!(committed.done);
        assert!(!committed.committing);
        assert_eq!(committed.loaded, 10);
    }

    #[tokio::test]
    async fn wait_for_cancel_handles_existing_and_new_cancellation() {
        let store = TransferProgressStore::new();
        store.begin("t", 10, false).unwrap();
        let pending = store.wait_for_cancel("t");
        tokio::pin!(pending);
        assert!(
            std::future::poll_fn(|cx| {
                std::task::Poll::Ready(std::future::Future::poll(pending.as_mut(), cx).is_pending())
            })
            .await
        );
        store.cancel("t", None).unwrap();
        tokio::time::timeout(Duration::from_secs(1), pending)
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), store.wait_for_cancel("t"))
            .await
            .unwrap();
    }

    #[test]
    fn restarted_staged_transfer_does_not_reuse_displayed_offset() {
        let store = TransferProgressStore::new();
        store.begin("t", 10, false).unwrap();
        store.add_loaded("t", 8);
        store.release_for_retry("t");
        assert_eq!(store.begin("t", 10, false).unwrap(), 0);
        assert_eq!(store.get("t").unwrap().loaded, 0);
    }

    #[test]
    fn cancel_all_keeps_active_entries_until_worker_finishes() {
        let store = TransferProgressStore::new();
        store.begin("t1", 100, false).unwrap();
        store.begin("t2", 200, false).unwrap();
        store.finish("t2", None);
        assert_eq!(store.active_count(), 1);

        let cancelled = store.cancel_all(Some("application closing"));
        assert_eq!(cancelled, 1);
        assert_eq!(store.active_count(), 1);
        assert!(!store.get("t1").unwrap().done);
        store.finish("t1", None);
        assert_eq!(store.active_count(), 0);
        let t1 = store.get("t1").unwrap();
        assert!(t1.cancelled);
        assert!(t1.done);
        assert_eq!(t1.error, "application closing");
        let t2 = store.get("t2").unwrap();
        assert!(!t2.cancelled);
        assert!(t2.done);
    }
}
