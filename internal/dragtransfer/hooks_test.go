package dragtransfer

import (
	"context"
	"errors"
	"testing"
)

func TestHooksDefaultAndCancellation(t *testing.T) {
	if err := Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	AddTotal(context.Background(), 1)
	AddLoaded(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = WithHooks(ctx, Hooks{Wait: func(context.Context) error {
		t.Fatal("cancelled checkpoint called the hook")
		return nil
	}})
	if err := Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

func TestHooksCallbacksAndErrors(t *testing.T) {
	var total, loaded int64
	sentinel := errors.New("pause checkpoint failed")
	ctx := WithHooks(context.Background(), Hooks{
		Wait:      func(context.Context) error { return sentinel },
		AddTotal:  func(n int64) { total += n },
		AddLoaded: func(n int64) { loaded += n },
	})
	if err := Wait(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("Wait = %v, want checkpoint error", err)
	}
	for _, n := range []int64{-3, 0, 3, 5} {
		AddTotal(ctx, n)
		AddLoaded(ctx, n)
	}
	if total != 8 || loaded != 8 {
		t.Fatalf("callbacks = (%d, %d), want (8, 8)", total, loaded)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithHooks(ctx, Hooks{Wait: func(context.Context) error {
		cancel()
		return sentinel
	}})
	if err := Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want cancellation to override checkpoint error", err)
	}
}

func TestHooksKeepParentValuesAndReplaceCallbacks(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "parent")
	ctx := WithHooks(parent, Hooks{AddTotal: func(int64) { t.Fatal("replaced callback ran") }})
	var total int64
	ctx = WithHooks(ctx, Hooks{AddTotal: func(n int64) { total += n }})
	AddTotal(ctx, 4)
	if total != 4 || ctx.Value(key{}) != "parent" {
		t.Fatal("hook context did not preserve parent values or replace callbacks")
	}
}
