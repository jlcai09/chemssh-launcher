//go:build windows

package webview

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	dragFeedbackOnce      sync.Once
	dragPerformedFormat   uint16
	dragLogicalFormat     uint16
	procDragGlobalSize    = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalSize")
	procDragReleaseMedium = syscall.NewLazyDLL("ole32.dll").NewProc("ReleaseStgMedium")
)

func dragFeedbackFormats() (performed, logical uint16) {
	dragFeedbackOnce.Do(func() {
		register := func(name string) uint16 {
			text, _ := syscall.UTF16PtrFromString(name)
			id, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(text)))
			return uint16(id)
		}
		dragPerformedFormat = register("Performed DropEffect")
		dragLogicalFormat = register("Logical Performed DropEffect")
	})
	return dragPerformedFormat, dragLogicalFormat
}

// Windows' filesystem-folder handler can acknowledge the completed copy through
// CFSTR_PERFORMEDDROPEFFECT instead of EndOperation. The logical effect may be
// sent before extraction and is never sufficient to finish the background job.
// https://learn.microsoft.com/en-us/windows/win32/shell/clipboard#cfstr_performeddropeffect
func setDragFeedback(o *dragDataObject, pformat, pmedium, release uintptr) uintptr {
	if pformat == 0 || pmedium == 0 {
		return eInvalidArg
	}
	format := (*formatEtcW)(comRef(pformat))
	performed, logical := dragFeedbackFormats()
	if format.cfFormat == 0 || (format.cfFormat != performed && format.cfFormat != logical) {
		return eNotImpl
	}
	medium := (*stgMediumW)(comRef(pmedium))
	if format.dwAspect != dvAspectContent {
		return dvEDvAspect
	}
	if format.lindex != -1 {
		return dvELindex
	}
	if format.tymed&tymedHG == 0 || medium.tymed != tymedHG || medium.hGlobal == 0 {
		return dvETymed
	}
	size, _, _ := procDragGlobalSize.Call(medium.hGlobal)
	if size < 4 {
		return eInvalidArg
	}
	data := globalLock(medium.hGlobal)
	if data == nil {
		return eFail
	}
	effect := *(*uint32)(data)
	globalUnlock(medium.hGlobal)
	if release != 0 {
		defer procDragReleaseMedium.Call(pmedium)
	}
	if format.cfFormat == performed && o.async != nil {
		// Ignore any early feedback. Only an accepted operation whose complete
		// local file list has been rendered can be acknowledged as copied.
		select {
		case <-o.ready:
			_, err := o.stagedPaths()
			o.async.mu.Lock()
			accepted := o.async.started && !o.async.terminal
			o.async.mu.Unlock()
			if accepted && err == nil {
				event := dragAsyncEvent{State: "done", Effect: effect}
				if effect&dropeffectCopy == 0 {
					event.State = "cancelled"
				}
				o.finishAsync(event)
			}
		default:
		}
	}
	return sOK
}
