#![cfg(target_os = "windows")]

use std::ffi::OsStr;
use std::os::windows::ffi::OsStrExt;
use std::path::PathBuf;
use std::sync::Mutex;

use windows::core::{implement, Result as WinResult, HRESULT};
use windows::Win32::Foundation::{
    BOOL, DRAGDROP_S_CANCEL, DRAGDROP_S_DROP, DRAGDROP_S_USEDEFAULTCURSORS, DV_E_FORMATETC,
    DV_E_TYMED, E_FAIL, E_INVALIDARG, E_NOTIMPL, HGLOBAL, HWND, POINT, S_OK,
};
use windows::Win32::System::Com::{
    IDataObject, IDataObject_Impl, IEnumFORMATETC, DVASPECT_CONTENT, FORMATETC, STGMEDIUM,
    STGMEDIUM_0, TYMED_HGLOBAL,
};
use windows::Win32::System::Memory::{GlobalAlloc, GlobalFree, GlobalLock, GlobalUnlock, GHND};
use windows::Win32::System::Ole::{
    DoDragDrop, IDropSource, IDropSource_Impl, OleInitialize, OleUninitialize, DROPEFFECT,
    DROPEFFECT_COPY, DROPEFFECT_MOVE,
};
use windows::Win32::System::SystemServices::MK_LBUTTON;
use windows::Win32::UI::Shell::{SHCreateStdEnumFmtEtc, DROPFILES};
use windows::Win32::UI::WindowsAndMessaging::{
    GetAncestor, GetCursorPos, WindowFromPoint, GA_ROOT,
};

use crate::{DragDropOutcome, WinNativeError};

const CF_UNICODETEXT: u16 = 13;
const CF_HDROP: u16 = 15;

type StageCallback = Box<dyn FnOnce() -> Result<Vec<PathBuf>, String> + Send>;

#[implement(IDropSource)]
struct SimpleDropSource;

impl IDropSource_Impl for SimpleDropSource_Impl {
    fn QueryContinueDrag(
        &self,
        escape_pressed: BOOL,
        key_state: windows::Win32::System::SystemServices::MODIFIERKEYS_FLAGS,
    ) -> HRESULT {
        if escape_pressed.as_bool() {
            return DRAGDROP_S_CANCEL;
        }
        if (key_state.0 & MK_LBUTTON.0) == 0 {
            return DRAGDROP_S_DROP;
        }
        S_OK
    }

    fn GiveFeedback(&self, _effect: DROPEFFECT) -> HRESULT {
        DRAGDROP_S_USEDEFAULTCURSORS
    }
}

/// OLE IDataObject that exposes:
/// - `CF_UNICODETEXT`: the JSON marker immediately (zero I/O) so internal panes recognise
///   the drop as an in-app move or transfer without downloading anything.
/// - `CF_HDROP`: lazily invokes `stage_fn` only when an external target (Explorer / Desktop)
///   actually requests the files and the pointer is outside the launcher window.
#[implement(IDataObject)]
struct DelayedFileDataObject {
    owner_root: isize,
    marker_utf16: Vec<u16>,
    stage_fn: Mutex<Option<StageCallback>>,
    staged: Mutex<Option<Result<Vec<PathBuf>, String>>>,
}

impl DelayedFileDataObject {
    fn new(owner_hwnd: HWND, marker: &str, stage_fn: StageCallback) -> Self {
        let owner_root = if owner_hwnd.0.is_null() {
            0
        } else {
            let root = unsafe { GetAncestor(owner_hwnd, GA_ROOT) };
            if root.0.is_null() {
                owner_hwnd.0 as isize
            } else {
                root.0 as isize
            }
        };
        let marker_utf16 = if marker.is_empty() {
            Vec::new()
        } else {
            OsStr::new(marker)
                .encode_wide()
                .chain(std::iter::once(0))
                .collect()
        };
        Self {
            owner_root,
            marker_utf16,
            stage_fn: Mutex::new(Some(stage_fn)),
            staged: Mutex::new(None),
        }
    }

    fn pointer_is_over_owner(&self) -> bool {
        if self.owner_root == 0 {
            return false;
        }
        let mut pt = POINT::default();
        if unsafe { GetCursorPos(&mut pt) }.is_err() {
            return false;
        }
        let hit = unsafe { WindowFromPoint(pt) };
        if hit.0.is_null() {
            return false;
        }
        let root = unsafe { GetAncestor(hit, GA_ROOT) };
        (root.0 as isize) == self.owner_root || (hit.0 as isize) == self.owner_root
    }

    fn ensure_staged(&self) -> Result<Vec<PathBuf>, String> {
        {
            let guard = self.staged.lock().map_err(|_| "staged lock poisoned".to_string())?;
            if let Some(res) = guard.as_ref() {
                return res.clone();
            }
        }
        let cb = self
            .stage_fn
            .lock()
            .map_err(|_| "stage_fn lock poisoned".to_string())?
            .take();
        let result = match cb {
            Some(f) => f(),
            None => Err("staging callback already consumed".into()),
        };
        if let Ok(mut guard) = self.staged.lock() {
            *guard = Some(result.clone());
        }
        result
    }
}

fn alloc_hglobal_bytes(bytes: &[u8]) -> WinResult<HGLOBAL> {
    unsafe {
        let hmem = GlobalAlloc(GHND, bytes.len())?;
        let ptr = GlobalLock(hmem) as *mut u8;
        if ptr.is_null() {
            let _ = GlobalFree(Some(hmem));
            return Err(E_FAIL.into());
        }
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), ptr, bytes.len());
        let _ = GlobalUnlock(hmem);
        Ok(hmem)
    }
}

fn build_hdrop_medium(paths: &[PathBuf]) -> WinResult<STGMEDIUM> {
    let mut wide_list: Vec<u16> = Vec::new();
    for path in paths {
        wide_list.extend(path.as_os_str().encode_wide());
        wide_list.push(0);
    }
    wide_list.push(0); // double-NUL terminator

    let header_size = std::mem::size_of::<DROPFILES>();
    let data_bytes = wide_list.len() * std::mem::size_of::<u16>();
    let mut buf = vec![0u8; header_size + data_bytes];

    let df = DROPFILES {
        pFiles: header_size as u32,
        pt: POINT::default(),
        fNC: BOOL(0),
        fWide: BOOL(1),
    };
    unsafe {
        std::ptr::copy_nonoverlapping(
            (&df as *const DROPFILES) as *const u8,
            buf.as_mut_ptr(),
            header_size,
        );
        std::ptr::copy_nonoverlapping(
            wide_list.as_ptr() as *const u8,
            buf.as_mut_ptr().add(header_size),
            data_bytes,
        );
    }
    let hglobal = alloc_hglobal_bytes(&buf)?;
    Ok(STGMEDIUM {
        tymed: TYMED_HGLOBAL.0 as u32,
        u: STGMEDIUM_0 { hGlobal: hglobal },
        pUnkForRelease: std::mem::ManuallyDrop::new(None),
    })
}

fn build_unicode_medium(wide: &[u16]) -> WinResult<STGMEDIUM> {
    let byte_len = wide.len() * std::mem::size_of::<u16>();
    let bytes = unsafe { std::slice::from_raw_parts(wide.as_ptr() as *const u8, byte_len) };
    let hglobal = alloc_hglobal_bytes(bytes)?;
    Ok(STGMEDIUM {
        tymed: TYMED_HGLOBAL.0 as u32,
        u: STGMEDIUM_0 { hGlobal: hglobal },
        pUnkForRelease: std::mem::ManuallyDrop::new(None),
    })
}

impl IDataObject_Impl for DelayedFileDataObject_Impl {
    fn GetData(&self, pformatetcin: *const FORMATETC) -> WinResult<STGMEDIUM> {
        let Some(fmt) = (unsafe { pformatetcin.as_ref() }) else {
            return Err(E_INVALIDARG.into());
        };
        if (fmt.tymed & TYMED_HGLOBAL.0 as u32) == 0 {
            return Err(DV_E_TYMED.into());
        }
        if fmt.cfFormat == CF_UNICODETEXT && !self.marker_utf16.is_empty() {
            return build_unicode_medium(&self.marker_utf16);
        }
        if fmt.cfFormat == CF_HDROP {
            // Never download while the pointer is still inside the launcher window:
            // internal panes read CF_UNICODETEXT instead.
            if self.pointer_is_over_owner() {
                return Err(E_FAIL.into());
            }
            let paths = self.ensure_staged().map_err(|_| windows::core::Error::from(E_FAIL))?;
            if paths.is_empty() {
                return Err(E_FAIL.into());
            }
            return build_hdrop_medium(&paths);
        }
        Err(DV_E_FORMATETC.into())
    }

    fn GetDataHere(&self, _pformatetc: *const FORMATETC, _pmedium: *mut STGMEDIUM) -> WinResult<()> {
        Err(E_NOTIMPL.into())
    }

    fn QueryGetData(&self, pformatetc: *const FORMATETC) -> HRESULT {
        let Some(fmt) = (unsafe { pformatetc.as_ref() }) else {
            return E_INVALIDARG;
        };
        if (fmt.tymed & TYMED_HGLOBAL.0 as u32) == 0 {
            return DV_E_TYMED;
        }
        if fmt.cfFormat == CF_HDROP {
            return S_OK;
        }
        if fmt.cfFormat == CF_UNICODETEXT && !self.marker_utf16.is_empty() {
            return S_OK;
        }
        DV_E_FORMATETC
    }

    fn GetCanonicalFormatEtc(
        &self,
        _pformatectin: *const FORMATETC,
        pformatetcout: *mut FORMATETC,
    ) -> HRESULT {
        if let Some(out) = unsafe { pformatetcout.as_mut() } {
            out.ptd = std::ptr::null_mut();
        }
        windows::Win32::Foundation::DATA_S_SAMEFORMATETC
    }

    fn SetData(
        &self,
        _pformatetc: *const FORMATETC,
        _pmedium: *const STGMEDIUM,
        _frelease: BOOL,
    ) -> WinResult<()> {
        Err(E_NOTIMPL.into())
    }

    fn EnumFormatEtc(&self, dwdirection: u32) -> WinResult<IEnumFORMATETC> {
        if dwdirection != windows::Win32::System::Com::DATADIR_GET.0 as u32 {
            return Err(E_NOTIMPL.into());
        }
        let mut formats = vec![FORMATETC {
            cfFormat: CF_HDROP,
            ptd: std::ptr::null_mut(),
            dwAspect: DVASPECT_CONTENT.0,
            lindex: -1,
            tymed: TYMED_HGLOBAL.0 as u32,
        }];
        if !self.marker_utf16.is_empty() {
            formats.push(FORMATETC {
                cfFormat: CF_UNICODETEXT,
                ptd: std::ptr::null_mut(),
                dwAspect: DVASPECT_CONTENT.0,
                lindex: -1,
                tymed: TYMED_HGLOBAL.0 as u32,
            });
        }
        unsafe { SHCreateStdEnumFmtEtc(&formats) }
    }

    fn DAdvise(
        &self,
        _pformatetc: *const FORMATETC,
        _advf: u32,
        _padvsink: Option<&windows::Win32::System::Com::IAdviseSink>,
    ) -> WinResult<u32> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
    }

    fn DUnadvise(&self, _dwconnection: u32) -> WinResult<()> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
    }

    fn EnumDAdvise(&self) -> WinResult<windows::Win32::System::Com::IEnumSTATDATA> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
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

/// Initiates a native Windows Shell OLE file drag-out with delayed staging:
/// `stage_fn` is called only if an external window (Desktop / Explorer) actually
/// accepts and requests the files.
pub fn start_shell_file_drag_delayed<F>(
    hwnd_raw: isize,
    marker: &str,
    stage_fn: F,
) -> Result<DragDropOutcome, WinNativeError>
where
    F: FnOnce() -> Result<Vec<PathBuf>, String> + Send + 'static,
{
    let _ole = OleGuard::init()?;
    let hwnd = HWND(hwnd_raw as *mut _);
    let data_object: IDataObject =
        DelayedFileDataObject::new(hwnd, marker, Box::new(stage_fn)).into();
    let drop_source: IDropSource = SimpleDropSource.into();
    let mut performed_effect = DROPEFFECT(0);

    let hr = unsafe {
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

/// Convenience wrapper when local paths are already staged.
pub fn start_shell_file_drag(
    hwnd_raw: isize,
    staged_paths: &[PathBuf],
) -> Result<DragDropOutcome, WinNativeError> {
    if staged_paths.is_empty() {
        return Err(WinNativeError::Platform(
            "no staged paths provided for OLE drag".into(),
        ));
    }
    let paths = staged_paths.to_vec();
    start_shell_file_drag_delayed(hwnd_raw, "", move || Ok(paths))
}
