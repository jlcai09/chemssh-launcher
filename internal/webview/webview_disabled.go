//go:build !windows || !webview2

package webview

import "context"

func Available() bool {
	return false
}

func DefaultEnabled() bool {
	return false
}

func Open(ctx context.Context, opts Options) error {
	return ErrUnsupported
}
