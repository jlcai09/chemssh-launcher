//go:build windows

package webview

import (
	"context"
	"errors"
	"testing"
	"unsafe"
)

func TestDragFeedbackRequiresCompletedAcceptedExtraction(t *testing.T) {
	performed, logical := dragFeedbackFormats()
	for _, tc := range []struct {
		name            string
		accepted, ready bool
		stageErr        error
		format          uint16
		want            string
	}{
		{name: "before extraction", accepted: true, format: performed},
		{name: "not accepted", ready: true, format: performed},
		{name: "logical effect is not completion", accepted: true, ready: true, format: logical},
		{name: "failed extraction", accepted: true, ready: true, stageErr: errors.New("download failed"), format: performed},
		{name: "completed copy", accepted: true, ready: true, format: performed, want: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []dragAsyncEvent
			o := &dragDataObject{ready: make(chan struct{}), stageErr: tc.stageErr, paths: []string{"file.txt"}}
			o.async = &dragAsyncState{ctx: context.Background(), started: tc.accepted, notify: func(e dragAsyncEvent) { events = append(events, e) }}
			if tc.ready {
				close(o.ready)
			}
			handle, err := newGlobalDropEffectHandle()
			if err != nil {
				t.Fatal(err)
			}
			defer globalFree(handle)
			format := formatEtcW{cfFormat: tc.format, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
			medium := stgMediumW{tymed: tymedHG, hGlobal: handle}
			if got := setDragFeedback(o, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium)), 0); got != sOK {
				t.Fatalf("SetData: %08X", got)
			}
			if tc.want == "" {
				if len(events) != 0 {
					t.Fatalf("premature completion: %+v", events)
				}
				return
			}
			if len(events) != 1 || events[0].State != tc.want || events[0].Effect != dropeffectCopy {
				t.Fatalf("completion: %+v", events)
			}
			o.finishAsync(dragAsyncEvent{State: "done", Effect: dropeffectCopy})
			if len(events) != 1 {
				t.Fatalf("duplicate completion: %+v", events)
			}
		})
	}
}

func TestDragFeedbackMediumOwnershipAndValidation(t *testing.T) {
	performed, _ := dragFeedbackFormats()
	format := formatEtcW{cfFormat: performed, dwAspect: dvAspectContent, lindex: -1, tymed: tymedHG}
	o := &dragDataObject{}
	short, err := globalAlloc(1)
	if err != nil {
		t.Fatal(err)
	}
	medium := stgMediumW{tymed: tymedHG, hGlobal: short}
	if got := setDragFeedback(o, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium)), 1); got != eInvalidArg {
		t.Fatalf("short medium: %08X", got)
	}
	if size, _, _ := procDragGlobalSize.Call(short); size == 0 {
		t.Fatal("failed SetData consumed caller medium")
	}
	globalFree(short)
	handle, err := newGlobalDropEffectHandle()
	if err != nil {
		t.Fatal(err)
	}
	medium = stgMediumW{tymed: tymedHG, hGlobal: handle}
	if got := setDragFeedback(o, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium)), 1); got != sOK {
		t.Fatalf("owned medium: %08X", got)
	}
	if size, _, _ := procDragGlobalSize.Call(handle); size != 0 {
		globalFree(handle)
		t.Fatal("successful SetData did not release transferred medium")
	}
}
