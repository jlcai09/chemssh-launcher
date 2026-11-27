#![cfg(target_os = "windows")]

use std::ffi::{c_void, OsStr};
use std::mem::ManuallyDrop;
use std::os::windows::ffi::OsStrExt;
use std::path::PathBuf;
use std::ptr::NonNull;
use std::sync::atomic::{fence, AtomicBool, AtomicI32, AtomicU32, Ordering};
use std::sync::{mpsc, Arc, Condvar, Mutex, OnceLock};
use std::time::{Duration, Instant};

use windows::core::{
    implement, ComObject, ComObjectInner, IUnknown, IUnknownImpl, Interface, Result as WinResult,
    GUID, HRESULT, PCWSTR,
};
use windows::Win32::Foundation::{
    GlobalFree, BOOL, DATA_S_SAMEFORMATETC, DRAGDROP_S_CANCEL, DRAGDROP_S_DROP,
    DRAGDROP_S_USEDEFAULTCURSORS, DV_E_DVASPECT, DV_E_FORMATETC, DV_E_LINDEX, DV_E_TYMED, E_ABORT,
    E_FAIL, E_INVALIDARG, E_NOINTERFACE, E_NOTIMPL, E_OUTOFMEMORY, E_POINTER, HGLOBAL, POINT, S_OK,
};
use windows::Win32::System::Com::Marshal::CoMarshalInterThreadInterfaceInStream;
use windows::Win32::System::Com::StructuredStorage::CoGetInterfaceAndReleaseStream;
use windows::Win32::System::Com::{
    CoInitializeEx, CoUninitialize, IAdviseSink, IBindCtx, IDataObject, IDataObject_Impl,
    IDataObject_Vtbl, IEnumFORMATETC, IEnumSTATDATA, IStream, COINIT_MULTITHREADED, DATADIR_GET,
    DVASPECT_CONTENT, FORMATETC, STGMEDIUM, STGMEDIUM_0, TYMED_HGLOBAL,
};
use windows::Win32::System::DataExchange::RegisterClipboardFormatW;
use windows::Win32::System::Memory::{
    GlobalAlloc, GlobalLock, GlobalSize, GlobalUnlock, GHND,
};
use windows::Win32::System::Ole::{
    DoDragDrop, IDropSource, IDropSource_Impl, OleInitialize, OleUninitialize, ReleaseStgMedium,
    DROPEFFECT, DROPEFFECT_COPY,
};
use windows::Win32::System::SystemServices::MODIFIERKEYS_FLAGS;
use windows::Win32::UI::Input::KeyboardAndMouse::{GetAsyncKeyState, VK_LBUTTON};
use windows::Win32::UI::Shell::{
    IDataObjectAsyncCapability, IDataObjectAsyncCapability_Impl, IDataObjectAsyncCapability_Vtbl,
    SHCreateStdEnumFmtEtc, DROPFILES,
};
use windows::Win32::UI::WindowsAndMessaging::GetCursorPos;

use crate::{AsyncDragEvent, DragDropOutcome, NativeDragGestureResult, WinNativeError};

const CF_UNICODETEXT: u16 = 13;
const CF_HDROP: u16 = 15;
const DRAG_MOVE_THRESHOLD: i32 = 4;
const ASYNC_HANDOFF_TIMEOUT: Duration = Duration::from_millis(100);

type StageCallback = Box<dyn FnOnce() -> Result<Vec<PathBuf>, String> + Send>;
type NotifyCallback = Arc<dyn Fn(AsyncDragEvent) + Send + Sync>;
type CancelCheck = Arc<dyn Fn() -> bool + Send + Sync>;

fn key_down_lbutton() -> bool {
    // Physical button state: under WebView2 the browser renderer runs in another
    // process, so OLE's thread-local GRFKEYSTATE often starts as 0 (button-less).
    unsafe { GetAsyncKeyState(VK_LBUTTON.0 as i32) < 0 }
}

fn register_clipboard_format(name: &str) -> u16 {
    let wide: Vec<u16> = OsStr::new(name)
        .encode_wide()
        .chain(std::iter::once(0))
        .collect();
    unsafe { RegisterClipboardFormatW(PCWSTR::from_raw(wide.as_ptr())) as u16 }
}

fn preferred_drop_effect_format() -> u16 {
    static CF: OnceLock<u16> = OnceLock::new();
    *CF.get_or_init(|| register_clipboard_format("Preferred DropEffect"))
}

fn feedback_formats() -> (u16, u16) {
    static CFS: OnceLock<(u16, u16)> = OnceLock::new();
    *CFS.get_or_init(|| {
        (
            register_clipboard_format("Performed DropEffect"),
            register_clipboard_format("Logical Performed DropEffect"),
        )
    })
}

fn cursor_left_press_point(start: POINT, cur: POINT) -> bool {
    let dx = cur.x.saturating_sub(start.x);
    let dy = cur.y.saturating_sub(start.y);
    (dx as i64) * (dx as i64) + (dy as i64) * (dy as i64)
        >= (DRAG_MOVE_THRESHOLD as i64) * (DRAG_MOVE_THRESHOLD as i64)
}

#[implement(IDropSource)]
struct AsyncDropSource {
    start: POINT,
    track_move: bool,
    moved: Arc<AtomicBool>,
    cancelled: Arc<AtomicBool>,
    continues: Arc<AtomicU32>,
    feedbacks: Arc<AtomicU32>,
}

impl AsyncDropSource {
    fn mark_movement(&self) {
        if self.moved.load(Ordering::Acquire) {
            return;
        }
        if !self.track_move {
            self.moved.store(true, Ordering::Release);
            return;
        }
        let mut pt = POINT::default();
        if unsafe { GetCursorPos(&mut pt) }.is_err() {
            self.moved.store(true, Ordering::Release);
            return;
        }
        if cursor_left_press_point(self.start, pt) {
            self.moved.store(true, Ordering::Release);
        }
    }
}

impl IDropSource_Impl for AsyncDropSource_Impl {
    fn QueryContinueDrag(&self, escape_pressed: BOOL, _key_state: MODIFIERKEYS_FLAGS) -> HRESULT {
        self.continues.fetch_add(1, Ordering::Relaxed);
        self.mark_movement();
        if escape_pressed.as_bool() {
            self.cancelled.store(true, Ordering::Release);
            return DRAGDROP_S_CANCEL;
        }
        if key_down_lbutton() {
            return S_OK;
        }
        if self.moved.load(Ordering::Acquire) {
            DRAGDROP_S_DROP
        } else {
            DRAGDROP_S_CANCEL
        }
    }

    fn GiveFeedback(&self, _effect: DROPEFFECT) -> HRESULT {
        self.feedbacks.fetch_add(1, Ordering::Relaxed);
        self.mark_movement();
        DRAGDROP_S_USEDEFAULTCURSORS
    }
}

struct AsyncInner {
    enabled: bool,
    started: bool,
    terminal: bool,
    loop_done: bool,
    final_event: Option<AsyncDragEvent>,
    held_self: usize,
    stage_fn: Option<StageCallback>,
    stage_started: bool,
    stage_done: bool,
    staged: Option<Result<Vec<PathBuf>, String>>,
}

struct AsyncShared {
    inner: Mutex<AsyncInner>,
    cvar: Condvar,
    notify: NotifyCallback,
    is_cancelled: CancelCheck,
}

impl AsyncShared {
    fn new(stage_fn: StageCallback, notify: NotifyCallback, is_cancelled: CancelCheck) -> Self {
        Self {
            inner: Mutex::new(AsyncInner {
                enabled: true,
                started: false,
                terminal: false,
                loop_done: false,
                final_event: None,
                held_self: 0,
                stage_fn: Some(stage_fn),
                stage_started: false,
                stage_done: false,
                staged: None,
            }),
            cvar: Condvar::new(),
            notify,
            is_cancelled,
        }
    }

    fn mark_loop_done(&self) {
        if let Ok(mut guard) = self.inner.lock() {
            guard.loop_done = true;
            self.cvar.notify_all();
        }
    }

    fn finish(&self, mut event: AsyncDragEvent) {
        let held_ptr = {
            let Ok(mut guard) = self.inner.lock() else {
                return;
            };
            if guard.terminal {
                return;
            }
            if matches!(event, AsyncDragEvent::Done { .. }) && (self.is_cancelled)() {
                event = AsyncDragEvent::Cancelled {
                    reason: Some("drag cancelled".into()),
                };
            }
            guard.terminal = true;
            guard.final_event = Some(event.clone());
            let ptr = guard.held_self;
            guard.held_self = 0;
            self.cvar.notify_all();
            ptr
        };

        (self.notify)(event);

        if held_ptr != 0 {
            unsafe {
                NonAgileDragObjectBox::Release(held_ptr as *mut NonAgileDragObjectBox);
            }
        }
    }

    fn snapshot(&self) -> (bool, bool, Option<AsyncDragEvent>) {
        self.inner
            .lock()
            .map(|g| (g.started, g.terminal, g.final_event.clone()))
            .unwrap_or((false, true, None))
    }
}

struct AsyncDragDataObject {
    marker_utf16: Vec<u16>,
    shared: Arc<AsyncShared>,
    released_tx: Mutex<Option<mpsc::Sender<()>>>,
}

impl Drop for AsyncDragDataObject {
    fn drop(&mut self) {
        self.shared
            .finish(AsyncDragEvent::Cancelled { reason: None });
        if let Ok(mut guard) = self.released_tx.lock() {
            if let Some(tx) = guard.take() {
                let _ = tx.send(());
            }
        }
    }
}

/// Non-agile COM object implementing `IDataObject` and `IDataObjectAsyncCapability`
/// without advertising `IAgileObject`.
///
/// Created on an MTA thread and marshaled to the UI STA thread via
/// `CoMarshalInterThreadInterfaceInStream` / `CoGetInterfaceAndReleaseStream`.
/// Because `IAgileObject` is intentionally omitted, standard COM marshaling
/// dispatches Explorer's background `GetData(CF_HDROP)` onto our MTA apartment
/// instead of blocking the WebView2 UI thread.
#[repr(C)]
struct NonAgileDragObjectBox {
    vtbl_data: &'static IDataObject_Vtbl,
    vtbl_async: &'static IDataObjectAsyncCapability_Vtbl,
    this: AsyncDragDataObject,
    count: AtomicI32,
}

impl NonAgileDragObjectBox {
    const VTBL_DATA: IDataObject_Vtbl = IDataObject_Vtbl::new::<Self, 0>();
    const VTBL_ASYNC: IDataObjectAsyncCapability_Vtbl =
        IDataObjectAsyncCapability_Vtbl::new::<Self, -1>();

    fn into_data_object(inner: AsyncDragDataObject) -> IDataObject {
        let boxed = Box::new(Self {
            vtbl_data: &Self::VTBL_DATA,
            vtbl_async: &Self::VTBL_ASYNC,
            this: inner,
            count: AtomicI32::new(1),
        });
        let raw = Box::into_raw(boxed) as *mut c_void;
        unsafe { IDataObject::from_raw(raw) }
    }
}

impl ComObjectInner for AsyncDragDataObject {
    type Outer = NonAgileDragObjectBox;

    fn into_object(self) -> ComObject<Self> {
        let boxed = Box::new(NonAgileDragObjectBox {
            vtbl_data: &NonAgileDragObjectBox::VTBL_DATA,
            vtbl_async: &NonAgileDragObjectBox::VTBL_ASYNC,
            this: self,
            count: AtomicI32::new(1),
        });
        unsafe { ComObject::from_raw(NonNull::new_unchecked(Box::into_raw(boxed))) }
    }
}

impl IUnknownImpl for NonAgileDragObjectBox {
    type Impl = AsyncDragDataObject;

    fn get_impl(&self) -> &Self::Impl {
        &self.this
    }

    fn get_impl_mut(&mut self) -> &mut Self::Impl {
        &mut self.this
    }

    fn into_inner(self) -> Self::Impl {
        self.this
    }

    unsafe fn QueryInterface(&self, iid: *const GUID, interface: *mut *mut c_void) -> HRESULT {
        if iid.is_null() || interface.is_null() {
            return E_POINTER;
        }
        let iid = &*iid;
        if *iid == IUnknown::IID || *iid == IDataObject::IID {
            *interface = (&self.vtbl_data as *const _) as *mut c_void;
            self.AddRef();
            return S_OK;
        }
        if *iid == IDataObjectAsyncCapability::IID {
            *interface = (&self.vtbl_async as *const _) as *mut c_void;
            self.AddRef();
            return S_OK;
        }
        *interface = std::ptr::null_mut();
        E_NOINTERFACE
    }

    fn AddRef(&self) -> u32 {
        (self.count.fetch_add(1, Ordering::Relaxed) + 1) as u32
    }

    unsafe fn Release(self_: *mut Self) -> u32 {
        let remaining = (*self_).count.fetch_sub(1, Ordering::Release) - 1;
        if remaining == 0 {
            fence(Ordering::Acquire);
            drop(Box::from_raw(self_));
        }
        remaining as u32
    }

    fn is_reference_count_one(&self) -> bool {
        self.count.load(Ordering::Acquire) == 1
    }

    unsafe fn GetTrustLevel(&self, value: *mut i32) -> HRESULT {
        if value.is_null() {
            return E_POINTER;
        }
        *value = 0;
        S_OK
    }

    unsafe fn from_inner_ref(inner: &Self::Impl) -> &Self {
        &*((inner as *const AsyncDragDataObject as *const *const c_void).sub(2) as *const Self)
    }

    fn to_object(&self) -> ComObject<Self::Impl>
    where
        Self::Impl: ComObjectInner<Outer = Self>,
    {
        self.AddRef();
        unsafe { ComObject::from_raw(NonNull::new_unchecked(self as *const Self as *mut Self)) }
    }

    const INNER_OFFSET_IN_POINTERS: usize = 2;
}

fn alloc_hglobal_bytes(bytes: &[u8]) -> WinResult<HGLOBAL> {
    unsafe {
        let hmem = GlobalAlloc(GHND, bytes.len())?;
        let ptr = GlobalLock(hmem) as *mut u8;
        if ptr.is_null() {
            let _ = GlobalFree(hmem);
            return Err(E_OUTOFMEMORY.into());
        }
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), ptr, bytes.len());
        let _ = GlobalUnlock(hmem);
        Ok(hmem)
    }
}

fn build_hglobal_medium(bytes: &[u8]) -> WinResult<STGMEDIUM> {
    let hglobal = alloc_hglobal_bytes(bytes)?;
    Ok(STGMEDIUM {
        tymed: TYMED_HGLOBAL.0 as u32,
        u: STGMEDIUM_0 { hGlobal: hglobal },
        pUnkForRelease: ManuallyDrop::new(None),
    })
}

fn build_hdrop_medium(paths: &[PathBuf]) -> WinResult<STGMEDIUM> {
    let mut wide_list: Vec<u16> = Vec::new();
    for path in paths {
        wide_list.extend(path.as_os_str().encode_wide());
        wide_list.push(0);
    }
    wide_list.push(0);

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
    build_hglobal_medium(&buf)
}

fn build_unicode_medium(wide: &[u16]) -> WinResult<STGMEDIUM> {
    let byte_len = wide.len() * std::mem::size_of::<u16>();
    let bytes = unsafe { std::slice::from_raw_parts(wide.as_ptr() as *const u8, byte_len) };
    build_hglobal_medium(bytes)
}

fn build_drop_effect_medium(effect: u32) -> WinResult<STGMEDIUM> {
    build_hglobal_medium(&effect.to_ne_bytes())
}

impl NonAgileDragObjectBox {
    fn validate_format(&self, fmt: &FORMATETC) -> HRESULT {
        let pref = preferred_drop_effect_format();
        let supported_cf = fmt.cfFormat == CF_HDROP
            || (fmt.cfFormat == CF_UNICODETEXT && !self.this.marker_utf16.is_empty())
            || (pref != 0 && fmt.cfFormat == pref);
        if !supported_cf {
            return DV_E_FORMATETC;
        }
        if fmt.dwAspect != DVASPECT_CONTENT.0 {
            return DV_E_DVASPECT;
        }
        if fmt.lindex != -1 {
            return DV_E_LINDEX;
        }
        if (fmt.tymed & TYMED_HGLOBAL.0 as u32) == 0 {
            return DV_E_TYMED;
        }
        S_OK
    }

    fn async_paths(&self) -> WinResult<Vec<PathBuf>> {
        let shared = &self.this.shared;
        let deadline = Instant::now() + ASYNC_HANDOFF_TIMEOUT;

        let stage_cb = {
            let mut guard = shared.inner.lock().map_err(|_| E_FAIL)?;
            if !guard.started || guard.terminal {
                return Err(DV_E_FORMATETC.into());
            }
            while !guard.loop_done && !guard.terminal && !(shared.is_cancelled)() {
                let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
                    break;
                };
                let (next_guard, _) = shared
                    .cvar
                    .wait_timeout(guard, remaining)
                    .map_err(|_| E_FAIL)?;
                guard = next_guard;
            }
            if (shared.is_cancelled)() {
                return Err(E_ABORT.into());
            }
            if !guard.loop_done {
                return Err(DV_E_FORMATETC.into());
            }
            if guard.terminal {
                return Err(E_ABORT.into());
            }
            if !guard.stage_started {
                guard.stage_started = true;
                guard.stage_fn.take()
            } else {
                None
            }
        };

        if let Some(cb) = stage_cb {
            let result = cb();
            if let Ok(mut guard) = shared.inner.lock() {
                guard.staged = Some(result);
                guard.stage_done = true;
                shared.cvar.notify_all();
            }
        }

        let staged = {
            let mut guard = shared.inner.lock().map_err(|_| E_FAIL)?;
            while !guard.stage_done && !guard.terminal && !(shared.is_cancelled)() {
                let (next_guard, _) = shared
                    .cvar
                    .wait_timeout(guard, Duration::from_millis(100))
                    .map_err(|_| E_FAIL)?;
                guard = next_guard;
            }
            if (shared.is_cancelled)() {
                drop(guard);
                shared.finish(AsyncDragEvent::Cancelled {
                    reason: Some("drag cancelled".into()),
                });
                return Err(E_ABORT.into());
            }
            guard.staged.clone()
        };

        match staged {
            Some(Ok(paths)) if !paths.is_empty() => Ok(paths),
            Some(Ok(_)) => {
                shared.finish(AsyncDragEvent::Error("no files staged for drop".into()));
                Err(E_FAIL.into())
            }
            Some(Err(err)) => {
                if (shared.is_cancelled)() {
                    shared.finish(AsyncDragEvent::Cancelled {
                        reason: Some("drag cancelled".into()),
                    });
                    Err(E_ABORT.into())
                } else {
                    shared.finish(AsyncDragEvent::Error(err));
                    Err(E_FAIL.into())
                }
            }
            None => Err(E_ABORT.into()),
        }
    }
}

impl IDataObject_Impl for NonAgileDragObjectBox {
    fn GetData(&self, pformatetcin: *const FORMATETC) -> WinResult<STGMEDIUM> {
        let Some(fmt) = (unsafe { pformatetcin.as_ref() }) else {
            return Err(E_INVALIDARG.into());
        };
        let hr = self.validate_format(fmt);
        if hr != S_OK {
            return Err(hr.into());
        }
        let pref = preferred_drop_effect_format();
        if fmt.cfFormat == CF_UNICODETEXT {
            return build_unicode_medium(&self.this.marker_utf16);
        }
        if pref != 0 && fmt.cfFormat == pref {
            return build_drop_effect_medium(DROPEFFECT_COPY.0);
        }
        if fmt.cfFormat == CF_HDROP {
            let paths = self.async_paths()?;
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
        self.validate_format(fmt)
    }

    fn GetCanonicalFormatEtc(
        &self,
        _pformatectin: *const FORMATETC,
        pformatetcout: *mut FORMATETC,
    ) -> HRESULT {
        if let Some(out) = unsafe { pformatetcout.as_mut() } {
            *out = FORMATETC {
                cfFormat: 0,
                ptd: std::ptr::null_mut(),
                dwAspect: 0,
                lindex: -1,
                tymed: 0,
            };
        }
        DATA_S_SAMEFORMATETC
    }

    fn SetData(
        &self,
        pformatetc: *const FORMATETC,
        pmedium: *const STGMEDIUM,
        frelease: BOOL,
    ) -> WinResult<()> {
        let (Some(fmt), Some(medium)) = (unsafe { pformatetc.as_ref() }, unsafe {
            pmedium.as_ref()
        }) else {
            return Err(E_INVALIDARG.into());
        };
        let (performed_cf, logical_cf) = feedback_formats();
        if fmt.cfFormat == 0 || (fmt.cfFormat != performed_cf && fmt.cfFormat != logical_cf) {
            return Err(E_NOTIMPL.into());
        }
        if fmt.dwAspect != DVASPECT_CONTENT.0 {
            return Err(DV_E_DVASPECT.into());
        }
        if fmt.lindex != -1 {
            return Err(DV_E_LINDEX.into());
        }
        let hglobal = unsafe { medium.u.hGlobal };
        if (fmt.tymed & TYMED_HGLOBAL.0 as u32) == 0
            || medium.tymed != TYMED_HGLOBAL.0 as u32
            || hglobal.0.is_null()
        {
            return Err(DV_E_TYMED.into());
        }
        let size = unsafe { GlobalSize(hglobal) };
        if size < 4 {
            return Err(E_INVALIDARG.into());
        }
        let ptr = unsafe { GlobalLock(hglobal) as *const u32 };
        if ptr.is_null() {
            return Err(E_FAIL.into());
        }
        let effect = unsafe { *ptr };
        let _ = unsafe { GlobalUnlock(hglobal) };
        if frelease.as_bool() {
            unsafe { ReleaseStgMedium(pmedium as *mut STGMEDIUM) };
        }

        // Windows Explorer's filesystem drop target may acknowledge copy completion
        // via CFSTR_PERFORMEDDROPEFFECT instead of calling EndOperation.
        if fmt.cfFormat == performed_cf {
            let ready_ok = self
                .this
                .shared
                .inner
                .lock()
                .ok()
                .is_some_and(|g| {
                    g.started
                        && !g.terminal
                        && g.stage_done
                        && matches!(g.staged.as_ref(), Some(Ok(_)))
                });
            if ready_ok {
                let event = if (effect & DROPEFFECT_COPY.0) != 0 {
                    AsyncDragEvent::Done { effect }
                } else {
                    AsyncDragEvent::Cancelled { reason: None }
                };
                self.this.shared.finish(event);
            }
        }
        Ok(())
    }

    fn EnumFormatEtc(&self, dwdirection: u32) -> WinResult<IEnumFORMATETC> {
        if dwdirection != DATADIR_GET.0 as u32 {
            return Err(E_NOTIMPL.into());
        }
        let mut formats = vec![FORMATETC {
            cfFormat: CF_HDROP,
            ptd: std::ptr::null_mut(),
            dwAspect: DVASPECT_CONTENT.0,
            lindex: -1,
            tymed: TYMED_HGLOBAL.0 as u32,
        }];
        if !self.this.marker_utf16.is_empty() {
            formats.push(FORMATETC {
                cfFormat: CF_UNICODETEXT,
                ptd: std::ptr::null_mut(),
                dwAspect: DVASPECT_CONTENT.0,
                lindex: -1,
                tymed: TYMED_HGLOBAL.0 as u32,
            });
        }
        let pref = preferred_drop_effect_format();
        if pref != 0 {
            formats.push(FORMATETC {
                cfFormat: pref,
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
        _padvsink: Option<&IAdviseSink>,
    ) -> WinResult<u32> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
    }

    fn DUnadvise(&self, _dwconnection: u32) -> WinResult<()> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
    }

    fn EnumDAdvise(&self) -> WinResult<IEnumSTATDATA> {
        Err(windows::Win32::Foundation::OLE_E_ADVISENOTSUPPORTED.into())
    }
}

impl IDataObjectAsyncCapability_Impl for NonAgileDragObjectBox {
    fn SetAsyncMode(&self, fdoopasync: BOOL) -> WinResult<()> {
        let mut guard = self.this.shared.inner.lock().map_err(|_| E_FAIL)?;
        if guard.started || guard.terminal {
            return Err(E_FAIL.into());
        }
        guard.enabled = fdoopasync.as_bool();
        Ok(())
    }

    fn GetAsyncMode(&self) -> WinResult<BOOL> {
        let guard = self.this.shared.inner.lock().map_err(|_| E_FAIL)?;
        Ok(BOOL(if guard.enabled { 1 } else { 0 }))
    }

    fn StartOperation(&self, _pbcreserved: Option<&IBindCtx>) -> WinResult<()> {
        {
            let mut guard = self.this.shared.inner.lock().map_err(|_| E_FAIL)?;
            if !guard.enabled
                || guard.started
                || guard.terminal
                || (self.this.shared.is_cancelled)()
            {
                return Err(E_ABORT.into());
            }
            // Hold one COM reference independently of the pointer gesture until finish().
            self.AddRef();
            guard.held_self = self as *const Self as usize;
            guard.started = true;
        }
        (self.this.shared.notify)(AsyncDragEvent::Started);
        Ok(())
    }

    fn InOperation(&self) -> WinResult<BOOL> {
        let guard = self.this.shared.inner.lock().map_err(|_| E_FAIL)?;
        Ok(BOOL(if guard.started && !guard.terminal {
            1
        } else {
            0
        }))
    }

    fn EndOperation(
        &self,
        hresult: HRESULT,
        _pbcreserved: Option<&IBindCtx>,
        dweffects: u32,
    ) -> WinResult<()> {
        let started = self
            .this
            .shared
            .inner
            .lock()
            .map_err(|_| E_FAIL)?
            .started;
        if !started {
            return Err(E_FAIL.into());
        }
        let event = if hresult.is_err() {
            AsyncDragEvent::Error(format!("system drop failed: HRESULT 0x{:08X}", hresult.0 as u32))
        } else if (dweffects & DROPEFFECT_COPY.0) == 0 {
            AsyncDragEvent::Cancelled { reason: None }
        } else {
            AsyncDragEvent::Done { effect: dweffects }
        };
        self.this.shared.finish(event);
        Ok(())
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

fn spawn_mta_drag_object(
    marker: &str,
    shared: Arc<AsyncShared>,
) -> Result<usize, WinNativeError> {
    let marker_utf16: Vec<u16> = if marker.is_empty() {
        Vec::new()
    } else {
        OsStr::new(marker)
            .encode_wide()
            .chain(std::iter::once(0))
            .collect()
    };

    let (ready_tx, ready_rx) = mpsc::channel::<Result<usize, String>>();
    let shared_for_mta = Arc::clone(&shared);

    std::thread::Builder::new()
        .name("ole-drag-mta".into())
        .spawn(move || {
            let hr = unsafe { CoInitializeEx(None, COINIT_MULTITHREADED) };
            if hr.is_err() {
                let _ = ready_tx.send(Err(format!(
                    "CoInitializeEx(MTA) failed: HRESULT 0x{:08X}",
                    hr.0 as u32
                )));
                return;
            }
            struct CoUninitGuard;
            impl Drop for CoUninitGuard {
                fn drop(&mut self) {
                    unsafe { CoUninitialize() };
                }
            }
            let _co_guard = CoUninitGuard;

            let (released_tx, released_rx) = mpsc::channel::<()>();
            let data_obj = NonAgileDragObjectBox::into_data_object(AsyncDragDataObject {
                marker_utf16,
                shared: Arc::clone(&shared_for_mta),
                released_tx: Mutex::new(Some(released_tx)),
            });
            let unk: Result<IUnknown, _> = data_obj.cast();
            let stream_res = unk.and_then(|u| unsafe {
                CoMarshalInterThreadInterfaceInStream(&IDataObject::IID, &u)
            });
            // Drop local refs; the marshaled stream owns the COM reference now.
            drop(data_obj);

            match stream_res {
                Ok(stream) => {
                    let raw = stream.into_raw() as usize;
                    let _ = ready_tx.send(Ok(raw));
                }
                Err(err) => {
                    let _ = ready_tx.send(Err(format!(
                        "CoMarshalInterThreadInterfaceInStream failed: {err}"
                    )));
                    return;
                }
            }

            // Keep the MTA thread alive until the last COM reference drops, while
            // also observing external job/app cancellation.
            loop {
                match released_rx.recv_timeout(Duration::from_millis(100)) {
                    Ok(()) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
                    Err(mpsc::RecvTimeoutError::Timeout) => {
                        if (shared_for_mta.is_cancelled)() {
                            shared_for_mta.finish(AsyncDragEvent::Cancelled {
                                reason: Some("drag cancelled".into()),
                            });
                        }
                    }
                }
            }
        })
        .map_err(|e| WinNativeError::Platform(format!("failed to spawn MTA drag thread: {e}")))?;

    match ready_rx.recv() {
        Ok(Ok(stream_raw)) => Ok(stream_raw),
        Ok(Err(msg)) => Err(WinNativeError::Platform(msg)),
        Err(_) => Err(WinNativeError::Platform(
            "MTA drag thread exited before marshaling".into(),
        )),
    }
}

/// Runs an asynchronous Windows Shell OLE drag-out gesture on the calling UI STA thread.
///
/// Must be called on the window's main UI STA thread while the left mouse button is pressed.
/// - Internal drops inside WebView2 read `CF_UNICODETEXT` (`marker`) with zero staging.
/// - Releasing without moving >= 4 px cancels cleanly with zero staging.
/// - Drops onto Windows Explorer / Desktop accept `IDataObjectAsyncCapability`, returning
///   immediately (`pending = true`) so SFTP staging runs on the MTA background apartment
///   without blocking the UI thread.
pub fn start_shell_file_drag_async<S, N, C>(
    _hwnd_raw: isize,
    marker: &str,
    stage_fn: S,
    notify_fn: N,
    is_cancelled_fn: C,
) -> Result<NativeDragGestureResult, WinNativeError>
where
    S: FnOnce() -> Result<Vec<PathBuf>, String> + Send + 'static,
    N: Fn(AsyncDragEvent) + Send + Sync + 'static,
    C: Fn() -> bool + Send + Sync + 'static,
{
    if !key_down_lbutton() {
        return Ok(NativeDragGestureResult {
            outcome: DragDropOutcome::Cancelled,
            pending: false,
            ok: true,
            status: "drag cancelled before start (mouse button already released)".into(),
        });
    }

    let _ole = OleGuard::init()?;

    let shared = Arc::new(AsyncShared::new(
        Box::new(stage_fn),
        Arc::new(notify_fn),
        Arc::new(is_cancelled_fn),
    ));

    let stream_raw = spawn_mta_drag_object(marker, Arc::clone(&shared))?;
    // `CoGetInterfaceAndReleaseStream` always consumes and releases the IStream.
    let stream = ManuallyDrop::new(unsafe { IStream::from_raw(stream_raw as *mut c_void) });
    let proxy: IDataObject = unsafe { CoGetInterfaceAndReleaseStream(&*stream) }.map_err(|e| {
        shared.finish(AsyncDragEvent::Cancelled { reason: None });
        WinNativeError::Platform(format!("CoGetInterfaceAndReleaseStream failed: {e}"))
    })?;

    let mut origin = POINT::default();
    let track_move = unsafe { GetCursorPos(&mut origin) }.is_ok();
    let moved = Arc::new(AtomicBool::new(false));
    let cancelled = Arc::new(AtomicBool::new(false));
    let continues = Arc::new(AtomicU32::new(0));
    let feedbacks = Arc::new(AtomicU32::new(0));

    let drop_source: IDropSource = AsyncDropSource {
        start: origin,
        track_move,
        moved: Arc::clone(&moved),
        cancelled: Arc::clone(&cancelled),
        continues: Arc::clone(&continues),
        feedbacks: Arc::clone(&feedbacks),
    }
    .into();

    if !key_down_lbutton() || (shared.is_cancelled)() {
        shared.mark_loop_done();
        shared.finish(AsyncDragEvent::Cancelled { reason: None });
        return Ok(NativeDragGestureResult {
            outcome: DragDropOutcome::Cancelled,
            pending: false,
            ok: true,
            status: "drag cancelled before DoDragDrop".into(),
        });
    }

    let mut performed_effect = DROPEFFECT(0);
    let hr = unsafe { DoDragDrop(&proxy, &drop_source, DROPEFFECT_COPY, &mut performed_effect) };
    shared.mark_loop_done();

    let (started, terminal, final_event) = shared.snapshot();
    let did_move = moved.load(Ordering::Acquire);
    let was_cancelled = cancelled.load(Ordering::Acquire) || hr == DRAGDROP_S_CANCEL;

    if hr.is_err() {
        let msg = format!("DoDragDrop failed: HRESULT 0x{:08X}", hr.0 as u32);
        shared.finish(AsyncDragEvent::Error(msg.clone()));
        return Err(WinNativeError::Platform(msg));
    }

    if started && !terminal {
        if was_cancelled {
            shared.finish(AsyncDragEvent::Cancelled {
                reason: Some("drag cancelled".into()),
            });
            return Ok(NativeDragGestureResult {
                outcome: DragDropOutcome::Cancelled,
                pending: false,
                ok: true,
                status: "the drag was cancelled".into(),
            });
        }
        return Ok(NativeDragGestureResult {
            outcome: DragDropOutcome::Dropped,
            pending: true,
            ok: true,
            status: "system accepted background extraction".into(),
        });
    }

    if started && terminal {
        return match final_event {
            Some(AsyncDragEvent::Done { .. }) => Ok(NativeDragGestureResult {
                outcome: DragDropOutcome::Dropped,
                pending: false,
                ok: true,
                status: "system completed extraction".into(),
            }),
            Some(AsyncDragEvent::Error(msg)) => Ok(NativeDragGestureResult {
                outcome: DragDropOutcome::Cancelled,
                pending: false,
                ok: false,
                status: msg,
            }),
            _ => Ok(NativeDragGestureResult {
                outcome: DragDropOutcome::Cancelled,
                pending: false,
                ok: true,
                status: "the drag was cancelled".into(),
            }),
        };
    }

    // `!started`: either internal drop (marker only), cancelled hold, or target refused.
    shared.finish(AsyncDragEvent::Cancelled { reason: None });

    if hr == DRAGDROP_S_DROP && performed_effect.0 != 0 {
        return Ok(NativeDragGestureResult {
            outcome: DragDropOutcome::Dropped,
            pending: false,
            ok: true,
            status: "drop handled without background extraction".into(),
        });
    }
    if was_cancelled || !did_move {
        return Ok(NativeDragGestureResult {
            outcome: DragDropOutcome::Cancelled,
            pending: false,
            ok: true,
            status: if !did_move {
                "no pointer movement; the armed press was cancelled".into()
            } else {
                "the drag was cancelled".into()
            },
        });
    }

    Ok(NativeDragGestureResult {
        outcome: DragDropOutcome::Cancelled,
        pending: false,
        ok: false,
        status: format!(
            "drop target did not accept drag (hr=0x{:08X}, effect={}, continues={}, feedbacks={})",
            hr.0 as u32,
            performed_effect.0,
            continues.load(Ordering::Relaxed),
            feedbacks.load(Ordering::Relaxed),
        ),
    })
}

/// Compatibility wrapper around `start_shell_file_drag_async`.
pub fn start_shell_file_drag_delayed<F>(
    hwnd_raw: isize,
    marker: &str,
    stage_fn: F,
) -> Result<DragDropOutcome, WinNativeError>
where
    F: FnOnce() -> Result<Vec<PathBuf>, String> + Send + 'static,
{
    let res = start_shell_file_drag_async(hwnd_raw, marker, stage_fn, |_| {}, || false)?;
    Ok(res.outcome)
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
