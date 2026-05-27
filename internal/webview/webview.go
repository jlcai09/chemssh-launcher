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
	CopyOnCtrlShiftC bool
	DisableDevTools  bool
	Fullscreen       bool
	UserAgent        string
}

type Opener interface {
	Open(ctx context.Context, opts Options) error
	Available() bool
}
