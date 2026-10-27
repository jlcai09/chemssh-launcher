//go:build !windows || !webview2

package webview

import "context"

func DefaultEnabled() bool {
	return false
}

func Open(ctx context.Context, opts Options) error {
	return ErrUnsupported
}
