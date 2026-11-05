// Package dragtransfer carries drag staging controls and progress through its
// context without coupling the file copier to the native drag host.
package dragtransfer

import "context"

// Hooks lets a drag owner pause staging at checkpoints and observe byte counts.
// Wait must return when ctx ends. Progress callbacks receive positive deltas;
// totals may increase as more files are discovered during traversal.
type Hooks struct {
	Wait      func(context.Context) error
	AddTotal  func(int64)
	AddLoaded func(int64)
}

type hooksKey struct{}

func WithHooks(ctx context.Context, hooks Hooks) context.Context {
	return context.WithValue(ctx, hooksKey{}, hooks)
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
}
