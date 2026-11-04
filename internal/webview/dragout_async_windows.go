//go:build windows

package webview

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// IDataObjectAsyncCapability uses BOOL (32 bits), not VARIANT_BOOL.
var guidDragAsync = guidW{0x90, 0x05, 0x8B, 0x3D, 0x91, 0xF6, 0xD2, 0x11, 0x8E, 0xA9, 0x00, 0x60, 0x97, 0xDF, 0x5B, 0xD4}
var (
	procDragCoInitializeEx = syscall.NewLazyDLL("ole32.dll").NewProc("CoInitializeEx")
	procDragCoUninitialize = syscall.NewLazyDLL("ole32.dll").NewProc("CoUninitialize")
	procDragMarshal        = syscall.NewLazyDLL("ole32.dll").NewProc("CoMarshalInterThreadInterfaceInStream")
	procDragUnmarshal      = syscall.NewLazyDLL("ole32.dll").NewProc("CoGetInterfaceAndReleaseStream")
)

type dragAsyncEvent struct {
	State   string
	HRESULT uint32
	Effect  uint32
	Err     error
}
type dragAsyncInterface struct {
	vtbl  *[8]uintptr
	owner *dragDataObject
}
type dragAsyncState struct {
	iface       dragAsyncInterface
	mu          sync.Mutex
	enabled     bool
	started     bool
	terminal    bool
	final       dragAsyncEvent
	held        bool
	notify      func(dragAsyncEvent)
	ctx         context.Context
	loopDone    chan struct{}
	loopOnce    sync.Once
	released    chan struct{}
	releaseOnce sync.Once
}

var dragAsyncVtbl = &[8]uintptr{
	syscall.NewCallback(dragAsyncQueryInterface), syscall.NewCallback(dragAsyncAddRef), syscall.NewCallback(dragAsyncRelease),
	syscall.NewCallback(dragAsyncSetMode), syscall.NewCallback(dragAsyncGetMode), syscall.NewCallback(dragAsyncStart),
	syscall.NewCallback(dragAsyncInOperation), syscall.NewCallback(dragAsyncEnd),
}

func dragAsyncOwner(this uintptr) *dragDataObject { return (*dragAsyncInterface)(comRef(this)).owner }
func dragAsyncQueryInterface(this, iid, out uintptr) uintptr {
	return dragDataQueryInterface(uintptr(unsafe.Pointer(dragAsyncOwner(this))), iid, out)
}
func dragAsyncAddRef(this uintptr) uintptr  { return uintptr(dragAsyncOwner(this).pin()) }
func dragAsyncRelease(this uintptr) uintptr { return uintptr(dragAsyncOwner(this).unpin()) }
func dragAsyncSetMode(this, enabled uintptr) uintptr {
	a := dragAsyncOwner(this).async
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started || a.terminal {
		return eFail
	}
	a.enabled = uint32(enabled) != 0
	return sOK
}
func dragAsyncGetMode(this, out uintptr) uintptr {
	if out == 0 {
		return eInvalidArg
	}
	a := dragAsyncOwner(this).async
	a.mu.Lock()
	enabled := a.enabled
	a.mu.Unlock()
	*(*int32)(comRef(out)) = 0
	if enabled {
		*(*int32)(comRef(out)) = 1
	}
	return sOK
}
func dragAsyncStart(this, reserved uintptr) uintptr {
	o := dragAsyncOwner(this)
	a := o.async
	a.mu.Lock()
	if !a.enabled || a.started || a.terminal || a.ctx.Err() != nil {
		a.mu.Unlock()
		return eAbort
	}
	// The operation owns one reference independently of the pointer gesture.
	o.pin()
	a.held = true
	a.started = true
	notify := a.notify
	a.mu.Unlock()
	if notify != nil {
		notify(dragAsyncEvent{State: "started"})
	}
	return sOK
}
func dragAsyncInOperation(this, out uintptr) uintptr {
	if out == 0 {
		return eInvalidArg
	}
	a := dragAsyncOwner(this).async
	a.mu.Lock()
	active := a.started && !a.terminal
	a.mu.Unlock()
	*(*int32)(comRef(out)) = 0
	if active {
		*(*int32)(comRef(out)) = 1
	}
	return sOK
}
func dragAsyncEnd(this, result, reserved, effect uintptr) uintptr {
	o := dragAsyncOwner(this)
	a := o.async
	a.mu.Lock()
	started := a.started
	a.mu.Unlock()
	if !started {
		return eFail
	}
	event := dragAsyncEvent{State: "done", HRESULT: uint32(result), Effect: uint32(effect)}
	if int32(result) < 0 {
		event.State = "error"
		event.Err = fmt.Errorf("system drop failed: HRESULT 0x%08X", uint32(result))
	} else if uint32(effect)&dropeffectCopy == 0 {
		event.State = "cancelled"
	}
	o.finishAsync(event)
	return sOK
}
func (o *dragDataObject) finishAsync(event dragAsyncEvent) {
	a := o.async
	a.mu.Lock()
	if a.terminal {
		a.mu.Unlock()
		return
	}
	if event.State == "done" && a.ctx.Err() != nil {
		event = dragAsyncEvent{State: "cancelled", Err: a.ctx.Err()}
	}
	a.terminal = true
	a.final = event
	held := a.held
	a.held = false
	notify := a.notify
	a.mu.Unlock()
	// Callback can cancel the context; it must never run under the state lock.
	if notify != nil {
		notify(event)
	}
	if held {
		o.unpin()
	}
}
func (o *dragDataObject) asyncPaths() ([]string, error) {
	a := o.async
	a.mu.Lock()
	allowed := a.started && !a.terminal
	a.mu.Unlock()
	if !allowed {
		return nil, errStagingPending
	}
	// This source supports targets that extract on a background worker. Never
	// start network IO on a drag-enter probe or the launcher's own marker drop.
	// A background worker may reach GetData just before Drop returns. Give
	// that race a bounded handoff, never an unbounded loopDone wait: targets
	// that call GetData synchronously inside Drop would otherwise deadlock.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-a.loopDone:
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	case <-timer.C:
		return nil, errStagingPending
	}
	a.mu.Lock()
	allowed = !a.terminal
	a.mu.Unlock()
	if !allowed {
		return nil, context.Canceled
	}
	o.beginStaging()
	select {
	case <-o.ready:
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	}
	paths, err := o.stagedPaths()
	if err != nil {
		o.finishAsync(dragAsyncEvent{State: "error", Err: err})
	}
	return paths, err
}

type dragMarshaledObject struct {
	object *dragDataObject
	stream uintptr
	err    error
}

// The server object is created in an MTA and marshaled to the window's STA.
// Standard COM marshaling then dispatches the target's background GetData to
// the MTA, rather than to the WebView thread that owns DoDragDrop.
func newAsyncDragObject(marker string, stage func() ([]string, error), notify func(dragAsyncEvent), ctx context.Context) (*dragDataObject, uintptr, error) {
	units, err := syscall.UTF16FromString(marker)
	if err != nil {
		return nil, 0, err
	}
	result := make(chan dragMarshaledObject, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hr, _, _ := procDragCoInitializeEx.Call(0, 0)
		if int32(hr) < 0 {
			result <- dragMarshaledObject{err: fmt.Errorf("initialize drag MTA: HRESULT 0x%08X", uint32(hr))}
			return
		}
		defer procDragCoUninitialize.Call()
		a := &dragAsyncState{enabled: true, notify: notify, ctx: ctx, loopDone: make(chan struct{}), released: make(chan struct{})}
		o := &dragDataObject{vtbl: dragDataObjectVtbl, refs: 1, marker: units, ready: make(chan struct{}), stage: stage, async: a}
		a.iface = dragAsyncInterface{vtbl: dragAsyncVtbl, owner: o}
		pinDragObject(o)
		var stream uintptr
		hr, _, _ = procDragMarshal.Call(uintptr(unsafe.Pointer(&guidDataObject)), uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&stream)))
		if int32(hr) < 0 || stream == 0 {
			o.unpin()
			result <- dragMarshaledObject{err: fmt.Errorf("marshal drag data object: HRESULT 0x%08X", uint32(hr))}
			return
		}
		result <- dragMarshaledObject{object: o, stream: stream}
		o.unpin() // The stream now owns a COM reference.
		<-a.released
	}()
	r := <-result
	if r.err != nil {
		return nil, 0, r.err
	}
	var proxy uintptr
	hr, _, _ := procDragUnmarshal.Call(r.stream, uintptr(unsafe.Pointer(&guidDataObject)), uintptr(unsafe.Pointer(&proxy)))
	if int32(hr) < 0 || proxy == 0 {
		return nil, 0, fmt.Errorf("unmarshal drag data object: HRESULT 0x%08X", uint32(hr))
	}
	// Observe host/job cancellation even when the target accepted extraction
	// but never calls GetData or EndOperation.
	go func() {
		select {
		case <-ctx.Done():
			r.object.finishAsync(dragAsyncEvent{State: "cancelled", Err: ctx.Err()})
		case <-r.object.async.released:
		}
	}()
	return r.object, proxy, nil
}
func releaseDragCOM(pointer uintptr) {
	if pointer != 0 {
		vtbl := *(**[3]uintptr)(comRef(pointer))
		syscall.SyscallN(vtbl[2], pointer)
	}
}

// dragFilesOutAsync ends with the pointer gesture, while a target that accepted
// IDataObjectAsyncCapability owns extraction and reports its result later.
func dragFilesOutAsync(hwnd uintptr, marker string, stage func() ([]string, error), report func(string), notify func(dragAsyncEvent), contexts ...context.Context) dragOutcome {
	if stage == nil {
		return dragOutcome{Err: errors.New("drag-out requires a staging callback")}
	}
	if !keyDown(vkLButton) {
		return dragOutcome{Cancelled: true}
	}
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	initialized, uninitialize := initializeDragOLE(func() uintptr { hr, _, _ := procOleInitialize.Call(0); return hr }, func() { procOleUninitialize.Call() })
	if uninitialize == nil {
		return dragOutcome{HRESULT: initialized, Err: fmt.Errorf("OleInitialize failed: HRESULT 0x%08X", initialized)}
	}
	defer uninitialize()
	object, proxy, err := newAsyncDragObject(marker, stage, notify, ctx)
	if err != nil {
		return dragOutcome{Err: err}
	}
	defer releaseDragCOM(proxy)
	// Reuse the UI-only source for physical-button/movement tracking, without
	// attaching the data object (legacy source callbacks prefetch on movement).
	tracking := &dragDataObject{hwnd: hwnd}
	var origin pointW
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&origin))); ok != 0 {
		tracking.startX, tracking.startY, tracking.trackMove = origin.x, origin.y, true
	}
	source := &dragDropSource{vtbl: dragAsyncSourceVtbl, refs: 1, object: tracking}
	pinDragObject(source)
	defer dragSourceRelease(uintptr(unsafe.Pointer(source)))
	// Marshaling can take long enough for the initial press to have ended.
	if !keyDown(vkLButton) || ctx.Err() != nil {
		object.finishAsync(dragAsyncEvent{State: "cancelled"})
		return dragOutcome{Cancelled: true}
	}
	effect := uint32(dropeffectCopy)
	hr, _, _ := procDoDragDrop.Call(proxy, uintptr(unsafe.Pointer(source)), dropeffectCopy, uintptr(unsafe.Pointer(&effect)))
	object.async.loopOnce.Do(func() { close(object.async.loopDone) })
	object.async.mu.Lock()
	started, terminal, final := object.async.started, object.async.terminal, object.async.final
	object.async.mu.Unlock()
	outcome := asyncDragOutcome(uint32(hr), effect, started, terminal, final)
	outcome.Moved = tracking.moved
	outcome.Cancelled = outcome.Cancelled || source.cancelled
	outcome.Continues, outcome.Feedbacks, outcome.FirstKeyState = source.continues, source.feedbacks, source.firstKeyState
	if outcome.Cancelled || int32(hr) < 0 || (effect == 0 && !started) {
		object.finishAsync(dragAsyncEvent{State: "cancelled", Err: outcome.Err})
		outcome.Pending = false
	} else if !started {
		object.finishAsync(dragAsyncEvent{State: "cancelled"})
		// A successful marker-only internal drop needs no system file delivery.
		outcome.Staging = "target did not accept an asynchronous file transfer"
	} else {
		outcome.Staging = "system accepted background extraction"
	}
	if report != nil {
		report(outcome.String())
	}
	runtime.KeepAlive(object)
	return outcome
}

// DoDragDrop may return NONE for an accepted async transfer. The eventual
// completion effect determines success, including when it wins the snapshot race.
func asyncDragOutcome(hr, effect uint32, started, terminal bool, final dragAsyncEvent) dragOutcome {
	out := dragOutcome{HRESULT: hr, Effect: effect, Pending: started && !terminal, Cancelled: hr == dragdropSCancel}
	if started && terminal {
		if final.State == "error" {
			out.Err = final.Err
		}
		out.Cancelled = out.Cancelled || final.State == "cancelled"
		if final.State == "done" {
			out.Effect = final.Effect
		}
	}
	if int32(hr) < 0 {
		out.Err = fmt.Errorf("DoDragDrop failed: HRESULT 0x%08X", hr)
	}
	return out
}

var dragAsyncSourceVtbl = &[5]uintptr{syscall.NewCallback(dragSourceQueryInterface), syscall.NewCallback(dragSourceAddRef), syscall.NewCallback(dragSourceRelease), syscall.NewCallback(dragAsyncSourceContinue), syscall.NewCallback(dragAsyncSourceFeedback)}

func dragAsyncSourceContinue(this, escape, keyState uintptr) uintptr {
	s := (*dragDropSource)(comRef(this))
	if s.continues == 0 {
		s.firstKeyState = uint32(keyState)
	}
	s.continues++
	s.object.markMovement()
	if escape != 0 {
		s.cancelled = true
		return dragdropSCancel
	}
	return dragButtonAction(keyDown(vkLButton), s.object.moved)
}
func dragAsyncSourceFeedback(this, effect uintptr) uintptr {
	s := (*dragDropSource)(comRef(this))
	s.feedbacks++
	s.object.markMovement()
	return dragdropSUseDefaultCursors
}
