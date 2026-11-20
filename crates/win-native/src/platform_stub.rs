#![cfg(not(target_os = "windows"))]

use std::path::PathBuf;

use crate::{DragDropOutcome, WinNativeError};

pub fn start_shell_file_drag(
    _hwnd_raw: isize,
    _staged_paths: &[PathBuf],
) -> Result<DragDropOutcome, WinNativeError> {
    Err(WinNativeError::UnsupportedPlatform)
}
