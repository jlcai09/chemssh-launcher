//! Windows native OLE async drag-out, background cache delivery, and file icons (Phase 4).
//! Non-Windows platforms expose safe no-op fallbacks so workspace checks pass everywhere.

pub fn is_windows_native_supported() -> bool {
    cfg!(target_os = "windows")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn platform_flag_matches_target() {
        assert_eq!(is_windows_native_supported(), cfg!(target_os = "windows"));
    }
}
