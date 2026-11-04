//go:build windows

package webview

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

// The tests below drive the COM object exactly as Windows does: through the
// vtable pointer stored in its first field. That layout is what no compiler
// checks, so it is worth verifying without a mouse.
//
// Out-parameters live in package variables on purpose. A callback invoked
// through SyscallN may grow this goroutine's stack on the first call, which
// silently invalidates a pointer into a local; COM callers pass pointers to
// memory Windows owns, so only the test needs the care.

var (
	scratchIID       guidW
	scratchFormatEtc formatEtcW
	scratchMedium    stgMediumW
	scratchMedium2   stgMediumW
	scratchPointer   uintptr
	scratchFetched   uint32
	scratchFormats   [8]formatEtcW
)

func newTestDragObject(t *testing.T, marker string, files []string) *dragDataObject {
	t.Helper()
	units, err := syscall.UTF16FromString(marker)
	if err != nil {
		t.Fatalf("UTF16FromString: %v", err)
	}
	object := &dragDataObject{
		vtbl:   dragDataObjectVtbl,
		refs:   1,
		marker: units,
		paths:  files,
	}
	pinDragObject(object)
	t.Cleanup(func() { object.unpin() })
	return object
}

func callMethod(t *testing.T, method uintptr, args ...uintptr) uint32 {
	t.Helper()
	result, _, _ := syscall.SyscallN(method, args...)
	return uint32(result)
}

func dragObjectRooted(object any) bool {
	dragPinMu.Lock()
	defer dragPinMu.Unlock()
	_, rooted := dragPinObjects[object]
	return rooted
}

func TestDragCOMRootsFollowRetainedReferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		object  any
		addRef  uintptr
		release uintptr
	}{
		{"data object", &dragDataObject{vtbl: dragDataObjectVtbl, refs: 1}, dragDataObjectVtbl[1], dragDataObjectVtbl[2]},
		{"drop source", &dragDropSource{vtbl: dragDropSourceVtbl, refs: 1}, dragDropSourceVtbl[1], dragDropSourceVtbl[2]},
		{"format enum", &dragFormatEnum{vtbl: dragFormatEnumVtbl, refs: 1}, dragFormatEnumVtbl[1], dragFormatEnumVtbl[2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pointer uintptr
			switch object := tc.object.(type) {
			case *dragDataObject:
				pointer = uintptr(unsafe.Pointer(object))
			case *dragDropSource:
				pointer = uintptr(unsafe.Pointer(object))
			case *dragFormatEnum:
				pointer = uintptr(unsafe.Pointer(object))
			}
			pinDragObject(tc.object)
			if !dragObjectRooted(tc.object) {
				t.Fatal("initial COM reference is not rooted")
			}
			if refs := callMethod(t, tc.addRef, pointer); refs != 2 {
				t.Fatalf("target AddRef = %d, want 2", refs)
			}
			// DoDragDrop's caller releases its own reference on return. The
			// target's reference must survive that and another completed drag.
			if refs := callMethod(t, tc.release, pointer); refs != 1 {
				t.Fatalf("loop-owner Release = %d, want 1", refs)
			}
			other := &dragDataObject{vtbl: dragDataObjectVtbl, refs: 1}
			pinDragObject(other)
			other.unpin()
			runtime.GC()
			if !dragObjectRooted(tc.object) || dragObjectRooted(other) {
				t.Fatal("another drag's final release changed the retained root")
			}
			if refs := callMethod(t, tc.release, pointer); refs != 0 {
				t.Fatalf("target Release = %d, want 0", refs)
			}
			if dragObjectRooted(tc.object) {
				t.Fatal("final COM Release left the object rooted")
			}
		})
	}
}

func TestDragFormatEnumeratorsAreIndependentlyRooted(t *testing.T) {
	scratchPointer = 0
	if hr := callMethod(t, dragDataObjectVtbl[8], 0, datadirGet, uintptr(unsafe.Pointer(&scratchPointer))); hr != sOK {
		t.Fatalf("EnumFormatEtc = 0x%08X", hr)
	}
	enumPointer := scratchPointer
	enum := (*dragFormatEnum)(comRef(enumPointer))
	if enum.refs != 1 || !dragObjectRooted(enum) {
		t.Fatal("new enumerator's caller reference is not rooted")
	}
	scratchPointer = 0
	if hr := callMethod(t, enum.vtbl[6], enumPointer, uintptr(unsafe.Pointer(&scratchPointer))); hr != sOK {
		t.Fatalf("Clone = 0x%08X", hr)
	}
	clonePointer := scratchPointer
	clone := (*dragFormatEnum)(comRef(clonePointer))
	if clone.refs != 1 || !dragObjectRooted(clone) {
		t.Fatal("clone's caller reference is not rooted")
	}
	if refs := callMethod(t, enum.vtbl[2], enumPointer); refs != 0 || dragObjectRooted(enum) {
		t.Fatalf("original enumerator final Release = %d, rooted=%t", refs, dragObjectRooted(enum))
	}
	runtime.GC()
	if !dragObjectRooted(clone) {
		t.Fatal("releasing the original removed the clone's root")
	}
	if hr := callMethod(t, clone.vtbl[5], clonePointer); hr != sOK {
		t.Fatalf("retained clone Reset = 0x%08X", hr)
	}
	if refs := callMethod(t, clone.vtbl[2], clonePointer); refs != 0 || dragObjectRooted(clone) {
		t.Fatalf("clone final Release = %d, rooted=%t", refs, dragObjectRooted(clone))
	}
}

func TestDragSourceQueryInterface(t *testing.T) {
	source := &dragDropSource{vtbl: dragDropSourceVtbl, refs: 1}
	pinDragObject(source)
	t.Cleanup(func() { dragSourceRelease(uintptr(unsafe.Pointer(source))) })
	// Use the documented GUID independently of the implementation constant.
	for _, iid := range []guidW{
		guidUnknown,
		{0x21, 0x01, 0, 0, 0, 0, 0, 0, 0xC0, 0, 0, 0, 0, 0, 0, 0x46},
	} {
		scratchIID, scratchPointer = iid, 0
		hr := callMethod(t, source.vtbl[0], uintptr(unsafe.Pointer(source)),
			uintptr(unsafe.Pointer(&scratchIID)), uintptr(unsafe.Pointer(&scratchPointer)))
		if hr != sOK || scratchPointer != uintptr(unsafe.Pointer(source)) || source.refs != 2 {
			t.Fatalf("QueryInterface = 0x%08X, pointer=%X refs=%d", hr, scratchPointer, source.refs)
		}
		if refs := callMethod(t, source.vtbl[2], scratchPointer); refs != 1 {
			t.Fatalf("Release = %d, want 1", refs)
		}
	}
	scratchIID, scratchPointer = guidDataObject, 1
	if hr := callMethod(t, source.vtbl[0], uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(&scratchIID)), uintptr(unsafe.Pointer(&scratchPointer))); hr != eNoInterface || scratchPointer != 0 || source.refs != 1 {
		t.Fatalf("unsupported QueryInterface = 0x%08X, pointer=%X refs=%d", hr, scratchPointer, source.refs)
	}
}

func TestDragDataObjectRejectsUnsupportedFormatRequests(t *testing.T) {
	object := newTestDragObject(t, `{}`, []string{`C:\Users\user\Desktop\a.txt`})
	for _, cf := range []uint16{cfHDrop, cfUnicodeText, preferredDropEffectFormat()} {
		for _, tc := range []struct {
			name   string
			aspect uint32
			index  int32
			tymed  uint32
			want   uint32
		}{
			{"empty medium", dvAspectContent, -1, 0, dvETymed},
			{"stream medium", dvAspectContent, -1, 4, dvETymed},
			{"thumbnail aspect", 2, -1, tymedHG, dvEDvAspect},
			{"combined aspects", 3, -1, tymedHG, dvEDvAspect},
			{"indexed content", dvAspectContent, 0, tymedHG, dvELindex},
		} {
			t.Run(tc.name, func(t *testing.T) {
				scratchFormatEtc = formatEtcW{cfFormat: cf, dwAspect: tc.aspect, lindex: tc.index, tymed: tc.tymed}
				if hr := callMethod(t, object.vtbl[5], uintptr(unsafe.Pointer(object)), uintptr(unsafe.Pointer(&scratchFormatEtc))); hr != tc.want {
					t.Fatalf("QueryGetData(CF %d) = 0x%08X, want 0x%08X", cf, hr, tc.want)
				}
				scratchMedium = stgMediumW{tymed: 99, hGlobal: 1, pUnkForRelease: 1}
				if hr := callMethod(t, object.vtbl[3], uintptr(unsafe.Pointer(object)), uintptr(unsafe.Pointer(&scratchFormatEtc)), uintptr(unsafe.Pointer(&scratchMedium))); hr != tc.want {
					t.Fatalf("GetData(CF %d) = 0x%08X, want 0x%08X", cf, hr, tc.want)
				}
				if scratchMedium != (stgMediumW{}) || object.fileRequests != 0 {
					t.Fatalf("invalid request allocated a medium or started staging: %+v", scratchMedium)
				}
			})
		}
		// TYMED is a bitmask; HGLOBAL plus another supported-by-target medium is valid.
		scratchFormatEtc = formatEtcW{cfFormat: cf, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG | 4}
		if hr := callMethod(t, object.vtbl[5], uintptr(unsafe.Pointer(object)), uintptr(unsafe.Pointer(&scratchFormatEtc))); hr != sOK {
			t.Fatalf("mixed TYMED mask = 0x%08X", hr)
		}
	}
	scratchFormatEtc = formatEtcW{dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	if hr := callMethod(t, object.vtbl[5], uintptr(unsafe.Pointer(object)), uintptr(unsafe.Pointer(&scratchFormatEtc))); hr != dvEFormatEtc {
		t.Fatalf("unregistered format = 0x%08X", hr)
	}
}

func TestDragSourceHonorsStagingCancellation(t *testing.T) {
	source := &dragDropSource{vtbl: dragDropSourceVtbl, refs: 1, object: &dragDataObject{stageCancelled: true}}
	pinDragObject(source)
	t.Cleanup(func() { dragSourceRelease(uintptr(unsafe.Pointer(source))) })
	if hr := callMethod(t, source.vtbl[3], uintptr(unsafe.Pointer(source)), 0, mkLButton); hr != dragdropSCancel || !source.cancelled {
		t.Fatalf("cancelled stage QueryContinueDrag = 0x%08X, cancelled=%t", hr, source.cancelled)
	}
}

func TestDragDataObjectQueriesAndServesFileList(t *testing.T) {
	paths := []string{`C:\Users\user\Desktop\a.txt`, `C:\Users\user\Desktop\b.txt`}
	object := newTestDragObject(t, `{"chemssh":"chemssh-launcher-file-transfer"}`, paths)

	iid := guidUnknown
	scratchPointer = 0
	if hr := callMethod(t, object.vtbl[0],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&iid)),
		uintptr(unsafe.Pointer(&scratchPointer)),
	); hr != sOK {
		t.Fatalf("QueryInterface(IID_IUnknown) = 0x%08X, want S_OK", hr)
	}
	if scratchPointer != uintptr(unsafe.Pointer(object)) {
		t.Fatal("QueryInterface did not return the same object")
	}
	if refs := callMethod(t, object.vtbl[2], scratchPointer); refs != 1 {
		t.Fatalf("QueryInterface Release = %d, want 1", refs)
	}

	scratchFormatEtc = formatEtcW{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	if hr := callMethod(t, object.vtbl[5],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&scratchFormatEtc)),
	); hr != sOK {
		t.Fatalf("QueryGetData(CF_HDROP) = 0x%08X, want S_OK", hr)
	}

	scratchMedium = stgMediumW{}
	if hr := callMethod(t, object.vtbl[3],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&scratchFormatEtc)),
		uintptr(unsafe.Pointer(&scratchMedium)),
	); hr != sOK {
		t.Fatalf("GetData(CF_HDROP) = 0x%08X, want S_OK", hr)
	}
	if scratchMedium.tymed != tymedHG {
		t.Fatalf("medium.tymed = %d, want TYMED_HGLOBAL", scratchMedium.tymed)
	}
	if scratchMedium.hGlobal == 0 {
		t.Fatal("GetData(CF_HDROP) returned no handle")
	}
	defer globalFree(scratchMedium.hGlobal)

	if got := readDropFiles(t, scratchMedium.hGlobal); len(got) != len(paths) || got[0] != paths[0] || got[1] != paths[1] {
		t.Fatalf("dropped files = %v, want %v", got, paths)
	}
}

func TestDragDataObjectKeepsInternalDragFreeOfStaging(t *testing.T) {
	object := &dragDataObject{
		vtbl:   dragDataObjectVtbl,
		refs:   1,
		marker: []uint16{'{', '}', 0},
		ready:  make(chan struct{}),
		stage: func() ([]string, error) {
			return []string{`C:\Users\user\Desktop\a.txt`}, nil
		},
	}
	pinDragObject(object)
	t.Cleanup(func() { object.unpin() })

	// Chromium asks for the files on drag enter. Over the launcher's own page
	// that must fail immediately: waiting would freeze the UI while the whole
	// selection is copied for a drop that only uses the text marker.
	scratchFormatEtc = formatEtcW{cfFormat: cfHDrop, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	scratchMedium = stgMediumW{}
	if hr := callMethod(t, object.vtbl[3],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&scratchFormatEtc)),
		uintptr(unsafe.Pointer(&scratchMedium)),
	); hr != eFail {
		t.Fatalf("GetData(CF_HDROP) = 0x%08X, want E_FAIL over the launcher", hr)
	}
	if object.stagingStarted() {
		t.Fatal("an internal drag started staging the selection")
	}
}

func readDropFiles(t *testing.T, handle uintptr) []string {
	t.Helper()
	base := globalLock(handle)
	if base == nil {
		t.Fatal("could not lock the medium")
	}
	defer globalUnlock(handle)
	header := (*dropFilesW)(base)
	var names []string
	cursor := unsafe.Add(base, uintptr(header.pFiles))
	for *(*uint16)(cursor) != 0 {
		length := 0
		for *(*uint16)(unsafe.Add(cursor, uintptr(length)*2)) != 0 {
			length++
		}
		names = append(names, syscall.UTF16ToString(unsafe.Slice((*uint16)(cursor), length)))
		cursor = unsafe.Add(cursor, uintptr(length+1)*2)
	}
	return names
}

func TestDragDataObjectServesMarkerText(t *testing.T) {
	const marker = `{"chemssh":"chemssh-launcher-file-transfer"}`
	object := newTestDragObject(t, marker, []string{`C:\Users\user\Desktop\a.txt`})

	scratchFormatEtc = formatEtcW{cfFormat: cfUnicodeText, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	scratchMedium2 = stgMediumW{}
	if hr := callMethod(t, object.vtbl[3],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&scratchFormatEtc)),
		uintptr(unsafe.Pointer(&scratchMedium2)),
	); hr != sOK {
		t.Fatalf("GetData(CF_UNICODETEXT) = 0x%08X, want S_OK", hr)
	}
	defer globalFree(scratchMedium2.hGlobal)

	base := globalLock(scratchMedium2.hGlobal)
	if base == nil {
		t.Fatal("could not lock the medium")
	}
	defer globalUnlock(scratchMedium2.hGlobal)
	if got := syscall.UTF16ToString(unsafe.Slice((*uint16)(base), len(marker))); got != marker {
		t.Fatalf("marker = %q, want %q", got, marker)
	}
}

func TestDragDataObjectServesPreferredDropEffect(t *testing.T) {
	object := newTestDragObject(t, `{}`, []string{`C:\Users\user\Desktop\a.txt`})

	scratchFormatEtc = formatEtcW{cfFormat: preferredDropEffectFormat(), dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	scratchMedium = stgMediumW{}
	if hr := callMethod(t, object.vtbl[3],
		uintptr(unsafe.Pointer(object)),
		uintptr(unsafe.Pointer(&scratchFormatEtc)),
		uintptr(unsafe.Pointer(&scratchMedium)),
	); hr != sOK {
		t.Fatalf("GetData(Preferred DropEffect) = 0x%08X, want S_OK", hr)
	}
	defer globalFree(scratchMedium.hGlobal)

	base := globalLock(scratchMedium.hGlobal)
	if base == nil {
		t.Fatal("could not lock the medium")
	}
	defer globalUnlock(scratchMedium.hGlobal)
	if got := *(*uint32)(base); got != dropeffectCopy {
		t.Fatalf("preferred effect = %d, want %d", got, dropeffectCopy)
	}
}

func TestDragDataObjectEnumeratesFormats(t *testing.T) {
	object := newTestDragObject(t, `{}`, []string{`C:\Users\user\Desktop\a.txt`})

	scratchPointer = 0
	if hr := callMethod(t, object.vtbl[8],
		uintptr(unsafe.Pointer(object)),
		uintptr(datadirGet),
		uintptr(unsafe.Pointer(&scratchPointer)),
	); hr != sOK {
		t.Fatalf("EnumFormatETC = 0x%08X, want S_OK", hr)
	}
	if scratchPointer == 0 {
		t.Fatal("EnumFormatETC returned no enumerator")
	}
	enumPointer := scratchPointer
	t.Cleanup(func() { dragEnumRelease(enumPointer) })

	scratchFetched = 0
	scratchFormats = [8]formatEtcW{}
	hr := callMethod(t, (*dragFormatEnum)(comRef(scratchPointer)).vtbl[3],
		scratchPointer,
		uintptr(len(scratchFormats)),
		uintptr(unsafe.Pointer(&scratchFormats[0])),
		uintptr(unsafe.Pointer(&scratchFetched)),
	)
	// S_FALSE is the documented answer when fewer entries were copied than asked.
	if hr != sOK && hr != sFalse {
		t.Fatalf("IEnumFORMATETC::Next = 0x%08X", hr)
	}
	if scratchFetched == 0 {
		t.Fatal("IEnumFORMATETC::Next copied nothing")
	}

	wanted := map[uint16]bool{cfHDrop: false, cfUnicodeText: false, preferredDropEffectFormat(): false}
	for index := 0; index < int(scratchFetched); index++ {
		format := scratchFormats[index]
		if _, ok := wanted[format.cfFormat]; ok {
			wanted[format.cfFormat] = true
		}
		if format.tymed&tymedHG == 0 || format.dwAspect != dvAspectContent || format.lindex != -1 {
			t.Fatalf("format %d is malformed: %+v", index, format)
		}
	}
	for format, seen := range wanted {
		if !seen {
			t.Fatalf("CF %d missing from the enumerator (%d entries)", format, scratchFetched)
		}
	}
}

func TestDragSourceCancelsOnEscape(t *testing.T) {
	source := &dragDropSource{vtbl: dragDropSourceVtbl, refs: 1}
	pinDragObject(source)
	t.Cleanup(func() { dragSourceRelease(uintptr(unsafe.Pointer(source))) })

	hr := callMethod(t, source.vtbl[3],
		uintptr(unsafe.Pointer(source)),
		1,
		uintptr(mkLButton),
	)
	if hr != dragdropSCancel || !source.cancelled {
		t.Fatalf("QueryContinueDrag(Escape) = 0x%08X, cancelled=%t", hr, source.cancelled)
	}
	if source.continues != 1 || source.firstKeyState != mkLButton {
		t.Fatalf("diagnostics = (%d, 0x%X), want (1, 0x%X)", source.continues, source.firstKeyState, mkLButton)
	}
}

func TestCursorLeftPressPoint(t *testing.T) {
	cases := []struct {
		name string
		x, y int32
		want bool
	}{
		{name: "same point", x: 100, y: 100, want: false},
		{name: "under the threshold", x: 102, y: 102, want: false},
		{name: "at the threshold", x: 104, y: 100, want: true},
		{name: "diagonal 3-4-5", x: 103, y: 104, want: true},
		{name: "backwards", x: 96, y: 95, want: true},
	}
	for _, testCase := range cases {
		if got := cursorLeftPressPoint(100, 100, testCase.x, testCase.y); got != testCase.want {
			t.Fatalf("%s: cursorLeftPressPoint = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}

// The drag loop now starts on an armed press, so a release without any movement
// must cancel: a drop there would move the selection onto the row under the
// cursor.
func TestDragSourceCancelsAnUnmovedPress(t *testing.T) {
	if hr := releaseAction(false); hr != dragdropSCancel {
		t.Fatalf("releaseAction(false) = 0x%08X, want DRAGDROP_S_CANCEL", hr)
	}
	if hr := releaseAction(true); hr != dragdropSDrop {
		t.Fatalf("releaseAction(true) = 0x%08X, want DRAGDROP_S_DROP", hr)
	}
}

func TestDragOutcomeKeepsCancelledDragsSilent(t *testing.T) {
	cases := []struct {
		name    string
		outcome dragOutcome
		want    bool
	}{
		{name: "released in place", outcome: dragOutcome{}, want: false},
		{name: "escape cancels", outcome: dragOutcome{Moved: true, Cancelled: true}, want: false},
		{name: "refused drop", outcome: dragOutcome{Moved: true}, want: true},
		{name: "completed copy", outcome: dragOutcome{Moved: true, Effect: dropeffectCopy}, want: false},
		{name: "escape after a failure", outcome: dragOutcome{Moved: true, Cancelled: true, Err: syscall.EINVAL}, want: true},
	}
	for _, testCase := range cases {
		if got := testCase.outcome.Failed(); got != testCase.want {
			t.Fatalf("%s: Failed = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}
