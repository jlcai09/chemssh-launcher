// Package dragtransfer carries drag staging controls and progress through its
// context without coupling the file copier to the native drag host.
package dragtransfer

import (
	"context"
	"sync"
)

// FileProgress describes one remote path as it is discovered and staged.
// Kind is "file" or "folder"; State is "caching", "done", or "error".
// Folder counters stay zero: only file writes contribute bytes.
type FileProgress struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Loaded int64  `json:"loaded"`
	Total  int64  `json:"total"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

// Hooks lets a drag owner pause staging at checkpoints and observe byte counts.
// Wait must return when ctx ends. Aggregate callbacks receive positive deltas;
// totals may increase as more files are discovered during traversal.
// File receives absolute per-path snapshots and is optional.
type Hooks struct {
	Wait      func(context.Context) error
	AddTotal  func(int64)
	AddLoaded func(int64)
	File      func(FileProgress)
}

type hooksKey struct{}
type fileKey struct{}
type fileScope struct {
	sync.Mutex
	progress FileProgress
}

func WithHooks(ctx context.Context, hooks Hooks) context.Context {
	return context.WithValue(ctx, hooksKey{}, hooks)
}

// ReportFile reports an absolute per-path snapshot without changing aggregate
// counters. It also permits final error reports after context cancellation.
func ReportFile(ctx context.Context, p FileProgress) {
	hooks, _ := ctx.Value(hooksKey{}).(Hooks)
	if hooks.File != nil {
		hooks.File(p)
	}
}

// WithFile starts an independent file write scope, retaining all parent hooks
// and values. AddLoaded on this context reports actual writes to this path as
// well as the existing aggregate callback. Sibling scopes never share counters.
// The caller reports completion or failure explicitly with ReportFile.
func WithFile(ctx context.Context, path string, total int64) context.Context {
	p := FileProgress{Path: path, Kind: "file", Total: total, State: "caching"}
	ctx = context.WithValue(ctx, fileKey{}, &fileScope{progress: p})
	ReportFile(ctx, p)
	return ctx
}

// Wait checks cancellation before and after the optional pause checkpoint.
func Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	hooks, _ := ctx.Value(hooksKey{}).(Hooks)
	if hooks.Wait == nil {
		return nil
	}
	err := hooks.Wait(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func AddTotal(ctx context.Context, n int64) {
	if n <= 0 {
		return
	}
	hooks, _ := ctx.Value(hooksKey{}).(Hooks)
	if hooks.AddTotal != nil {
		hooks.AddTotal(n)
	}
}

func AddLoaded(ctx context.Context, n int64) {
	if n <= 0 {
		return
	}
	hooks, _ := ctx.Value(hooksKey{}).(Hooks)
	if hooks.AddLoaded != nil {
		hooks.AddLoaded(n)
	}
	if scope, _ := ctx.Value(fileKey{}).(*fileScope); scope != nil {
		scope.Lock()
		scope.progress.Loaded += n
		p := scope.progress
		scope.Unlock()
		ReportFile(ctx, p)
	}
}
