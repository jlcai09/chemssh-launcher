//go:build windows

package webview

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func asyncTestCall(pointer uintptr, index int, args ...uintptr) uint32 {
	vtbl := *(**[12]uintptr)(comRef(pointer))
	argv := append([]uintptr{pointer}, args...)
	hr, _, _ := syscall.SyscallN(vtbl[index], argv...)
	return uint32(hr)
}
func asyncTestQuery(pointer uintptr, iid *guidW) (uintptr, error) {
	var out uintptr
	hr := asyncTestCall(pointer, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if int32(hr) < 0 || out == 0 {
		return 0, fmt.Errorf("QueryInterface: 0x%08X", hr)
	}
	return out, nil
}
func asyncTestSTA(t *testing.T) {
	t.Helper()
	runtime.LockOSThread()
	hr, _, _ := procOleInitialize.Call(0)
	if int32(hr) < 0 {
		runtime.UnlockOSThread()
		t.Fatalf("OleInitialize: %08X", hr)
	}
	t.Cleanup(func() { procOleUninitialize.Call(); runtime.UnlockOSThread() })
}

func TestAsyncDragMarshalingDoesNotNeedSourceSTAPump(t *testing.T) {
	asyncTestSTA(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	events := make(chan dragAsyncEvent, 8)
	var stages atomic.Int32
	object, proxy, err := newAsyncDragObject("marker", func() ([]string, error) {
		stages.Add(1)
		close(entered)
		<-release
		return []string{`C:\Users\user\file.txt`}, nil
	}, func(e dragAsyncEvent) { events <- e }, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragCOM(proxy)
	async, err := asyncTestQuery(proxy, &guidDragAsync)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragCOM(async)
	var mode int32
	if hr := asyncTestCall(async, 4, uintptr(unsafe.Pointer(&mode))); hr != sOK || mode != 1 {
		t.Fatalf("GetAsyncMode: %08X %d", hr, mode)
	}
	format := formatEtcW{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	var medium stgMediumW
	if hr := asyncTestCall(proxy, 3, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium))); hr != dvEFormatEtc {
		t.Fatalf("hover GetData must refuse without IO: %08X", hr)
	}
	if stages.Load() != 0 {
		t.Fatal("probe started staging")
	}
	if hr := asyncTestCall(async, 5, 0); hr != sOK {
		t.Fatalf("StartOperation: %08X", hr)
	}
	if (<-events).State != "started" {
		t.Fatal("missing start")
	}
	// Marshal the original identity from the source STA to a second STA. Its
	// calls must execute on the MTA even while this STA does not pump messages.
	var stream uintptr
	hr, _, _ := procDragMarshal.Call(uintptr(unsafe.Pointer(&guidDataObject)), proxy, uintptr(unsafe.Pointer(&stream)))
	if int32(hr) < 0 {
		t.Fatalf("worker marshal: %08X", hr)
	}
	finished := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		h, _, _ := procDragCoInitializeEx.Call(0, 2)
		if int32(h) < 0 {
			finished <- fmt.Errorf("worker init: %08X", h)
			return
		}
		defer procDragCoUninitialize.Call()
		var data uintptr
		h, _, _ = procDragUnmarshal.Call(stream, uintptr(unsafe.Pointer(&guidDataObject)), uintptr(unsafe.Pointer(&data)))
		if int32(h) < 0 {
			finished <- fmt.Errorf("worker unmarshal: %08X", h)
			return
		}
		defer releaseDragCOM(data)
		var m stgMediumW
		if h := asyncTestCall(data, 3, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&m))); h != sOK {
			finished <- fmt.Errorf("worker GetData: %08X", h)
			return
		}
		if m.hGlobal == 0 {
			finished <- fmt.Errorf("empty medium")
			return
		}
		globalFree(m.hGlobal)
		a, e := asyncTestQuery(data, &guidDragAsync)
		if e != nil {
			finished <- e
			return
		}
		defer releaseDragCOM(a)
		h = uintptr(asyncTestCall(a, 7, sOK, 0, dropeffectCopy))
		if h != sOK {
			finished <- fmt.Errorf("EndOperation: %08X", h)
			return
		}
		finished <- nil
	}()
	object.async.loopOnce.Do(func() { close(object.async.loopDone) })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("background GetData requires the source STA to pump")
	}
	select {
	case e := <-events:
		t.Fatalf("completed before staging: %+v", e)
	default:
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker blocked after staging")
	}
	select {
	case event := <-events:
		if event.State != "done" {
			t.Fatalf("terminal: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no EndOperation event")
	}
	if stages.Load() != 1 {
		t.Fatal("staging count", stages.Load())
	}
	if hr := asyncTestCall(async, 7, eFail, 0, 0); hr != sOK {
		t.Fatal("duplicate EndOperation", hr)
	}
	select {
	case e := <-events:
		t.Fatalf("duplicate terminal: %+v", e)
	default:
	}
}

func TestAsyncDragOutcomeUsesEndOperationInsteadOfTemporaryEffect(t *testing.T) {
	pending := asyncDragOutcome(dragdropSDrop, 0, true, false, dragAsyncEvent{})
	pending.Moved = true
	if !pending.Pending || pending.Failed() {
		t.Fatalf("accepted NONE must remain pending: %+v", pending)
	}
	done := asyncDragOutcome(dragdropSDrop, 0, true, true, dragAsyncEvent{State: "done", Effect: dropeffectCopy})
	done.Moved = true
	if done.Pending || done.Failed() {
		t.Fatalf("rapid EndOperation success was lost: %+v", done)
	}
	cancelled := asyncDragOutcome(dragdropSDrop, 0, true, true, dragAsyncEvent{State: "cancelled", Err: context.Canceled})
	cancelled.Moved = true
	if !cancelled.Cancelled || cancelled.Failed() {
		t.Fatalf("cancelled operation reported failure: %+v", cancelled)
	}
}

func TestAsyncDragSynchronousGetDataCannotDeadlockLoop(t *testing.T) {
	asyncTestSTA(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stages atomic.Int32
	_, proxy, err := newAsyncDragObject("marker", func() ([]string, error) { stages.Add(1); return nil, nil }, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragCOM(proxy)
	a, err := asyncTestQuery(proxy, &guidDragAsync)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragCOM(a)
	if hr := asyncTestCall(a, 5, 0); hr != sOK {
		t.Fatal(hr)
	}
	format := formatEtcW{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	var medium stgMediumW
	start := time.Now()
	hr := asyncTestCall(proxy, 3, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium)))
	if hr != dvEFormatEtc || time.Since(start) > time.Second || stages.Load() != 0 {
		t.Fatalf("synchronous GetData waited/staged: %08X %s %d", hr, time.Since(start), stages.Load())
	}
}

func TestAsyncDragCancelWithoutGetDataReleasesOperation(t *testing.T) {
	asyncTestSTA(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan dragAsyncEvent, 4)
	object, proxy, err := newAsyncDragObject("marker", func() ([]string, error) { t.Error("unexpected staging"); return nil, nil }, func(e dragAsyncEvent) { events <- e }, ctx)
	if err != nil {
		t.Fatal(err)
	}
	async, err := asyncTestQuery(proxy, &guidDragAsync)
	if err != nil {
		releaseDragCOM(proxy)
		t.Fatal(err)
	}
	if hr := asyncTestCall(async, 5, 0); hr != sOK {
		t.Fatalf("start: %08X", hr)
	}
	cancel()
	releaseDragCOM(async)
	releaseDragCOM(proxy)
	select {
	case <-object.async.released:
	case <-time.After(3 * time.Second):
		t.Fatal("operation self-reference leaked after cancellation")
	}
	if (<-events).State != "started" || (<-events).State != "cancelled" {
		t.Fatal("wrong lifecycle")
	}
}
