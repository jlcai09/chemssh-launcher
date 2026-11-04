package webview

import (
	"context"
	"errors"
)

var ErrUnsupported = errors.New("webview2 support is not available in this build")

type Options struct {
	Title            string
	URL              string
	Width            int
	Height           int
	Debug            bool
	DataPath         string
	CopyOnCtrlShiftC bool
	DisableDevTools  bool
	Fullscreen       bool
	// CloseInterceptor is called when the user attempts to close the window.
	// If it returns true, a confirmation dialog is shown. If the user cancels,
	// the window stays open. If it returns false, the window closes normally.
	CloseInterceptor func() bool
	// DragOutFiles copies the remote files a native drag-out is exposing and
	// returns their local paths. The host calls it once per drag, in the
	// background, after a system target accepts asynchronous extraction. A drop
	// inside the launcher travels through the marker without staging copies.
	// Accepted jobs survive OLE exit. Cancellation or host shutdown cancels ctx;
	// implementations must interrupt IO and remove incomplete copies while
	// preserving copies that finished successfully.
	DragOutFiles func(ctx context.Context, session string, paths []string) ([]string, error)
	// RetainDragOutSession keeps the connection alive independently of file tabs.
	// The returned release is called when the job ends or the host closes.
	RetainDragOutSession func(session string) (release func(), err error)
	// Report receives non-fatal host diagnostics, such as a native drag that
	// never started. It runs on the UI thread and must not block.
	Report func(message string)
}
