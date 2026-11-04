//go:build windows && webview2

package webview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// TestNativeDragWebViewStagingSmoke verifies actual JavaScript delivery while
// real OLE GetData is waiting. Opt in only on an idle interactive desktop.
func TestNativeDragWebViewStagingSmoke(t *testing.T) {
	if os.Getenv("CHEMSSH_NATIVE_DRAG_UI_SMOKE") != "1" {
		t.Skip("set CHEMSSH_NATIVE_DRAG_UI_SMOKE=1 on an idle interactive desktop")
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("controlled smoke target requires 64-bit Windows")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := smokeInteractiveDesktop(); err != nil {
		t.Skip(err)
	}
	for _, key := range []uintptr{vkLButton, vkRButton, vkEscape} {
		if keyDown(key) {
			t.Skip("desktop input is in use")
		}
	}
	var original pointW
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&original)))
	defer smokeSetCursor.Call(uintptr(original.x), uintptr(original.y))
	hr, cleanup := initializeDragOLE(func() uintptr { hr, _, _ := procOleInitialize.Call(0); return hr }, func() { procOleUninitialize.Call() })
	if cleanup == nil {
		t.Fatalf("OleInitialize = %08X", hr)
	}
	defer cleanup()
	ready, received := make(chan struct{}), make(chan struct{})
	var readyOnce, receivedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			readyOnce.Do(func() { close(ready) })
		case "/staging":
			receivedOnce.Do(func() { close(received) })
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	profile := t.TempDir()
	w := webview2.NewWithOptions(webview2.WebViewOptions{DataPath: profile, WindowOptions: webview2.WindowOptions{Title: "ChemSSH staging event smoke", Width: 300, Height: 200}})
	if w == nil {
		t.Fatal("WebView2 unavailable")
	}
	defer func() {
		// The wrapper's Destroy only posts WM_CLOSE and never closes its browser
		// controller. Close this test-owned controller before profile cleanup.
		if chromium, ok := chromiumFromWebView(w); ok {
			address := reflect.ValueOf(chromium).Elem().FieldByName("controller").Pointer()
			if address != 0 {
				vtable := *(**[26]uintptr)(comRef(address))
				syscall.SyscallN(vtable[24], address) // ICoreWebView2Controller::Close
			}
		}
		smokeDestroyWindow.Call(uintptr(w.Window()))
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := os.RemoveAll(profile); err == nil {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	w.SetHtml(fmt.Sprintf(`<html><body><div id="status">Staging event smoke</div><script>window.addEventListener('chemssh-native-drag-staging', e => {if(e.detail.caching) {document.getElementById('status').textContent='Caching'; requestAnimationFrame(()=>requestAnimationFrame(()=>fetch('%s/staging')))}});fetch('%s/ready')</script></body></html>`, server.URL, server.URL))
	fixture := &smokeFixture{thread: windows.GetCurrentThreadId(), paths: []string{`C:\Users\user\Desktop\unused.txt`}}
	if err := fixture.open(); err != nil {
		t.Skip(err)
	}
	defer fixture.close()
	state := newSmokeCase("copy")
	fixture.current, fixture.sourceTarget.state, fixture.dropTarget.state = state, state, state
	setPtr := smokeUser32.NewProc("SetWindowLongPtrW")
	callProc := smokeUser32.NewProc("CallWindowProcW")
	var previous uintptr
	callback := syscall.NewCallback(func(hwnd, message, wp, lp uintptr) uintptr {
		if message == 0x0201 {
			state.outcome = dragFilesOut(fixture.source, `{}`, func() ([]string, error) {
				state.stages.Add(1)
				smokeSignal(state.requested)
				<-state.stagePermit
				close(state.stageDone)
				return fixture.paths, nil
			}, nil, func(active bool) { w.Eval(dragOutStagingScript("ui-smoke", active)) })
			state.finished = true
			smokeSignal(state.returned)
			return 0
		}
		result, _, _ := callProc.Call(previous, hwnd, message, wp, lp)
		return result
	})
	previous, _, _ = setPtr.Call(fixture.source, gwlpWndProc, callback)
	if previous == 0 {
		t.Fatal("cannot subclass controlled source")
	}
	defer setPtr.Call(fixture.source, gwlpWndProc, previous)
	driverDone := make(chan error, 1)
	go func() {
		defer func() { state.permit(); w.Dispatch(func() { w.Terminate() }) }()
		if err := smokeWait(ready, "WebView page ready"); err != nil {
			driverDone <- err
			return
		}
		if _, _, err := smokeSetCursor.Call(uintptr(fixture.sourcePoint.x), uintptr(fixture.sourcePoint.y)); err != syscall.Errno(0) {
			driverDone <- err
			return
		}
		if err := smokeMouse(0x0002); err != nil {
			driverDone <- err
			return
		}
		defer func() {
			smokeSetCursor.Call(uintptr(fixture.sourcePoint.x), uintptr(fixture.sourcePoint.y))
			_ = smokeMouse(0x0004)
		}()
		if err := smokeWait(state.entered, "OLE source enter"); err != nil {
			driverDone <- err
			return
		}
		if err := fixture.move(fixture.targetPoint, fixture.target); err != nil {
			driverDone <- err
			return
		}
		if err := smokeWait(state.requested, "blocked stage"); err != nil {
			driverDone <- err
			return
		}
		select {
		case <-received:
			select {
			case <-state.stageDone:
				driverDone <- fmt.Errorf("JavaScript delivery happened after staging completed")
				return
			default:
			}
		case <-time.After(3 * time.Second):
			state.permit()
			_ = smokeEscape(false)
			time.Sleep(120 * time.Millisecond)
			_ = smokeEscape(true)
			driverDone <- fmt.Errorf("no JavaScript staging event while OLE GetData/CoWait waited; stage released only for cleanup")
			return
		}
		state.permit()
		if err := smokeWait(state.supplied, "target received staged files"); err != nil {
			driverDone <- err
			return
		}
		_ = smokeMouse(0x0004)
		driverDone <- smokeWait(state.returned, "OLE return")
	}()
	w.Run()
	if err := <-driverDone; err != nil {
		t.Fatal(err)
	}
	t.Log("actual WebView JavaScript staging event reached local HTTP observer before staging was released during real OLE GetData wait")
	runtime.KeepAlive(callback)
}
