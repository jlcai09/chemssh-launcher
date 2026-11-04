//go:build windows

package webview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// This uses Windows' real filesystem-folder drop handler, not an emulated
// IDropTarget. It touches only fresh temporary directories and is opt-in.
func TestAsyncDragWindowsFolderTarget(t *testing.T) {
	if os.Getenv("CHEMSSH_NATIVE_DRAG_SHELL_SMOKE") != "1" {
		t.Skip("set CHEMSSH_NATIVE_DRAG_SHELL_SMOKE=1 for the Windows Shell folder target")
	}
	asyncTestSTA(t)
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "file.txt"), []byte("file contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "folder", "child.txt"), []byte("folder contents"), 0600); err != nil {
		t.Fatal(err)
	}
	// GUID values use native COM memory order.
	itemIID := guidW{0x1e, 0x6d, 0x82, 0x43, 0x18, 0xe7, 0xee, 0x42, 0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}
	handlerID := guidW{0x25, 0xe2, 0x81, 0x39, 0x59, 0xf5, 0xd3, 0x11, 0x8e, 0x3a, 0x00, 0xc0, 0x4f, 0x68, 0x37, 0xd5}
	targetIID := guidW{0x22, 0x01, 0, 0, 0, 0, 0, 0, 0xc0, 0, 0, 0, 0, 0, 0, 0x46}
	path, _ := syscall.UTF16PtrFromString(destination)
	var item, target uintptr
	create := syscall.NewLazyDLL("shell32.dll").NewProc("SHCreateItemFromParsingName")
	hr, _, _ := create.Call(uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&itemIID)), uintptr(unsafe.Pointer(&item)))
	if int32(hr) < 0 {
		t.Fatalf("SHCreateItemFromParsingName: %08X", hr)
	}
	defer releaseDragCOM(item)
	h := asyncTestCall(item, 3, 0, uintptr(unsafe.Pointer(&handlerID)), uintptr(unsafe.Pointer(&targetIID)), uintptr(unsafe.Pointer(&target)))
	if int32(h) < 0 || target == 0 {
		t.Fatalf("BindToHandler(IDropTarget): %08X", h)
	}
	defer releaseDragCOM(target)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	entered := make(chan struct{})
	events := make(chan dragAsyncEvent, 8)
	var stages atomic.Int32
	object, proxy, err := newAsyncDragObject("marker", func() ([]string, error) {
		stages.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []string{filepath.Join(cache, "file.txt"), filepath.Join(cache, "folder")}, nil
	}, func(e dragAsyncEvent) { events <- e }, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragCOM(proxy)
	effect := uint32(dropeffectCopy)
	// POINTL is a struct by value: packed on win64 and two arguments on win32.
	point := windowPointArgs(pointW{})
	args := []uintptr{proxy, mkLButton}
	args = append(args, point...)
	args = append(args, uintptr(unsafe.Pointer(&effect)))
	h = asyncTestCall(target, 3, args...)
	if int32(h) < 0 {
		t.Fatalf("DragEnter: %08X", h)
	}
	t.Logf("Windows folder DragEnter: hr=%08X effect=%d staged=%d", h, effect, stages.Load())
	effect = dropeffectCopy
	args = []uintptr{proxy, 0}
	args = append(args, point...)
	args = append(args, uintptr(unsafe.Pointer(&effect)))
	started := time.Now()
	h = asyncTestCall(target, 6, args...)
	cost := time.Since(started)
	object.async.loopOnce.Do(func() { close(object.async.loopDone) })
	object.async.mu.Lock()
	accepted := object.async.started
	object.async.mu.Unlock()
	t.Logf("Windows folder Drop returned: hr=%08X effect=%d cost=%s asyncAccepted=%v", h, effect, cost, accepted)
	if int32(h) < 0 || !accepted {
		t.Fatalf("Windows folder did not accept asynchronous extraction: %08X effect=%d", h, effect)
	}
	if cost > time.Second {
		t.Fatalf("Drop did not promptly return: %s", cost)
	}
	// Service the Shell target's STA just as the normal UI loop would.
	wake, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if wake == 0 {
		t.Fatal("CreateEvent failed")
	}
	defer procCloseHandle.Call(wake)
	peek := smokeUser32.NewProc("PeekMessageW")
	pump := func() {
		var message smokeMessage
		for {
			got, _, _ := peek.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1)
			if got == 0 {
				break
			}
			smokeTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
			smokeDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
		}
		var index uint32
		procCoWaitForMultipleHandles.Call(0x18, 10, 1, uintptr(unsafe.Pointer(&wake)), uintptr(unsafe.Pointer(&index)))
	}
	waiting := time.NewTimer(3 * time.Second)
	defer waiting.Stop()
waitForExtraction:
	for {
		select {
		case <-entered:
			break waitForExtraction
		case <-waiting.C:
			t.Fatal("Shell did not request background extraction")
		default:
			pump()
		}
	}
	select {
	case e := <-events:
		if e.State != "started" {
			t.Fatalf("premature terminal: %+v", e)
		}
	default:
		t.Fatal("no StartOperation notification")
	}
	if _, err := os.Stat(filepath.Join(destination, "file.txt")); !os.IsNotExist(err) {
		t.Fatal("target populated before cache was released")
	}
	close(release)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event.State != "done" {
				t.Fatalf("Shell failed delivery: %+v", event)
			}
			for relative, want := range map[string]string{"file.txt": "file contents", filepath.Join("folder", "child.txt"): "folder contents"} {
				got, err := os.ReadFile(filepath.Join(destination, relative))
				if err != nil || string(got) != want {
					t.Fatal(fmt.Sprintf("destination %s: %q %v", relative, got, err))
				}
			}
			t.Logf("Windows folder delivered file and recursive folder; stages=%d completionHRESULT=%08X effect=%d", stages.Load(), event.HRESULT, event.Effect)
			return
		case <-deadline.C:
			paths, stageErr := object.stagedPaths()
			entries, listErr := os.ReadDir(destination)
			t.Fatalf("Shell did not finish copying: staged=%v stageErr=%v entries=%v listErr=%v", paths, stageErr, entries, listErr)
		default:
			pump()
		}
	}
}
