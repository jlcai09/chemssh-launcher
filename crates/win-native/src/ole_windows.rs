#![cfg(target_os = "windows")]

use std::path::PathBuf;

use windows::core::{implement, Result as WinResult, HSTRING, PCWSTR};
use windows::Win32::Foundation::{
    BOOL, DRAGDROP_S_CANCEL, DRAGDROP_S_DROP, DRAGDROP_S_USEDEFAULTCURSORS, E_INVALIDARG, HWND,
    S_OK,
};
use windows::Win32::System::Com::IDataObject;
use windows::Win32::System::Ole::{
    DoDragDrop, IDropSource, IDropSource_Impl, OleInitialize, OleUninitialize, DROPEFFECT,
    DROPEFFECT_COPY, DROPEFFECT_MOVE,
};
use windows::Win32::System::SystemServices::MK_LBUTTON;
use windows::Win32::UI::Shell::{ILFree, SHCreateDataObject, SHParseDisplayName};

use crate::{DragDropOutcome, WinNativeError};

#[implement(IDropSource)]
struct SimpleDropSource;

impl IDropSource_Impl for SimpleDropSource_Impl {
    fn QueryContinueDrag(
        &self,
        escape_pressed: BOOL,
        key_state: windows::Win32::System::SystemServices::MODIFIERKEYS_FLAGS,
    ) -> windows::core::HRESULT {
        if escape_pressed.as_bool() {
            return DRAGDROP_S_CANCEL;
        }
        if (key_state.0 & MK_LBUTTON.0) == 0 {
            return DRAGDROP_S_DROP;
        }
        S_OK
    }

    fn GiveFeedback(&self, _effect: DROPEFFECT) -> windows::core::HRESULT {
        DRAGDROP_S_USEDEFAULTCURSORS
    }
}

struct OleGuard;

impl OleGuard {
    fn init() -> Result<Self, WinNativeError> {
        unsafe {
            OleInitialize(None)
                .map_err(|e| WinNativeError::Platform(format!("OleInitialize failed: {e}")))?;
        }
        Ok(Self)
    }
}

impl Drop for OleGuard {
    fn drop(&mut self) {
        unsafe {
            OleUninitialize();
        }
    }
}

/// Initiates a native Windows Shell OLE file drag-out for the staged local files/folders.
pub fn start_shell_file_drag(
    hwnd_raw: isize,
    staged_paths: &[PathBuf],
) -> Result<DragDropOutcome, WinNativeError> {
    if staged_paths.is_empty() {
        return Err(WinNativeError::Platform(
            "no staged paths provided for OLE drag".into(),
        ));
    }

    let _ole = OleGuard::init()?;
    let hwnd = HWND(hwnd_raw as *mut _);

    let mut pidls = Vec::with_capacity(staged_paths.len());
    for path in staged_paths {
        let wide = HSTRING::from(path.as_os_str());
        let mut pidl = std::ptr::null_mut();
        let hr: WinResult<()> = unsafe {
            SHParseDisplayName(PCWSTR::from_raw(wide.as_ptr()), None, &mut pidl, 0, None)
        };
        if let Err(err) = hr {
            for p in pidls {
                unsafe { ILFree(Some(p)) };
            }
            return Err(WinNativeError::Platform(format!(
                "SHParseDisplayName failed for {}: {err}",
                path.display()
            )));
        }
        pidls.push(pidl as *const _);
    }

    let data_obj_res: WinResult<IDataObject> =
        unsafe { SHCreateDataObject(None, Some(&pidls), None) };

    for p in &pidls {
        unsafe { ILFree(Some(*p)) };
    }

    let data_object = data_obj_res
        .map_err(|e| WinNativeError::Platform(format!("SHCreateDataObject failed: {e}")))?;
    let drop_source: IDropSource = SimpleDropSource.into();
    let mut performed_effect = DROPEFFECT(0);

    let hr = unsafe {
        let _ = hwnd;
        DoDragDrop(
            &data_object,
            &drop_source,
            DROPEFFECT_COPY | DROPEFFECT_MOVE,
            &mut performed_effect,
        )
    };

    if hr == DRAGDROP_S_DROP && performed_effect.0 != 0 {
        Ok(DragDropOutcome::Dropped)
    } else if hr == DRAGDROP_S_CANCEL || hr == DRAGDROP_S_DROP {
        Ok(DragDropOutcome::Cancelled)
    } else if hr == E_INVALIDARG {
        Err(WinNativeError::Platform("DoDragDrop invalid arguments".into()))
    } else {
        Err(WinNativeError::Platform(format!("DoDragDrop returned {hr:?}")))
    }
}
