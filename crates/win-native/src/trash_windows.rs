use std::path::Path;

#[cfg(target_os = "windows")]
pub fn move_to_trash(path: &Path) -> Result<(), String> {
    use windows::core::PCWSTR;
    use windows::Win32::UI::Shell::{
        SHFileOperationW, FOF_ALLOWUNDO, FOF_NOCONFIRMATION, FOF_NOERRORUI, FOF_SILENT, FO_DELETE,
        SHFILEOPSTRUCTW,
    };

    let abs = std::path::absolute(path)
        .map_err(|e| format!("resolve trash path {}: {e}", path.display()))?;
    let raw = abs.to_string_lossy();
    let clean = if let Some(unc) = raw.strip_prefix(r"\\?\UNC\") {
        format!(r"\\{unc}")
    } else if let Some(rest) = raw.strip_prefix(r"\\?\") {
        rest.to_string()
    } else {
        raw.into_owned()
    };

    if clean.is_empty() || clean.contains('\0') {
        return Err("invalid path for recycle bin".into());
    }

    let mut wide: Vec<u16> = clean.encode_utf16().collect();
    wide.push(0);
    wide.push(0);

    let mut op = SHFILEOPSTRUCTW {
        hwnd: Default::default(),
        wFunc: FO_DELETE,
        pFrom: PCWSTR(wide.as_ptr()),
        pTo: PCWSTR::null(),
        fFlags: (FOF_ALLOWUNDO | FOF_NOCONFIRMATION | FOF_SILENT | FOF_NOERRORUI).0 as u16,
        fAnyOperationsAborted: false.into(),
        hNameMappings: core::ptr::null_mut(),
        lpszProgressTitle: PCWSTR::null(),
    };

    let code = unsafe { SHFileOperationW(&mut op) };
    if code != 0 || op.fAnyOperationsAborted.as_bool() {
        return Err(format!(
            "SHFileOperationW recycle bin delete failed with code {code}"
        ));
    }
    Ok(())
}

#[cfg(not(target_os = "windows"))]
pub fn move_to_trash(_path: &Path) -> Result<(), String> {
    Err("recycle bin is only supported on Windows".into())
}
