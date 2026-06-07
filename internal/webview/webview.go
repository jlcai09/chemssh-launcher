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
	UserAgent        string
	// CloseInterceptor is called when the user attempts to close the window.
	// If it returns true, a confirmation dialog is shown. If the user cancels,
	// the window stays open. If it returns false, the window closes normally.
	CloseInterceptor func() bool
}

type Opener interface {
	Open(ctx context.Context, opts Options) error
	Available() bool
}
