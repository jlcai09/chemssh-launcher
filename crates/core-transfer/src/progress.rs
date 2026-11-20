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
        let entry = guard.entry(id.to_string()).or_insert_with(|| TransferEntry {
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

    pub fn pause(&self, id: &str) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard
            .get_mut(id)
            .ok_or_else(|| TransferError::NotFound(id.to_string()))?;
        if !entry.progress.done && !entry.progress.cancelled {
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

    pub fn cancel(&self, id: &str, reason: Option<&str>) -> Result<TransferProgress, TransferError> {
        let mut guard = self
            .items
            .lock()
            .map_err(|_| TransferError::Internal("transfer store lock poisoned".into()))?;
        let entry = guard.entry(id.to_string()).or_insert_with(|| TransferEntry {
            progress: TransferProgress::new(id.to_string(), 0),
            active: false,
            notify: Arc::new(Notify::new()),
        });
        entry.progress.cancelled = true;
        entry.progress.paused = false;
        entry.progress.done = true;
        entry.active = false;
        entry.progress.error = reason.unwrap_or("transfer cancelled").to_string();
        entry.notify.notify_waiters();
        Ok(entry.progress.clone())
    }

    pub fn finish(&self, id: &str, err: Option<String>) {
        if id.is_empty() {
            return;
        }
        if let Ok(mut guard) = self.items.lock() {
            if let Some(entry) = guard.get_mut(id) {
                entry.active = false;
                entry.progress.done = true;
                entry.progress.paused = false;
                if let Some(msg) = err {
                    entry.progress.error = msg;
                } else if !entry.progress.cancelled && entry.progress.total > 0 {
                    entry.progress.loaded = entry.progress.total;
                }
                entry.notify.notify_waiters();
            }
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
}
