#![cfg(not(target_os = "windows"))]

use std::path::PathBuf;

use crate::{AsyncDragEvent, DragDropOutcome, NativeDragGestureResult, WinNativeError};

pub fn start_shell_file_drag(
    _hwnd_raw: isize,
    _staged_paths: &[PathBuf],
) -> Result<DragDropOutcome, WinNativeError> {
    Err(WinNativeError::UnsupportedPlatform)
}

pub fn start_shell_file_drag_delayed<F>(
    _hwnd_raw: isize,
    _marker: &str,
    _stage_fn: F,
) -> Result<DragDropOutcome, WinNativeError>
where
    F: FnOnce() -> Result<Vec<PathBuf>, String> + Send + 'static,
{
    Err(WinNativeError::UnsupportedPlatform)
}

pub fn start_shell_file_drag_async<S, N, C>(
    _hwnd_raw: isize,
    _marker: &str,
    _stage_fn: S,
    _notify_fn: N,
    _is_cancelled_fn: C,
) -> Result<NativeDragGestureResult, WinNativeError>
where
    S: FnOnce() -> Result<Vec<PathBuf>, String> + Send + 'static,
    N: Fn(AsyncDragEvent) + Send + Sync + 'static,
    C: Fn() -> bool + Send + Sync + 'static,
{
    Err(WinNativeError::UnsupportedPlatform)
}
