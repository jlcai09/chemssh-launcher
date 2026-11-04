//go:build windows

package webview

import (
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Equal-length array assignments enforce the native ABI during cross-compilation
// too: a 386 test executable must fail to compile if padding regresses.
var (
	_ [unsafe.Sizeof(formatEtcW{})]byte                  = [8 + 3*unsafe.Sizeof(uintptr(0))]byte{}
	_ [unsafe.Offsetof(formatEtcW{}.ptd)]byte            = [unsafe.Sizeof(uintptr(0))]byte{}
	_ [unsafe.Offsetof(formatEtcW{}.dwAspect)]byte       = [2 * unsafe.Sizeof(uintptr(0))]byte{}
	_ [unsafe.Offsetof(formatEtcW{}.lindex)]byte         = [2*unsafe.Sizeof(uintptr(0)) + 4]byte{}
	_ [unsafe.Offsetof(formatEtcW{}.tymed)]byte          = [2*unsafe.Sizeof(uintptr(0)) + 8]byte{}
	_ [unsafe.Sizeof(stgMediumW{})]byte                  = [3 * unsafe.Sizeof(uintptr(0))]byte{}
	_ [unsafe.Offsetof(stgMediumW{}.hGlobal)]byte        = [unsafe.Sizeof(uintptr(0))]byte{}
	_ [unsafe.Offsetof(stgMediumW{}.pUnkForRelease)]byte = [2 * unsafe.Sizeof(uintptr(0))]byte{}
)

func TestInitializeDragOLEPairsSuccessfulCalls(t *testing.T) {
	for _, hr := range []uint32{sOK, sFalse, 0x80010106, eOutOfMemory} {
		initialized, uninitialized := 0, 0
		got, cleanup := initializeDragOLE(func() uintptr { initialized++; return uintptr(hr) }, func() { uninitialized++ })
		if initialized != 1 || got != hr {
			t.Fatalf("initialize = (%d, %08X), want (1, %08X)", initialized, got, hr)
		}
		if int32(hr) < 0 {
			if cleanup != nil || uninitialized != 0 {
				t.Fatalf("failed initialization 0x%08X provided cleanup", hr)
			}
			continue
		}
		if cleanup == nil {
			t.Fatalf("successful initialization 0x%08X missing cleanup", hr)
		}
		cleanup()
		if uninitialized != 1 {
			t.Fatalf("OleUninitialize count = %d, want 1", uninitialized)
		}
	}
}

func TestPointerOverExternalWindowUsesTopLevelOwnership(t *testing.T) {
	roots := map[uintptr]uintptr{1: 1, 2: 1, 3: 3, 4: 3}
	for _, tc := range []struct {
		source, target uintptr
		want           bool
	}{
		{1, 1, false}, {1, 2, false}, {2, 1, false}, {2, 2, false},
		{1, 3, true}, {2, 4, true}, {0, 3, false}, {1, 0, false}, {1, 9, false},
	} {
		if got := pointerOverExternalWindow(tc.source, tc.target, func(hwnd uintptr) uintptr { return roots[hwnd] }); got != tc.want {
			t.Fatalf("source=%d target=%d external=%t, want %t", tc.source, tc.target, got, tc.want)
		}
	}
}

func TestWindowPointArgsPreservesSignedCoordinates(t *testing.T) {
	point := pointW{x: -123, y: -456}
	args := windowPointArgs(point)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if len(args) != 1 || uint32(args[0]) != uint32(point.x) || uint32(uint64(args[0])>>32) != uint32(point.y) {
			t.Fatalf("packed POINT = %X", args)
		}
	} else if len(args) != 2 || args[0] != uintptr(uint32(point.x)) || args[1] != uintptr(uint32(point.y)) {
		t.Fatalf("stack POINT = %X", args)
	}
}

func TestDragFilesOutRejectsReleasedInitiatingButton(t *testing.T) {
	if keyDown(vkLButton) {
		t.Skip("the initiating button is physically held")
	}
	staged := false
	outcome := dragFilesOut(0, `{}`, func() ([]string, error) {
		staged = true
		return nil, nil
	}, nil)
	if !outcome.Cancelled || outcome.Err != nil || staged || outcome.Continues != 0 {
		t.Fatalf("released-before-dispatch drag = %+v, staged=%t", outcome, staged)
	}
}

func TestDragButtonAction(t *testing.T) {
	for _, tc := range []struct {
		down, moved bool
		want        uintptr
	}{
		{true, false, sOK}, {true, true, sOK},
		{false, false, dragdropSCancel}, {false, true, dragdropSDrop},
	} {
		if hr := dragButtonAction(tc.down, tc.moved); hr != tc.want {
			t.Fatalf("buttonDown=%t moved=%t: 0x%08X, want 0x%08X", tc.down, tc.moved, hr, tc.want)
		}
	}
}

func TestPollDragStageWaitsBeyondOldLimit(t *testing.T) {
	ready := make(chan struct{})
	polls := 0
	if err := pollDragStage(ready, func() bool { return false }, func() uint32 {
		polls++
		if polls == 700 {
			close(ready)
			return sOK
		}
		return rpcSCallPending
	}); err != nil || polls != 700 {
		t.Fatalf("long staging wait: polls=%d err=%v", polls, err)
	}
}

func TestPollDragStageCancellationAndFailure(t *testing.T) {
	ready := make(chan struct{})
	if err := pollDragStage(ready, func() bool { return true }, func() uint32 { t.Fatal("wait after cancellation"); return sOK }); err != errStagingCancelled {
		t.Fatalf("cancelled staging = %v", err)
	}
	if err := pollDragStage(ready, func() bool { return false }, func() uint32 { return eFail }); err == nil {
		t.Fatal("COM wait failure was ignored")
	}
}

func TestWaitForDragStageSignalsWindowsEvent(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, cleanup := initializeDragOLE(func() uintptr { hr, _, _ := procOleInitialize.Call(0); return hr }, func() { procOleUninitialize.Call() })
	if cleanup == nil {
		t.Fatalf("OleInitialize = 0x%08X", hr)
	}
	defer cleanup()
	ready := make(chan struct{})
	timer := time.AfterFunc(20*time.Millisecond, func() { close(ready) })
	defer timer.Stop()
	if err := waitForDragStage(ready, func() bool { return false }); err != nil {
		t.Fatalf("waitForDragStage = %v", err)
	}
	if err := waitForDragStage(make(chan struct{}), func() bool { return true }); err != errStagingCancelled {
		t.Fatalf("cancelled Windows event wait = %v", err)
	}
}

func TestDropFilesHandleCarriesEveryPath(t *testing.T) {
	paths := []string{`C:\Users\user\Desktop\a.txt`, `C:\Users\user\Desktop\b.txt`}
	handle, err := newDropFilesHandle(paths)
	if err != nil {
		t.Fatalf("newDropFilesHandle: %v", err)
	}
	defer globalFree(handle)

	base := globalLock(handle)
	if base == nil {
		t.Fatal("could not lock the drag payload")
	}
	defer globalUnlock(handle)

	header := (*dropFilesW)(base)
	if header.pFiles != uint32(unsafe.Sizeof(dropFilesW{})) {
		t.Fatalf("pFiles = %d, want %d", header.pFiles, unsafe.Sizeof(dropFilesW{}))
	}
	if header.fWide != 1 {
		t.Fatalf("fWide = %d, want 1 (UTF-16 list)", header.fWide)
	}

	var got []string
	cursor := unsafe.Add(base, uintptr(header.pFiles))
	for *(*uint16)(cursor) != 0 {
		length := 0
		for *(*uint16)(unsafe.Add(cursor, uintptr(length)*2)) != 0 {
			length++
		}
		got = append(got, syscall.UTF16ToString(unsafe.Slice((*uint16)(cursor), length)))
		cursor = unsafe.Add(cursor, uintptr(length+1)*2)
	}
	// Explorer reads past the list unless it ends with a second NUL, so the loop
	// above has to stop at that terminator rather than at the last name.
	if *(*uint16)(cursor) != 0 {
		t.Fatal("payload is not double-NUL terminated")
	}
	if len(got) != len(paths) {
		t.Fatalf("payload lists %d paths, want %d: %v", len(got), len(paths), got)
	}
	for index, path := range paths {
		if got[index] != path {
			t.Fatalf("path %d = %q, want %q", index, got[index], path)
		}
	}
}

func TestDropFilesHandleRejectsEmptyList(t *testing.T) {
	if _, err := newDropFilesHandle(nil); err == nil {
		t.Fatal("expected an error for an empty file list")
	}
}

func TestGlobalTextHandleKeepsMarker(t *testing.T) {
	const marker = `{"chemssh":"chemssh-launcher-file-transfer"}`
	units, err := syscall.UTF16FromString(marker)
	if err != nil {
		t.Fatalf("UTF16FromString: %v", err)
	}
	handle, err := newGlobalTextHandle(units)
	if err != nil {
		t.Fatalf("newGlobalTextHandle: %v", err)
	}
	defer globalFree(handle)

	base := globalLock(handle)
	if base == nil {
		t.Fatal("could not lock the drag marker")
	}
	defer globalUnlock(handle)

	if got := syscall.UTF16ToString(unsafe.Slice((*uint16)(base), len(units))); got != marker {
		t.Fatalf("marker = %q, want %q", got, marker)
	}
}
