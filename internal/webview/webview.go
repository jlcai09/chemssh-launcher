package webview

import (
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
}
