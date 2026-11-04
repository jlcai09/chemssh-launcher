//go:build windows

package webview

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestNativeDragOLESmoke uses the real desktop, OLE modal loop and registered
// IDropTargets, rather than invoking the source's callbacks from the test. Run
// explicitly on an idle, unlocked Windows desktop:
//
//	$env:CHEMSSH_NATIVE_DRAG_SMOKE='1'; go test ./internal/webview -run '^TestNativeDragOLESmoke$' -count=1 -v -timeout=45s
//
// The two visible topmost windows belong only to this test. No Explorer window,
// user directory or existing application receives an input or a drop. Do not run
// this opt-in smoke concurrently with another desktop automation test.
func TestNativeDragOLESmoke(t *testing.T) {
	if os.Getenv("CHEMSSH_NATIVE_DRAG_SMOKE") != "1" {
		t.Skip("set CHEMSSH_NATIVE_DRAG_SMOKE=1 on an idle interactive Windows desktop")
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the smoke IDropTarget POINTL callback currently supports Windows amd64/arm64 only")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := smokeInteractiveDesktop(); err != nil {
		t.Skipf("interactive desktop unavailable: %v", err)
	}
	for _, key := range []uintptr{vkLButton, vkRButton, 0x04, vkEscape, 0x10, 0x11, 0x12} {
		if keyDown(key) {
			t.Skipf("desktop input is in use (virtual key 0x%02X is down)", key)
		}
	}
	var original pointW
	if ok, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&original))); ok == 0 {
		t.Skipf("GetCursorPos: %v", err)
	}
	defer func() {
		ok, _, err := smokeSetCursor.Call(uintptr(original.x), uintptr(original.y))
		var restored pointW
		read, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&restored)))
		if ok == 0 || read == 0 || restored != original {
			t.Errorf("could not verify original cursor position restored: %v", err)
		} else {
			t.Log("original cursor position restored and verified")
		}
	}()
	hr, _, _ := procOleInitialize.Call(0)
	if int32(hr) < 0 {
		t.Fatalf("OleInitialize on locked OS thread: 0x%08X", uint32(hr))
	}
	defer procOleUninitialize.Call()
	uiThread := windows.GetCurrentThreadId()

	root := t.TempDir()
	paths := []string{filepath.Join(root, "first file.txt"), filepath.Join(root, "folder selection")}
	if err := os.WriteFile(paths[0], []byte("native file payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths[1], 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths[1], "nested.txt"), []byte("native folder payload"), 0600); err != nil {
		t.Fatal(err)
	}

	fixture := &smokeFixture{thread: uiThread, paths: paths}
	if err := fixture.open(); err != nil {
		t.Skipf("cannot create unobscured controlled desktop windows: %v", err)
	}
	defer fixture.close()
	t.Logf("real OleInitialize + RegisterDragDrop: OS UI thread=%d; two controlled windows; payload=file+directory", uiThread)

	for _, mode := range []string{"copy", "release-without-movement", "escape-during-staging"} {
		t.Logf("scenario: %s", mode)
		func() {
			state := newSmokeCase(mode)
			fixture.current = state
			fixture.sourceTarget.state = state
			fixture.dropTarget.state = state
			driverDone := make(chan error, 1)
			go func() { driverDone <- fixture.drive(state) }()

			// A NULL HWND filter also pumps the hidden windows created by OLE.
			var message smokeMessage
			for !state.finished {
				got, _, err := smokeGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
				if int32(got) <= 0 {
					t.Fatalf("GetMessage interrupted: result=%d error=%v", int32(got), err)
				}
				smokeTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
				smokeDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
			}
			if err := <-driverDone; err != nil {
				t.Fatalf("%v; outcome=%s", err, state.outcome.String())
			}
			if state.callbackErr != nil {
				t.Fatal(state.callbackErr)
			}
			state.stagingMu.Lock()
			stagingEvents := append([]bool(nil), state.stagingEvents...)
			stagingThreads := append([]uint32(nil), state.stagingThreads...)
			state.stagingMu.Unlock()
			var wantStaging []bool
			if mode != "release-without-movement" {
				wantStaging = []bool{true, false}
			}
			if !reflect.DeepEqual(stagingEvents, wantStaging) {
				t.Fatalf("staging lifecycle = %v, want exactly %v without duplicate notifications", stagingEvents, wantStaging)
			}
			for index, thread := range stagingThreads {
				if thread != uiThread {
					t.Fatalf("staging callback %d ran on OS thread %d, want OLE UI thread %d", index, thread, uiThread)
				}
			}
			t.Logf("staging lifecycle=%v, callback OS threads=%v; exact transitions on OLE UI thread verified", stagingEvents, stagingThreads)
			outcome := state.outcome
			t.Log(outcome.String())
			if outcome.Err != nil {
				t.Fatal(outcome.Err)
			}
			if state.enters == 0 || outcome.Continues == 0 {
				t.Fatal("real OLE did not enter the registered source target and run QueryContinueDrag")
			}
			switch mode {
			case "copy":
				if outcome.HRESULT != dragdropSDrop || outcome.Effect != dropeffectCopy || !outcome.Moved || outcome.Cancelled || outcome.Failed() {
					t.Fatalf("expected completed OLE copy: %+v", outcome)
				}
				if state.stages.Load() != 1 || state.drops != 1 || outcome.FileRequests < 2 {
					t.Fatalf("stage=%d Drop=%d GetData=%d; want one stage, one real Drop, multiple file requests", state.stages.Load(), state.drops, outcome.FileRequests)
				}
				if !reflect.DeepEqual(state.enterPaths, paths) || !reflect.DeepEqual(state.dropPaths, paths) {
					t.Fatal("OS-dispatched DragEnter/Drop did not receive both expected CF_HDROP paths")
				}
				file, err := os.ReadFile(state.dropPaths[0])
				if err != nil || string(file) != "native file payload" {
					t.Fatalf("dropped file unreadable: %v", err)
				}
				folder, err := os.ReadFile(filepath.Join(state.dropPaths[1], "nested.txt"))
				if err != nil || string(folder) != "native folder payload" {
					t.Fatalf("dropped directory unreadable: %v", err)
				}
				t.Log("OS-dispatched DragEnter and Drop decoded CF_HDROP via DragQueryFileW: file + directory, both contents verified; stage once")
			case "release-without-movement":
				if outcome.HRESULT != dragdropSCancel || outcome.Effect != 0 || outcome.Moved || outcome.Failed() || state.stages.Load() != 0 || state.drops != 0 {
					t.Fatalf("stationary release must cancel without staging/drop: %+v, stage=%d Drop=%d", outcome, state.stages.Load(), state.drops)
				}
			case "escape-during-staging":
				if outcome.HRESULT != dragdropSCancel || !outcome.Cancelled || outcome.Effect != 0 || outcome.Failed() || state.stages.Load() != 1 || state.drops != 0 {
					t.Fatalf("Escape must cancel pending real GetData without Drop: %+v, stage=%d Drop=%d", outcome, state.stages.Load(), state.drops)
				}
				if state.cancelLatency > 3*time.Second {
					t.Fatalf("Escape cancellation took %s; expected bounded COM wait", state.cancelLatency)
				}
				t.Logf("real target GetData interrupted by Escape in %s while staging was still blocked; no Drop", state.cancelLatency.Round(time.Millisecond))
			}
		}()
		if t.Failed() {
			break
		}
	}
	runtime.KeepAlive(fixture)
}

var (
	smokeUser32           = syscall.NewLazyDLL("user32.dll")
	smokeRegisterClass    = smokeUser32.NewProc("RegisterClassExW")
	smokeUnregisterClass  = smokeUser32.NewProc("UnregisterClassW")
	smokeCreateWindow     = smokeUser32.NewProc("CreateWindowExW")
	smokeDestroyWindow    = smokeUser32.NewProc("DestroyWindow")
	smokeDefWindowProc    = smokeUser32.NewProc("DefWindowProcW")
	smokeGetMessage       = smokeUser32.NewProc("GetMessageW")
	smokeTranslateMessage = smokeUser32.NewProc("TranslateMessage")
	smokeDispatchMessage  = smokeUser32.NewProc("DispatchMessageW")
	smokePostMessage      = smokeUser32.NewProc("PostMessageW")
	smokeGetForeground    = smokeUser32.NewProc("GetForegroundWindow")
	smokeSetCursor        = smokeUser32.NewProc("SetCursorPos")
	smokeSendInput        = smokeUser32.NewProc("SendInput")
	smokeSystemParameters = smokeUser32.NewProc("SystemParametersInfoW")
	smokeOpenInputDesktop = smokeUser32.NewProc("OpenInputDesktop")
	smokeCloseDesktop     = smokeUser32.NewProc("CloseDesktop")
	smokeGetThreadDesktop = smokeUser32.NewProc("GetThreadDesktop")
	smokeGetObjectInfo    = smokeUser32.NewProc("GetUserObjectInformationW")
	smokeRegisterDrop     = syscall.NewLazyDLL("ole32.dll").NewProc("RegisterDragDrop")
	smokeRevokeDrop       = syscall.NewLazyDLL("ole32.dll").NewProc("RevokeDragDrop")
	smokeReleaseMedium    = syscall.NewLazyDLL("ole32.dll").NewProc("ReleaseStgMedium")
	smokeDragQueryFile    = syscall.NewLazyDLL("shell32.dll").NewProc("DragQueryFileW")
)

const smokeAbortMessage = 0x8000 + 91

type smokeWindowClass struct {
	size, style                        uint32
	callback                           uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
	smallIcon                          uintptr
}

type smokeMessage struct {
	hwnd           uintptr
	message        uint32
	wparam, lparam uintptr
	time           uint32
	point          pointW
	private        uint32
}

type smokeMouseInput struct {
	dx, dy            int32
	data, flags, time uint32
	extra             uintptr
}

type smokeInput struct {
	kind uint32
	data smokeMouseInput
}

type smokeCase struct {
	mode                                             string
	entered, external, requested, supplied, returned chan struct{}
	stagePermit, stageDone                           chan struct{}
	stageOnce                                        sync.Once
	stages                                           atomic.Int32
	stagingMu                                        sync.Mutex
	stagingEvents                                    []bool
	stagingThreads                                   []uint32
	finished                                         bool
	enters, drops                                    int
	enterPaths, dropPaths                            []string
	outcome                                          dragOutcome
	callbackErr                                      error
	cancelLatency                                    time.Duration
}

func newSmokeCase(mode string) *smokeCase {
	return &smokeCase{mode: mode, entered: make(chan struct{}), external: make(chan struct{}), requested: make(chan struct{}), supplied: make(chan struct{}), returned: make(chan struct{}), stagePermit: make(chan struct{}), stageDone: make(chan struct{})}
}

func smokeSignal(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (s *smokeCase) permit() { s.stageOnce.Do(func() { close(s.stagePermit) }) }

func smokeWait(ch <-chan struct{}, what string) error {
	select {
	case <-ch:
		return nil
	case <-time.After(4 * time.Second):
		return fmt.Errorf("timed out waiting for %s (desktop may not be interactive)", what)
	}
}

type smokeFixture struct {
	thread                   uint32
	paths                    []string
	source, target           uintptr
	sourcePoint, targetPoint pointW
	className                *uint16
	callback                 uintptr
	current                  *smokeCase
	sourceTarget, dropTarget *smokeDropTarget
}

func (f *smokeFixture) open() error {
	name, _ := syscall.UTF16PtrFromString(fmt.Sprintf("ChemSSHNativeDragSmoke_%d_%d", os.Getpid(), f.thread))
	f.className = name
	f.callback = syscall.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		if windows.GetCurrentThreadId() != f.thread {
			panic("native smoke window callback left its OS UI thread")
		}
		s := f.current
		if s != nil && hwnd == f.source {
			switch message {
			case 0x0201: // WM_LBUTTONDOWN, generated by SendInput
				s.outcome = dragFilesOut(f.source, `{"chemssh":"native-smoke"}`, func() ([]string, error) {
					s.stages.Add(1)
					smokeSignal(s.requested)
					<-s.stagePermit
					defer close(s.stageDone)
					return f.paths, nil
				}, nil, func(active bool) {
					// Retain raw notifications: assertions must detect duplicate
					// transitions and worker-thread delivery rather than mask them.
					s.stagingMu.Lock()
					s.stagingEvents = append(s.stagingEvents, active)
					s.stagingThreads = append(s.stagingThreads, windows.GetCurrentThreadId())
					s.stagingMu.Unlock()
				})
				s.finished = true
				smokeSignal(s.returned)
				return 0
			case smokeAbortMessage:
				s.finished = true
				return 0
			}
		}
		result, _, _ := smokeDefWindowProc.Call(hwnd, message, wparam, lparam)
		return result
	})
	class := smokeWindowClass{size: uint32(unsafe.Sizeof(smokeWindowClass{})), callback: f.callback, name: name, background: 6}
	if atom, _, err := smokeRegisterClass.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("RegisterClassExW: %v", err)
	}
	var work struct{ left, top, right, bottom int32 }
	if ok, _, err := smokeSystemParameters.Call(0x0030, 0, uintptr(unsafe.Pointer(&work)), 0); ok == 0 || work.right-work.left < 620 || work.bottom-work.top < 300 {
		f.close()
		return fmt.Errorf("work area too small or unavailable: %v", err)
	}
	f.sourcePoint = pointW{work.left + 170, work.top + 170}
	f.targetPoint = pointW{work.left + 470, work.top + 170}
	create := func(title string, x int32) (uintptr, error) {
		caption, _ := syscall.UTF16PtrFromString(title)
		// WS_EX_TOPMOST | WS_EX_TOOLWINDOW; WS_POPUP | WS_VISIBLE | WS_BORDER.
		hwnd, _, err := smokeCreateWindow.Call(0x88, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(caption)), 0x90800000, uintptr(x), uintptr(work.top+50), 260, 240, 0, 0, 0, 0)
		if hwnd == 0 {
			return 0, fmt.Errorf("CreateWindowExW: %v", err)
		}
		return hwnd, nil
	}
	var err error
	f.source, err = create("ChemSSH native smoke: source", work.left+40)
	if err == nil {
		f.target, err = create("ChemSSH native smoke: safe target", work.left+340)
	}
	if err != nil {
		f.close()
		return err
	}
	f.sourceTarget = newSmokeTarget(f.thread, false)
	f.dropTarget = newSmokeTarget(f.thread, true)
	for _, entry := range []struct {
		hwnd   uintptr
		object *smokeDropTarget
	}{{f.source, f.sourceTarget}, {f.target, f.dropTarget}} {
		hr, _, _ := smokeRegisterDrop.Call(entry.hwnd, uintptr(unsafe.Pointer(entry.object)))
		if uint32(hr) != sOK {
			f.close()
			return fmt.Errorf("RegisterDragDrop: 0x%08X", uint32(hr))
		}
		entry.object.registered = true
	}
	// A background test runner may not call SetForegroundWindow successfully.
	// The real left press on our unobscured source activates it naturally.
	if sourceHit, targetHit := windowAtPoint(f.sourcePoint), windowAtPoint(f.targetPoint); sourceHit != f.source || targetHit != f.target {
		f.close()
		return fmt.Errorf("controlled points obscured: source own=%t target own=%t", sourceHit == f.source, targetHit == f.target)
	}
	return nil
}

func (f *smokeFixture) close() {
	for _, entry := range []struct {
		hwnd   uintptr
		object *smokeDropTarget
	}{{f.target, f.dropTarget}, {f.source, f.sourceTarget}} {
		if entry.hwnd != 0 {
			if entry.object != nil && entry.object.registered {
				smokeRevokeDrop.Call(entry.hwnd)
				entry.object.registered = false
			}
			smokeDestroyWindow.Call(entry.hwnd)
		}
	}
	f.source, f.target = 0, 0
	if f.className != nil {
		smokeUnregisterClass.Call(uintptr(unsafe.Pointer(f.className)), 0)
	}
}

func (f *smokeFixture) ownForeground() bool {
	foreground, _, _ := smokeGetForeground.Call()
	return foreground == f.source || foreground == f.target
}

func (f *smokeFixture) move(point pointW, hwnd uintptr) error {
	if !f.ownForeground() {
		return fmt.Errorf("foreground changed outside the controlled windows; input stopped")
	}
	if windowAtPoint(point) != hwnd {
		return fmt.Errorf("controlled input point is obscured; input stopped")
	}
	if ok, _, err := smokeSetCursor.Call(uintptr(point.x), uintptr(point.y)); ok == 0 {
		return fmt.Errorf("SetCursorPos: %v", err)
	}
	return nil
}

func smokeMouse(flags uint32) error {
	input := smokeInput{data: smokeMouseInput{flags: flags}}
	count, _, err := smokeSendInput.Call(1, uintptr(unsafe.Pointer(&input)), unsafe.Sizeof(input))
	runtime.KeepAlive(&input)
	if count != 1 {
		return fmt.Errorf("SendInput(mouse): sent=%d error=%v", count, err)
	}
	return nil
}

func smokeEscape(up bool) error {
	input := smokeInput{kind: 1}
	keyboard := (*struct {
		vk, scan    uint16
		flags, time uint32
		extra       uintptr
	})(unsafe.Pointer(&input.data))
	keyboard.vk = uint16(vkEscape)
	if up {
		keyboard.flags = 2
	}
	count, _, err := smokeSendInput.Call(1, uintptr(unsafe.Pointer(&input)), unsafe.Sizeof(input))
	runtime.KeepAlive(&input)
	if count != 1 {
		return fmt.Errorf("SendInput(Escape): sent=%d error=%v", count, err)
	}
	return nil
}

func (f *smokeFixture) drive(s *smokeCase) (result error) {
	pressed, escaped := false, false
	defer func() {
		// Always unblock the staging worker, including failed input handshakes.
		s.permit()
		if pressed || escaped {
			// Release only over a window we created; never deliver a click/key-up
			// to whatever existing application happens to be under the cursor.
			if f.ownForeground() && windowAtPoint(f.sourcePoint) == f.source {
				smokeSetCursor.Call(uintptr(f.sourcePoint.x), uintptr(f.sourcePoint.y))
				if pressed {
					_ = smokeMouse(0x0004)
				}
				if escaped {
					_ = smokeEscape(true)
				}
			} else if result == nil {
				result = fmt.Errorf("desktop ownership changed before synthetic input cleanup")
			}
		}
		if s.stages.Load() != 0 {
			if err := smokeWait(s.stageDone, "staging worker cleanup"); result == nil {
				result = err
			}
		}
		select {
		case <-s.returned:
		default:
			smokePostMessage.Call(f.source, smokeAbortMessage, 0, 0)
		}
	}()
	if windowAtPoint(f.sourcePoint) != f.source {
		return fmt.Errorf("controlled source point is obscured before initial press")
	}
	if ok, _, err := smokeSetCursor.Call(uintptr(f.sourcePoint.x), uintptr(f.sourcePoint.y)); ok == 0 {
		return fmt.Errorf("initial SetCursorPos: %v", err)
	}
	if err := smokeMouse(0x0002); err != nil {
		return err
	}
	pressed = true
	// DragEnter on our registered source proves DoDragDrop recorded its origin
	// and entered its native loop before the input driver moves the pointer.
	if err := smokeWait(s.entered, "real OLE source DragEnter"); err != nil {
		return err
	}
	if s.mode == "release-without-movement" {
		if err := smokeMouse(0x0004); err != nil {
			return err
		}
		pressed = false
		return smokeWait(s.returned, "stationary OLE cancellation")
	}
	if err := f.move(f.targetPoint, f.target); err != nil {
		return err
	}
	if err := smokeWait(s.external, "real external target DragEnter"); err != nil {
		return err
	}
	if err := smokeWait(s.requested, "external staging callback"); err != nil {
		return err
	}
	if s.mode == "escape-during-staging" {
		if !f.ownForeground() {
			return fmt.Errorf("foreground changed before Escape; input stopped")
		}
		started := time.Now()
		if err := smokeEscape(false); err != nil {
			return err
		}
		escaped = true
		if err := smokeWait(s.returned, "Escape cancellation while GetData waits"); err != nil {
			return err
		}
		s.cancelLatency = time.Since(started)
		return nil
	}
	s.permit()
	if err := smokeWait(s.supplied, "target received staged CF_HDROP"); err != nil {
		return err
	}
	if err := f.move(f.targetPoint, f.target); err != nil {
		return err
	}
	if err := smokeMouse(0x0004); err != nil {
		return err
	}
	pressed = false
	return smokeWait(s.returned, "completed OLE copy")
}

// The OS calls this real IDropTarget after RegisterDragDrop. The POINTL value
// occupies one 64-bit argument in these callbacks, matching the Win64 ABI.
type smokeDropTarget struct {
	vtbl                 *[7]uintptr
	refs                 atomic.Int32
	thread               uint32
	external, registered bool
	state                *smokeCase
	format               formatEtcW
	medium               stgMediumW
}

func newSmokeTarget(thread uint32, external bool) *smokeDropTarget {
	target := &smokeDropTarget{thread: thread, external: external}
	target.refs.Store(1)
	target.vtbl = &[7]uintptr{syscall.NewCallback(smokeTargetQuery), syscall.NewCallback(smokeTargetAddRef), syscall.NewCallback(smokeTargetRelease), syscall.NewCallback(smokeTargetEnter), syscall.NewCallback(smokeTargetOver), syscall.NewCallback(smokeTargetLeave), syscall.NewCallback(smokeTargetDrop)}
	return target
}

func smokeTargetQuery(this, iid, out uintptr) uintptr {
	if out == 0 {
		return eInvalidArg
	}
	*(*uintptr)(comRef(out)) = 0
	want := guidW{0x22, 0x01, 0, 0, 0, 0, 0, 0, 0xC0, 0, 0, 0, 0, 0, 0, 0x46}
	if iid != 0 && (*(*guidW)(comRef(iid)) == guidUnknown || *(*guidW)(comRef(iid)) == want) {
		*(*uintptr)(comRef(out)) = this
		smokeTargetAddRef(this)
		return sOK
	}
	return eNoInterface
}

func smokeTargetAddRef(this uintptr) uintptr {
	return uintptr((*smokeDropTarget)(comRef(this)).refs.Add(1))
}
func smokeTargetRelease(this uintptr) uintptr {
	return uintptr((*smokeDropTarget)(comRef(this)).refs.Add(-1))
}

func smokeTargetEnter(this, data, keys, point, effect uintptr) uintptr {
	target := (*smokeDropTarget)(comRef(this))
	s := target.state
	if s == nil {
		*(*uint32)(comRef(effect)) = 0
		return sOK
	}
	if windows.GetCurrentThreadId() != target.thread {
		s.callbackErr = fmt.Errorf("IDropTarget called on wrong OS thread")
		return eFail
	}
	if !target.external {
		s.enters++
		*(*uint32)(comRef(effect)) = dropeffectCopy
		smokeSignal(s.entered)
		return sOK
	}
	smokeSignal(s.external)
	paths, err := target.readFiles(data)
	if err != nil {
		if s.mode != "escape-during-staging" {
			s.callbackErr = err
		}
		*(*uint32)(comRef(effect)) = 0
		return sOK
	}
	s.enterPaths = paths
	*(*uint32)(comRef(effect)) = dropeffectCopy
	smokeSignal(s.supplied)
	return sOK
}

func smokeTargetOver(this, keys, point, effect uintptr) uintptr {
	*(*uint32)(comRef(effect)) = dropeffectCopy
	return sOK
}
func smokeTargetLeave(this uintptr) uintptr { return sOK }

func smokeTargetDrop(this, data, keys, point, effect uintptr) uintptr {
	target := (*smokeDropTarget)(comRef(this))
	s := target.state
	s.drops++
	paths, err := target.readFiles(data)
	if err != nil {
		s.callbackErr = err
		*(*uint32)(comRef(effect)) = 0
		return eFail
	}
	s.dropPaths = paths
	*(*uint32)(comRef(effect)) = dropeffectCopy
	return sOK
}

func (target *smokeDropTarget) readFiles(data uintptr) ([]string, error) {
	if data == 0 {
		return nil, fmt.Errorf("OS supplied no IDataObject")
	}
	vtbl := *(**[12]uintptr)(comRef(data))
	target.format = formatEtcW{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	target.medium = stgMediumW{}
	hr, _, _ := syscall.SyscallN(vtbl[5], data, uintptr(unsafe.Pointer(&target.format)))
	if uint32(hr) != sOK {
		return nil, fmt.Errorf("OS target QueryGetData(CF_HDROP): 0x%08X", uint32(hr))
	}
	hr, _, _ = syscall.SyscallN(vtbl[3], data, uintptr(unsafe.Pointer(&target.format)), uintptr(unsafe.Pointer(&target.medium)))
	if uint32(hr) != sOK {
		return nil, fmt.Errorf("OS target GetData(CF_HDROP): 0x%08X", uint32(hr))
	}
	defer smokeReleaseMedium.Call(uintptr(unsafe.Pointer(&target.medium)))
	if target.medium.tymed != tymedHG || target.medium.hGlobal == 0 {
		return nil, fmt.Errorf("invalid CF_HDROP storage medium")
	}
	count, _, _ := smokeDragQueryFile.Call(target.medium.hGlobal, 0xFFFFFFFF, 0, 0)
	if count == 0 || count > 16 {
		return nil, fmt.Errorf("DragQueryFileW count=%d", count)
	}
	paths := make([]string, 0, count)
	for index := uintptr(0); index < count; index++ {
		length, _, _ := smokeDragQueryFile.Call(target.medium.hGlobal, index, 0, 0)
		if length == 0 || length > 32767 {
			return nil, fmt.Errorf("DragQueryFileW invalid path length=%d", length)
		}
		buffer := make([]uint16, length+1)
		smokeDragQueryFile.Call(target.medium.hGlobal, index, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		paths = append(paths, syscall.UTF16ToString(buffer))
	}
	runtime.KeepAlive(target)
	return paths, nil
}

func smokeInteractiveDesktop() error {
	input, _, err := smokeOpenInputDesktop.Call(0, 0, 1) // DESKTOP_READOBJECTS
	if input == 0 {
		return fmt.Errorf("OpenInputDesktop: %v", err)
	}
	defer smokeCloseDesktop.Call(input)
	threadDesktop, _, err := smokeGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if threadDesktop == 0 {
		return fmt.Errorf("GetThreadDesktop: %v", err)
	}
	name := func(handle uintptr) (string, error) {
		var buffer [256]uint16
		var needed uint32
		if ok, _, err := smokeGetObjectInfo.Call(handle, 2, uintptr(unsafe.Pointer(&buffer[0])), unsafe.Sizeof(buffer), uintptr(unsafe.Pointer(&needed))); ok == 0 {
			return "", fmt.Errorf("GetUserObjectInformation(UOI_NAME): %v", err)
		}
		return syscall.UTF16ToString(buffer[:]), nil
	}
	inputName, err := name(input)
	if err != nil {
		return err
	}
	threadName, err := name(threadDesktop)
	if err != nil {
		return err
	}
	if inputName != threadName || inputName != "Default" {
		return fmt.Errorf("input desktop=%q thread desktop=%q; unlocked Default desktop required", inputName, threadName)
	}
	return nil
}
