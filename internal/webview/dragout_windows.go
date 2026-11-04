//go:build windows

package webview

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Native multi-file drag-out.
//
// Chromium decides which formats an HTML5 drag exposes to the operating system,
// and the only file-carrying one it writes is the single-file DownloadURL. To
// drag several files to Explorer or the desktop in one gesture the launcher has
// to own the drag loop, so the OLE data object is built here.
//
// The object exposes two formats:
//
//	CF_HDROP       the staged local copies, which Explorer and the desktop accept
//	CF_UNICODETEXT a JSON marker, the only extra channel Chromium forwards to the
//	               page, so our own panes can tell an internal drop from an upload
//
// The staged copies are made once per drag, in the background, and only for an
// external drop. Chromium reads the file list as soon as a drag enters any of
// its windows, so while the pointer is over the launcher CF_HDROP answers with
// "not available" instead of blocking: the internal drop travels through the
// text marker and must not download the selection. Once the pointer leaves the
// window, staging starts and an external target can wait for the real files.

const (
	cfUnicodeText = 13
	cfHDrop       = 15

	sOK          = 0
	sFalse       = 1
	eNotImpl     = 0x80004001
	eNoInterface = 0x80004002
	eAbort       = 0x80004004
	eFail        = 0x80004005
	eInvalidArg  = 0x80070057
	eOutOfMemory = 0x8007000E

	dvEFormatEtc           = 0x80040064
	dvELindex              = 0x80040068
	dvETymed               = 0x80040069
	dvEDvAspect            = 0x8004006B
	oleEAdviseNotSupported = 0x80040003
	dataESameFormatEtc     = 0x00040130

	tymedHG         = 1
	dvAspectContent = 1
	datadirGet      = 1

	dropeffectCopy = 1

	dragdropSDrop              = 0x00040100
	dragdropSCancel            = 0x00040101
	dragdropSUseDefaultCursors = 0x00040102

	gmemMoveable = 0x0002
	gmemZeroinit = 0x0040

	mkLButton = 0x0001
	mkRButton = 0x0002

	vkLButton = 0x01
	vkRButton = 0x02
	vkEscape  = 0x1B

	gaRoot          = 2
	idcWait         = 32514
	rpcSCallPending = 0x80010115
)

var (
	procDoDragDrop               = syscall.NewLazyDLL("ole32.dll").NewProc("DoDragDrop")
	procOleInitialize            = syscall.NewLazyDLL("ole32.dll").NewProc("OleInitialize")
	procOleUninitialize          = syscall.NewLazyDLL("ole32.dll").NewProc("OleUninitialize")
	procCoWaitForMultipleHandles = syscall.NewLazyDLL("ole32.dll").NewProc("CoWaitForMultipleHandles")
	procCreateEventW             = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateEventW")
	procSetEvent                 = syscall.NewLazyDLL("kernel32.dll").NewProc("SetEvent")
	procCloseHandle              = syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
	procLoadCursorW              = syscall.NewLazyDLL("user32.dll").NewProc("LoadCursorW")
	procSetCursor                = syscall.NewLazyDLL("user32.dll").NewProc("SetCursor")
	procGlobalAlloc              = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalAlloc")
	procGlobalLock               = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalLock")
	procGlobalUnlock             = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalUnlock")
	procGlobalFree               = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalFree")

	procGetAsyncKeyState         = syscall.NewLazyDLL("user32.dll").NewProc("GetAsyncKeyState")
	procGetCursorPos             = syscall.NewLazyDLL("user32.dll").NewProc("GetCursorPos")
	procWindowFromPoint          = syscall.NewLazyDLL("user32.dll").NewProc("WindowFromPoint")
	procGetAncestor              = syscall.NewLazyDLL("user32.dll").NewProc("GetAncestor")
	procRegisterClipboardFormatW = syscall.NewLazyDLL("user32.dll").NewProc("RegisterClipboardFormatW")

	dropEffectFormat sync.Once
	dropEffectCF     uint16
)

// Bound each COM-aware wait so Escape can cancel a slow external drop. Staging
// itself may take longer than a minute for a large directory.
const dragStagePollMilliseconds = 100

// dragMoveThreshold is the distance that turns an armed press into a drag, the
// same default Windows uses for SM_CXDRAG/SM_CYDRAG.
const dragMoveThreshold = 4

var (
	errStagingCancelled = errors.New("the external drop was cancelled while staging")
	errStagingPending   = errors.New("the dragged files are only staged for an external drop")
)

type pointW struct {
	x int32
	y int32
}

// keyDown reports the physical button state. OLE derives GRFKEYSTATE from the
// messages this thread has processed, which lags behind when WebView2 forwards
// the mouse to another process, so a live drag can look button-less.
func keyDown(virtualKey uintptr) bool {
	state, _, _ := procGetAsyncKeyState.Call(virtualKey)
	return int16(state) < 0
}

// comRef turns a pointer Windows handed back to us into an unsafe.Pointer. The
// direct form, unsafe.Pointer(address), trips vet's unsafeptr check because the
// value went through a uintptr; every object it points at stays pinned below.
func comRef(address uintptr) unsafe.Pointer {
	holder := address
	return *(*unsafe.Pointer)(unsafe.Pointer(&holder))
}

// Interface identifiers are compared as raw GUID bytes, so the byte order has to
// match the in-memory COM layout (Data1/Data2/Data3 little endian).
type guidW [16]byte

var (
	guidUnknown       = guidW{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}
	guidDataObject    = guidW{0x0E, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}
	guidDropSource    = guidW{0x21, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}
	guidEnumFormatEtc = guidW{0x03, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}
)

// formatEtcW mirrors FORMATETC. Native pointer alignment supplies the padding
// for both 32-bit and 64-bit Windows.
type formatEtcW struct {
	cfFormat uint16
	ptd      uintptr
	dwAspect uint32
	lindex   int32
	tymed    uint32
}

// stgMediumW mirrors STGMEDIUM, whose union holds an HGLOBAL here.
type stgMediumW struct {
	tymed          uint32
	hGlobal        uintptr
	pUnkForRelease uintptr
}

// dropFilesW mirrors DROPFILES.
type dropFilesW struct {
	pFiles uint32
	x      int32
	y      int32
	fNC    int32
	fWide  int32
}

type dragDataObject struct {
	vtbl   *[12]uintptr
	refs   int32
	marker []uint16
	// ready closes once staging is final. It is nil when paths was supplied
	// directly, as the tests do.
	ready          chan struct{}
	stage          func() ([]string, error)
	async          *dragAsyncState
	hwnd           uintptr
	stageOn        sync.Once
	stageRun       atomic.Bool
	stageCancelled bool
	// Staging notifications are emitted only on the OLE thread, never by the
	// download goroutine: the WebView COM object belongs to that apartment.
	stageChanged func(bool)
	stageVisible bool
	pathsMu      sync.Mutex
	paths        []string
	stageErr     error
	stageCost    time.Duration
	// Movement tracking for a drag that starts on an armed press: releasing the
	// button without leaving the press point must cancel, not drop, so the
	// selection never lands on the row that is still under the cursor.
	startX    int32
	startY    int32
	trackMove bool
	moved     bool
	// Diagnostics, only touched from the drag loop on the UI thread.
	fileRequests int32
}

type dragFormatEnum struct {
	vtbl    *[7]uintptr
	refs    int32
	mu      sync.Mutex
	formats []formatEtcW
	index   int
}

type dragDropSource struct {
	vtbl *[5]uintptr
	refs int32
	// object lets the drag loop notice that the pointer left the window, which is
	// the earliest moment an external drop becomes possible.
	object *dragDataObject
	// Diagnostics, only touched from the drag loop on the UI thread.
	continues     int32
	feedbacks     int32
	firstKeyState uint32
	// cancelled records an Escape during the loop: a deliberate cancel must not
	// be reported as a refused drop.
	cancelled bool
}

var (
	dragDataObjectVtbl *[12]uintptr
	dragFormatEnumVtbl *[7]uintptr
	dragDropSourceVtbl *[5]uintptr

	dragPinMu      sync.Mutex
	dragPinObjects = make(map[any]struct{})
)

func init() {
	dragDataObjectVtbl = &[12]uintptr{
		syscall.NewCallback(dragDataQueryInterface),
		syscall.NewCallback(dragDataAddRef),
		syscall.NewCallback(dragDataRelease),
		syscall.NewCallback(dragDataGetData),
		syscall.NewCallback(dragDataGetDataHere),
		syscall.NewCallback(dragDataQueryGetData),
		syscall.NewCallback(dragDataGetCanonicalFormatEtc),
		syscall.NewCallback(dragDataSetData),
		syscall.NewCallback(dragDataEnumFormatEtc),
		syscall.NewCallback(dragDataDAdvise),
		syscall.NewCallback(dragDataDUnadvise),
		syscall.NewCallback(dragDataEnumDAdvise),
	}
	dragFormatEnumVtbl = &[7]uintptr{
		syscall.NewCallback(dragEnumQueryInterface),
		syscall.NewCallback(dragEnumAddRef),
		syscall.NewCallback(dragEnumRelease),
		syscall.NewCallback(dragEnumNext),
		syscall.NewCallback(dragEnumSkip),
		syscall.NewCallback(dragEnumReset),
		syscall.NewCallback(dragEnumClone),
	}
	dragDropSourceVtbl = &[5]uintptr{
		syscall.NewCallback(dragSourceQueryInterface),
		syscall.NewCallback(dragSourceAddRef),
		syscall.NewCallback(dragSourceRelease),
		syscall.NewCallback(dragSourceQueryContinueDrag),
		syscall.NewCallback(dragSourceGiveFeedback),
	}
}

// dragOutcome summarises one drag for the log and for the page: a drag the
// target refuses is otherwise completely silent.
type dragOutcome struct {
	HRESULT       uint32
	Effect        uint32
	Moved         bool
	Cancelled     bool
	Continues     int32
	Feedbacks     int32
	FirstKeyState uint32
	FileRequests  int32
	Staging       string
	Err           error
	Pending       bool
}

func (o dragOutcome) String() string {
	line := fmt.Sprintf(
		"drag-out: hr=0x%08X effect=%d moved=%t cancelled=%t continue=%d feedback=%d keyState=0x%X fileRequests=%d %s",
		o.HRESULT, o.Effect, o.Moved, o.Cancelled, o.Continues, o.Feedbacks, o.FirstKeyState, o.FileRequests, o.Staging,
	)
	if o.Err != nil {
		line += "; " + o.Err.Error()
	}
	return line
}

// Failed reports a drag that could not move any file: OLE refused the call, the
// target refused the drop, or the staging download failed. A press released in
// place or cancelled with Escape never became a drop, so it stays silent.
func (o dragOutcome) Failed() bool {
	if o.Err != nil || int32(o.HRESULT) < 0 {
		return true
	}
	if o.Pending {
		return false
	}
	if o.Cancelled || !o.Moved {
		return false
	}
	return o.Effect == 0
}

// dragFilesOut runs a blocking OLE drag loop for the files produced by stage.
// It must be called on the thread that owns the window and has OLE initialized.
// report, when set, receives one diagnostic line.
// stagingChanged runs only on the OLE UI thread, including on cancellation.
func dragFilesOut(hwnd uintptr, marker string, stage func() ([]string, error), report func(string), stagingChanged ...func(bool)) dragOutcome {
	if stage == nil {
		return dragOutcome{Err: errors.New("drag-out requires a staging callback")}
	}
	markerUnits, err := syscall.UTF16FromString(marker)
	if err != nil {
		return dragOutcome{Err: err}
	}
	// The page arms only a left-button press. Dispatch may reach this thread
	// after release, in which case starting OLE would create a phantom drag.
	if !keyDown(vkLButton) {
		return dragOutcome{Cancelled: true, Staging: "the initiating button was released before the drag started"}
	}
	// The drag loop starts before the copies exist. Chromium does ask for
	// CF_HDROP while the drag is over the launcher, so that request is answered
	// from what is already staged and never waits for a copy of the selection.
	object := &dragDataObject{
		vtbl:   dragDataObjectVtbl,
		refs:   1,
		marker: markerUnits,
		ready:  make(chan struct{}),
		stage:  stage,
		hwnd:   hwnd,
	}
	if len(stagingChanged) > 0 {
		object.stageChanged = stagingChanged[0]
	}
	defer object.setStagingVisible(false)
	source := &dragDropSource{vtbl: dragDropSourceVtbl, refs: 1, object: object}
	pinDragObject(object)
	pinDragObject(source)
	// Release only this call's references. A target may legally retain an
	// interface after DoDragDrop returns; its final Release removes that root.
	defer object.unpin()
	defer dragSourceRelease(uintptr(unsafe.Pointer(source)))

	initialized, uninitialize := initializeDragOLE(
		func() uintptr {
			hr, _, _ := procOleInitialize.Call(0)
			return hr
		},
		func() { procOleUninitialize.Call() },
	)
	if uninitialize == nil {
		outcome := dragOutcome{
			HRESULT: initialized,
			Err:     fmt.Errorf("OleInitialize failed: HRESULT 0x%08X", initialized),
		}
		if report != nil {
			report(outcome.String())
		}
		return outcome
	}
	defer uninitialize()

	// The drag now starts on an armed press, before the pointer has moved. The
	// press point is what tells a real drag apart from a held release.
	var origin pointW
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&origin))); ok != 0 {
		object.startX, object.startY, object.trackMove = origin.x, origin.y, true
	}

	effect := uint32(dropeffectCopy)
	hr, _, _ := procDoDragDrop.Call(
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(source)),
		uintptr(dropeffectCopy),
		uintptr(unsafe.Pointer(&effect)),
	)
	runtime.KeepAlive(object)
	runtime.KeepAlive(source)
	runtime.KeepAlive(markerUnits)

	paths, stageErr, stageCost, stageDone := object.stagingSnapshot()
	outcome := dragOutcome{
		HRESULT:       uint32(hr),
		Effect:        effect,
		Moved:         object.moved,
		Cancelled:     source.cancelled || object.stageCancelled,
		Continues:     source.continues,
		Feedbacks:     source.feedbacks,
		FirstKeyState: source.firstKeyState,
		FileRequests:  object.fileRequests,
	}
	switch {
	case outcome.Cancelled:
		outcome.Staging = "the drag was cancelled"
	case !outcome.Moved:
		outcome.Staging = "no pointer movement; the armed press was cancelled"
	case !stageDone && !object.stagingStarted():
		outcome.Staging = "staging skipped; the drag stayed inside the launcher"
	case !stageDone:
		outcome.Staging = "staging still running"
	case stageErr != nil:
		outcome.Staging = "staging failed: " + stageErr.Error()
	default:
		outcome.Staging = fmt.Sprintf("staged %d in %s", len(paths), stageCost.Round(time.Millisecond))
	}
	if int32(hr) < 0 {
		outcome.Err = fmt.Errorf("DoDragDrop failed: HRESULT 0x%08X", uint32(hr))
	} else if !outcome.Cancelled && stageDone && stageErr != nil && object.fileRequests > 0 {
		// Only a target that asked for the file list was affected by the failure;
		// an internal drop already completed through the marker.
		outcome.Err = stageErr
	}
	if report != nil {
		report(outcome.String())
	}
	return outcome
}

// initializeDragOLE pairs both S_OK and S_FALSE with OleUninitialize. A failed
// initialization (including an incompatible apartment) must not enter the drag loop.
func initializeDragOLE(initialize func() uintptr, uninitialize func()) (uint32, func()) {
	hr := uint32(initialize())
	if int32(hr) < 0 {
		return hr, nil
	}
	return hr, uninitialize
}

// beginStaging renders the copies once, in the background. GetData waits on the
// same channel, so a target either sees the finished list or a staging error.
func (o *dragDataObject) beginStaging() {
	o.stageOn.Do(func() {
		o.stageRun.Store(true)
		if o.async == nil {
			o.setStagingVisible(true)
		}
		if o.stage == nil {
			close(o.ready)
			return
		}
		started := time.Now()
		go func() {
			paths, err := o.stage()
			o.pathsMu.Lock()
			o.paths, o.stageErr, o.stageCost = paths, err, time.Since(started)
			o.pathsMu.Unlock()
			close(o.ready)
		}()
	})
}

func (o *dragDataObject) setStagingVisible(visible bool) {
	if o.stageVisible == visible {
		return
	}
	o.stageVisible = visible
	if o.stageChanged != nil {
		o.stageChanged(visible)
	}
}

// refreshStagingVisible is called by the OLE callbacks, so a completed copy
// hides its indicator even when the pointer returns to an internal target.
func (o *dragDataObject) refreshStagingVisible() {
	if !o.stageVisible || o.ready == nil {
		return
	}
	select {
	case <-o.ready:
		o.setStagingVisible(false)
	default:
	}
}

func (o *dragDataObject) stagingStarted() bool {
	return o.stageRun.Load()
}

// filePaths answers CF_HDROP. Chromium reads the file list on drag enter, and
// the launcher's own page is the first target a drag enters; blocking that call
// would freeze the UI thread while the whole selection is downloaded for a drop
// that only needs the marker. So a pointer over the launcher gets the list only
// when staging already finished, while a target outside the window waits for
// the copies because it cannot accept the drop without them.
func (o *dragDataObject) filePaths() ([]string, error) {
	if o.ready == nil {
		return o.paths, nil
	}
	if !o.pointerLeftWindow() {
		return o.stagedPaths()
	}
	return o.waitPaths()
}

// stagedPaths reports the copies without starting or waiting for staging.
func (o *dragDataObject) stagedPaths() ([]string, error) {
	o.pathsMu.Lock()
	defer o.pathsMu.Unlock()
	select {
	case <-o.ready:
		return o.paths, o.stageErr
	default:
		return nil, errStagingPending
	}
}

// waitPaths blocks until staging finishes. Tests fill paths directly and have no
// ready channel.
func (o *dragDataObject) waitPaths() ([]string, error) {
	if o.ready == nil {
		return o.paths, nil
	}
	o.beginStaging()
	defer o.setStagingVisible(false)
	if err := waitForDragStage(o.ready, func() bool { return keyDown(vkEscape) }); err != nil {
		if errors.Is(err, errStagingCancelled) {
			o.stageCancelled = true
		}
		return nil, err
	}
	o.pathsMu.Lock()
	defer o.pathsMu.Unlock()
	return o.paths, o.stageErr
}

// waitForDragStage keeps the STA in COM's modal wait rather than blocking it on
// a Go channel. Default flags dispatch only COM's required messages, avoiding an
// arbitrary application-message pump and its nested drags. Escape cancels this
// wait and the drag; the host then cancels the staging context and its active IO.
func waitForDragStage(ready <-chan struct{}, cancelled func() bool) error {
	select {
	case <-ready:
		return nil
	default:
	}
	event, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if event == 0 {
		return fmt.Errorf("could not create the drag staging event: %v", err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ready:
			procSetEvent.Call(event)
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		// The signaling goroutine must finish before its handle can be closed or
		// recycled, including when Escape won the race with staging completion.
		<-stopped
		procCloseHandle.Call(event)
	}()
	cursor, _, _ := procLoadCursorW.Call(0, idcWait)
	if cursor != 0 {
		previous, _, _ := procSetCursor.Call(cursor)
		defer procSetCursor.Call(previous)
	}
	return pollDragStage(ready, cancelled, func() uint32 {
		var index uint32
		hr, _, _ := procCoWaitForMultipleHandles.Call(
			0, dragStagePollMilliseconds, 1,
			uintptr(unsafe.Pointer(&event)), uintptr(unsafe.Pointer(&index)),
		)
		return uint32(hr)
	})
}

func pollDragStage(ready <-chan struct{}, cancelled func() bool, wait func() uint32) error {
	for {
		select {
		case <-ready:
			return nil
		default:
		}
		if cancelled() {
			return errStagingCancelled
		}
		if hr := wait(); hr != sOK && hr != rpcSCallPending {
			return fmt.Errorf("drag staging wait failed: HRESULT 0x%08X", hr)
		}
	}
}

func (o *dragDataObject) stagingSnapshot() ([]string, error, time.Duration, bool) {
	if o.ready == nil {
		return o.paths, nil, 0, true
	}
	o.pathsMu.Lock()
	defer o.pathsMu.Unlock()
	done := true
	select {
	case <-o.ready:
	default:
		done = false
	}
	return o.paths, o.stageErr, o.stageCost, done
}

// pointerLeftWindow reports whether the drag is over another window. A target
// that is not the launcher needs real files, so staging may start now instead of
// waiting for the drop.
func (o *dragDataObject) pointerLeftWindow() bool {
	if o.hwnd == 0 {
		return false
	}
	var point pointW
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&point))); ok == 0 {
		return false
	}
	return pointerOverExternalWindow(o.hwnd, windowAtPoint(point), rootWindow)
}

func rootWindow(hwnd uintptr) uintptr {
	root, _, _ := procGetAncestor.Call(hwnd, gaRoot)
	return root
}

// WindowFromPoint takes POINT by value: one packed 64-bit argument on win64,
// two 32-bit stack arguments on win32. Negative monitor coordinates retain their
// two's-complement representation in either ABI.
func windowAtPoint(point pointW) uintptr {
	args := windowPointArgs(point)
	hwnd, _, _ := procWindowFromPoint.Call(args...)
	return hwnd
}

func windowPointArgs(point pointW) []uintptr {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		return []uintptr{uintptr(uint64(uint32(point.x)) | uint64(uint32(point.y))<<32)}
	}
	return []uintptr{uintptr(uint32(point.x)), uintptr(uint32(point.y))}
}

func pointerOverExternalWindow(source, target uintptr, rootOf func(uintptr) uintptr) bool {
	if source == 0 || target == 0 || source == target {
		return false
	}
	sourceRoot, targetRoot := rootOf(source), rootOf(target)
	// If either lookup failed, stay conservative and avoid staging an internal
	// drag. Child WebView2 windows share the launcher's top-level ancestor.
	return sourceRoot != 0 && targetRoot != 0 && sourceRoot != targetRoot
}

// markMovement records that the pointer left the press point. Without it a
// button release is a cancelled press, not a drop: the armed drag starts while
// the cursor is still on the source row.
func (o *dragDataObject) markMovement() {
	if o.moved {
		return
	}
	if !o.trackMove {
		// Tests drive this object without a real window or cursor.
		o.moved = true
		return
	}
	var point pointW
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&point))); ok == 0 {
		o.moved = true
		return
	}
	o.moved = cursorLeftPressPoint(o.startX, o.startY, point.x, point.y)
}

func cursorLeftPressPoint(startX, startY, x, y int32) bool {
	dx, dy := x-startX, y-startY
	return dx*dx+dy*dy >= dragMoveThreshold*dragMoveThreshold
}

// releaseAction is what a button release means: a press that never moved is a
// cancelled hold, anything else ends in a drop.
func dragButtonAction(initiatingButtonDown, moved bool) uintptr {
	if initiatingButtonDown {
		return sOK
	}
	return releaseAction(moved)
}

func releaseAction(moved bool) uintptr {
	if moved {
		return dragdropSDrop
	}
	return dragdropSCancel
}

func (o *dragDataObject) pin() int32 {
	return atomic.AddInt32(&o.refs, 1)
}

func (o *dragDataObject) unpin() int32 {
	remaining := releaseDragObject(o, &o.refs)
	if remaining == 0 && o.async != nil {
		o.finishAsync(dragAsyncEvent{State: "cancelled"})
		o.async.releaseOnce.Do(func() { close(o.async.released) })
	}
	return remaining
}

// pinDragObject roots a newly created COM object with its initial reference.
// Go's collector cannot see raw interface pointers retained by Windows.
func pinDragObject(object any) {
	dragPinMu.Lock()
	dragPinObjects[object] = struct{}{}
	dragPinMu.Unlock()
}

func releaseDragObject(object any, refs *int32) int32 {
	remaining := atomic.AddInt32(refs, -1)
	if remaining == 0 {
		dragPinMu.Lock()
		delete(dragPinObjects, object)
		dragPinMu.Unlock()
	}
	return remaining
}

func dragDataQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return eInvalidArg
	}
	if riid != 0 {
		guid := *(*guidW)(comRef(riid))
		object := (*dragDataObject)(comRef(this))
		if guid == guidDragAsync && object.async != nil {
			*(*uintptr)(comRef(ppv)) = uintptr(unsafe.Pointer(&object.async.iface))
			object.pin()
			return sOK
		}
		if guid == guidUnknown || guid == guidDataObject {
			*(*uintptr)(comRef(ppv)) = this
			(*dragDataObject)(comRef(this)).pin()
			return sOK
		}
	}
	*(*uintptr)(comRef(ppv)) = 0
	return eNoInterface
}

func dragDataAddRef(this uintptr) uintptr {
	return uintptr((*dragDataObject)(comRef(this)).pin())
}

func dragDataRelease(this uintptr) uintptr {
	return uintptr((*dragDataObject)(comRef(this)).unpin())
}

func dragDataGetData(this, pformatetc, pmedium uintptr) uintptr {
	if pformatetc == 0 || pmedium == 0 {
		return eInvalidArg
	}
	format := (*formatEtcW)(comRef(pformatetc))
	medium := (*stgMediumW)(comRef(pmedium))
	*medium = stgMediumW{}
	if hr := validateDragFormat(format); hr != sOK {
		return hr
	}
	object := (*dragDataObject)(comRef(this))
	switch format.cfFormat {
	case cfHDrop:
		var paths []string
		var stageErr error
		if object.async != nil {
			paths, stageErr = object.asyncPaths()
			if errors.Is(stageErr, errStagingPending) {
				return dvEFormatEtc
			}
		} else {
			object.fileRequests++
			paths, stageErr = object.filePaths()
		}
		if object.async != nil && object.async.ctx.Err() != nil {
			return eAbort
		}
		if errors.Is(stageErr, errStagingCancelled) {
			return eAbort
		}
		if stageErr != nil || len(paths) == 0 {
			return eFail
		}
		handle, allocErr := newDropFilesHandle(paths)
		if allocErr != nil {
			return eOutOfMemory
		}
		medium.tymed = tymedHG
		medium.hGlobal = handle
		return sOK
	case cfUnicodeText:
		handle, allocErr := newGlobalTextHandle(object.marker)
		if allocErr != nil {
			return eOutOfMemory
		}
		medium.tymed = tymedHG
		medium.hGlobal = handle
		return sOK
	case preferredDropEffectFormat():
		handle, allocErr := newGlobalDropEffectHandle()
		if allocErr != nil {
			return eOutOfMemory
		}
		medium.tymed = tymedHG
		medium.hGlobal = handle
		return sOK
	}
	return dvEFormatEtc
}

func dragDataGetDataHere(this, pformatetc, pmedium uintptr) uintptr {
	return eNotImpl
}

func dragDataQueryGetData(this, pformatetc uintptr) uintptr {
	if pformatetc == 0 {
		return eInvalidArg
	}
	return validateDragFormat((*formatEtcW)(comRef(pformatetc)))
}

// validateDragFormat is shared by the probe and renderer: S_OK promises that
// GetData can produce the requested aspect, index and storage medium.
func validateDragFormat(format *formatEtcW) uintptr {
	if format.cfFormat != cfHDrop && format.cfFormat != cfUnicodeText &&
		(format.cfFormat == 0 || format.cfFormat != preferredDropEffectFormat()) {
		return dvEFormatEtc
	}
	if format.dwAspect != dvAspectContent {
		return dvEDvAspect
	}
	if format.lindex != -1 {
		return dvELindex
	}
	if format.tymed&tymedHG == 0 {
		return dvETymed
	}
	return sOK
}

func dragDataGetCanonicalFormatEtc(this, pformatetcIn, pformatetcOut uintptr) uintptr {
	if pformatetcOut != 0 {
		*(*formatEtcW)(comRef(pformatetcOut)) = formatEtcW{lindex: -1}
	}
	return dataESameFormatEtc
}

func dragDataSetData(this, pformatetc, pmedium, release uintptr) uintptr {
	return setDragFeedback((*dragDataObject)(comRef(this)), pformatetc, pmedium, release)
}

func dragDataEnumFormatEtc(this, direction, ppenum uintptr) uintptr {
	if uint32(direction) != datadirGet || ppenum == 0 {
		return eNotImpl
	}
	enum := &dragFormatEnum{vtbl: dragFormatEnumVtbl, refs: 1, formats: dragFormats()}
	pinDragObject(enum)
	*(*uintptr)(comRef(ppenum)) = uintptr(unsafe.Pointer(enum))
	return sOK
}

func dragDataDAdvise(this, pformatetc, advf, advSink, connection uintptr) uintptr {
	return oleEAdviseNotSupported
}

func dragDataDUnadvise(this, connection uintptr) uintptr {
	return oleEAdviseNotSupported
}

func dragDataEnumDAdvise(this, ppenum uintptr) uintptr {
	return oleEAdviseNotSupported
}

func dragFormats() []formatEtcW {
	return []formatEtcW{
		{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG},
		{cfFormat: cfUnicodeText, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG},
		{cfFormat: preferredDropEffectFormat(), dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG},
	}
}

// preferredDropEffectFormat is "Preferred DropEffect", which tells the target
// that the staged copies come with a copy, not a move.
func preferredDropEffectFormat() uint16 {
	dropEffectFormat.Do(func() {
		name, err := syscall.UTF16PtrFromString("Preferred DropEffect")
		if err != nil {
			return
		}
		registered, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name)))
		dropEffectCF = uint16(registered)
	})
	return dropEffectCF
}

func newGlobalDropEffectHandle() (uintptr, error) {
	handle, err := globalAlloc(4)
	if err != nil {
		return 0, err
	}
	base := globalLock(handle)
	if base == nil {
		globalFree(handle)
		return 0, errors.New("could not lock the drop effect")
	}
	*(*uint32)(base) = dropeffectCopy
	globalUnlock(handle)
	return handle, nil
}

func dragEnumQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return eInvalidArg
	}
	if riid != 0 {
		guid := *(*guidW)(comRef(riid))
		if guid == guidUnknown || guid == guidEnumFormatEtc {
			*(*uintptr)(comRef(ppv)) = this
			atomic.AddInt32(&(*dragFormatEnum)(comRef(this)).refs, 1)
			return sOK
		}
	}
	*(*uintptr)(comRef(ppv)) = 0
	return eNoInterface
}

func dragEnumAddRef(this uintptr) uintptr {
	return uintptr(atomic.AddInt32(&(*dragFormatEnum)(comRef(this)).refs, 1))
}

func dragEnumRelease(this uintptr) uintptr {
	enum := (*dragFormatEnum)(comRef(this))
	return uintptr(releaseDragObject(enum, &enum.refs))
}

func dragEnumNext(this, celt, rgelt, fetched uintptr) uintptr {
	enum := (*dragFormatEnum)(comRef(this))
	enum.mu.Lock()
	defer enum.mu.Unlock()
	want := int(uint32(celt))
	if want <= 0 || rgelt == 0 {
		return eInvalidArg
	}
	count := 0
	entrySize := unsafe.Sizeof(formatEtcW{})
	for count < want && enum.index+count < len(enum.formats) {
		target := (*formatEtcW)(unsafe.Add(comRef(rgelt), uintptr(count)*entrySize))
		*target = enum.formats[enum.index+count]
		count++
	}
	enum.index += count
	if fetched != 0 {
		*(*uint32)(comRef(fetched)) = uint32(count)
	}
	if count == want {
		return sOK
	}
	return sFalse
}

func dragEnumSkip(this, celt uintptr) uintptr {
	enum := (*dragFormatEnum)(comRef(this))
	enum.mu.Lock()
	defer enum.mu.Unlock()
	remaining := len(enum.formats) - enum.index
	skip := int(uint32(celt))
	if skip > remaining {
		enum.index = len(enum.formats)
		return sFalse
	}
	enum.index += skip
	return sOK
}

func dragEnumReset(this uintptr) uintptr {
	enum := (*dragFormatEnum)(comRef(this))
	enum.mu.Lock()
	defer enum.mu.Unlock()
	enum.index = 0
	return sOK
}

func dragEnumClone(this, ppenum uintptr) uintptr {
	if ppenum == 0 {
		return eInvalidArg
	}
	source := (*dragFormatEnum)(comRef(this))
	source.mu.Lock()
	defer source.mu.Unlock()
	clone := &dragFormatEnum{
		vtbl:    dragFormatEnumVtbl,
		refs:    1,
		formats: source.formats,
		index:   source.index,
	}
	pinDragObject(clone)
	*(*uintptr)(comRef(ppenum)) = uintptr(unsafe.Pointer(clone))
	return sOK
}

func dragSourceQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return eInvalidArg
	}
	if riid != 0 {
		guid := *(*guidW)(comRef(riid))
		if guid == guidUnknown || guid == guidDropSource {
			*(*uintptr)(comRef(ppv)) = this
			atomic.AddInt32(&(*dragDropSource)(comRef(this)).refs, 1)
			return sOK
		}
	}
	*(*uintptr)(comRef(ppv)) = 0
	return eNoInterface
}

func dragSourceAddRef(this uintptr) uintptr {
	return uintptr(atomic.AddInt32(&(*dragDropSource)(comRef(this)).refs, 1))
}

func dragSourceRelease(this uintptr) uintptr {
	source := (*dragDropSource)(comRef(this))
	return uintptr(releaseDragObject(source, &source.refs))
}

func dragSourceQueryContinueDrag(this, escapePressed, keyState uintptr) uintptr {
	source := (*dragDropSource)(comRef(this))
	if source.continues == 0 {
		source.firstKeyState = uint32(keyState)
	}
	source.continues++
	object := source.object
	if object != nil && object.stageCancelled {
		source.cancelled = true
		return dragdropSCancel
	}
	if object != nil {
		object.markMovement()
		object.refreshStagingVisible()
		if object.pointerLeftWindow() {
			object.beginStaging()
		}
	}
	if escapePressed != 0 {
		source.cancelled = true
		return dragdropSCancel
	}
	// Only the initiating left button controls this drag. WebView2's reported
	// key state can lag a release; neither it nor an unrelated right button may
	// keep a released left-button drag alive.
	return dragButtonAction(keyDown(vkLButton), object == nil || object.moved)
}

func dragSourceGiveFeedback(this, effect uintptr) uintptr {
	source := (*dragDropSource)(comRef(this))
	source.feedbacks++
	if object := source.object; object != nil {
		object.markMovement()
		object.refreshStagingVisible()
		if object.pointerLeftWindow() {
			object.beginStaging()
		}
	}
	return dragdropSUseDefaultCursors
}

// newDropFilesHandle builds the DROPFILES payload Explorer reads out of
// CF_HDROP: a header followed by a double-NUL-terminated UTF-16 path list.
func newDropFilesHandle(paths []string) (uintptr, error) {
	header := dropFilesW{pFiles: uint32(unsafe.Sizeof(dropFilesW{})), fWide: 1}
	var names []uint16
	for _, path := range paths {
		units, err := syscall.UTF16FromString(path)
		if err != nil {
			continue
		}
		names = append(names, units...)
	}
	if len(names) == 0 {
		return 0, errors.New("no usable file paths to drag")
	}
	names = append(names, 0)
	size := int(unsafe.Sizeof(header)) + len(names)*2
	handle, err := globalAlloc(size)
	if err != nil {
		return 0, err
	}
	base := globalLock(handle)
	if base == nil {
		globalFree(handle)
		return 0, errors.New("could not lock the drag payload")
	}
	*(*dropFilesW)(base) = header
	target := unsafe.Slice((*uint16)(unsafe.Add(base, unsafe.Sizeof(header))), len(names))
	copy(target, names)
	globalUnlock(handle)
	return handle, nil
}

func newGlobalTextHandle(units []uint16) (uintptr, error) {
	if len(units) == 0 {
		return 0, errors.New("empty drag marker")
	}
	size := len(units) * 2
	handle, err := globalAlloc(size)
	if err != nil {
		return 0, err
	}
	base := globalLock(handle)
	if base == nil {
		globalFree(handle)
		return 0, errors.New("could not lock the drag marker")
	}
	source := unsafe.Slice((*uint16)(base), len(units))
	copy(source, units)
	globalUnlock(handle)
	return handle, nil
}

func globalAlloc(size int) (uintptr, error) {
	handle, _, err := procGlobalAlloc.Call(gmemMoveable|gmemZeroinit, uintptr(size))
	if handle == 0 {
		return 0, fmt.Errorf("GlobalAlloc(%d) failed: %v", size, err)
	}
	return handle, nil
}

func globalLock(handle uintptr) unsafe.Pointer {
	pointer, _, _ := procGlobalLock.Call(handle)
	if pointer == 0 {
		return nil
	}
	return comRef(pointer)
}

func globalUnlock(handle uintptr) {
	_, _, _ = procGlobalUnlock.Call(handle)
}

func globalFree(handle uintptr) {
	_, _, _ = procGlobalFree.Call(handle)
}
