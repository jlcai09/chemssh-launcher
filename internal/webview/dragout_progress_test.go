package webview

import (
	"context"
	"testing"
	"time"

	"chemssh-launcher/internal/dragtransfer"
)

func TestDragJobProgressPauseResumeAndDelivery(t *testing.T) {
	r := newDragJobRegistry(nil)
	defer r.close()
	ctx, _ := r.add("job", "session", nil)
	r.update("job", "caching", nil)
	dragtransfer.AddTotal(ctx, 100)
	dragtransfer.AddLoaded(ctx, 25)
	if !r.control("job", "pause") {
		t.Fatal("pause rejected")
	}
	got := r.snapshot()[0]
	if got.Total != 100 || got.Loaded != 25 || !got.Paused {
		t.Fatalf("progress: %+v", got)
	}
	if r.update("job", "delivering", nil) {
		t.Fatal("paused job delivered")
	}
	finished := make(chan bool, 1)
	go func() { finished <- r.finishCaching(ctx, "job") }()
	select {
	case <-finished:
		t.Fatal("paused job did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	if !r.control("job", "resume") {
		t.Fatal("resume rejected")
	}
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("delivery rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not wake delivery")
	}
	if r.control("job", "pause") || r.control("job", "cancel") {
		t.Fatal("system delivery allowed control")
	}
	dragtransfer.AddLoaded(ctx, 80)
	if got := r.snapshot()[0].Loaded; got != 25 {
		t.Fatal("late progress changed delivery", got)
	}
	r.update("job", "done", nil)
	got = r.snapshot()[0]
	if got.EndedAt == 0 || got.Paused {
		t.Fatalf("terminal state: %+v", got)
	}
}

func TestDragJobCancelAndCloseWakePausedCache(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			r := newDragJobRegistry(nil)
			defer r.close()
			ctx, _ := r.add("job", "session", nil)
			r.update("job", "accepted", nil)
			r.control("job", "pause")
			result := make(chan error, 1)
			go func() { result <- dragtransfer.Wait(ctx) }()
			if action == "close" {
				r.close()
			} else if !r.control("job", "cancel") {
				t.Fatal("cancel rejected")
			}
			select {
			case err := <-result:
				if err != context.Canceled {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("paused cache was not released")
			}
		})
	}
}

func TestDragJobProgressFlushesOnStateChange(t *testing.T) {
	var events []dragJob
	r := newDragJobRegistry(func(job dragJob) { events = append(events, job) })
	defer r.close()
	ctx, _ := r.add("job", "session", nil)
	r.update("job", "caching", nil)
	for i := 0; i < 100; i++ {
		dragtransfer.AddLoaded(ctx, 1)
	}
	if len(events) >= 100 {
		t.Fatal("byte updates were not coalesced")
	}
	r.update("job", "delivering", nil)
	if got := events[len(events)-1]; got.Loaded != 100 || got.State != "delivering" {
		t.Fatalf("final counters missing: %+v", got)
	}
}
