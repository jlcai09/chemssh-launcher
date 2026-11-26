#![cfg(not(target_os = "windows"))]

use std::path::PathBuf;

use crate::{DragDropOutcome, WinNativeError};

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
