package webview

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestDragJobOutlivesGestureAndOnlyConfirmsDelivery(t *testing.T) {
	var events []dragJob
	r := newDragJobRegistry(func(job dragJob) { events = append(events, job) })
	defer r.close()
	ctx, ok := r.add("drag", "session", []string{"/home/user/file"})
	if !ok {
		t.Fatal("add")
	}
	r.update("drag", "accepted", nil)
	r.update("drag", "caching", nil)
	if ctx.Err() != nil {
		t.Fatal("gesture must not cancel accepted transfer")
	}
	r.update("drag", "delivering", nil)
	if r.cancel("drag") {
		t.Fatal("cannot cancel Shell's copy after paths were handed over")
	}
	if got := r.snapshot()[0].State; got != "delivering" {
		t.Fatal(got)
	}
	r.update("drag", "done", nil)
	if ctx.Err() != context.Canceled {
		t.Fatal("terminal event must release context")
	}
	r.update("drag", "error", errors.New("late error"))
	if got := r.snapshot()[0].State; got != "done" {
		t.Fatal(got)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Revision <= events[i-1].Revision {
			t.Fatal("non-monotonic revision")
		}
	}
}

func TestDragJobCancellationAndHostClose(t *testing.T) {
	r := newDragJobRegistry(nil)
	ctx, _ := r.add("first", "session", []string{"/home/user/file"})
	r.update("first", "caching", nil)
	if !r.cancel("first") || ctx.Err() != context.Canceled {
		t.Fatal("cancel did not interrupt context")
	}
	r.update("first", "done", nil)
	if got := r.snapshot()[0].State; got != "cancelled" {
		t.Fatal(got)
	}
	second, _ := r.add("second", "session", nil)
	r.close()
	if second.Err() != context.Canceled {
		t.Fatal("host close left transfer alive")
	}
	if _, ok := r.add("third", "session", nil); ok {
		t.Fatal("closed registry accepted a job")
	}
}

func TestDragJobSnapshotIsolationAndHistoryBound(t *testing.T) {
	r := newDragJobRegistry(nil)
	defer r.close()
	paths := []string{"original"}
	r.add("active", "session", paths)
	paths[0] = "changed"
	for i := 0; i < 80; i++ {
		id := fmt.Sprint(i)
		r.add(id, "session", nil)
		r.update(id, "done", nil)
	}
	jobs := r.snapshot()
	if len(jobs) > 50 {
		t.Fatal("unbounded completed history")
	}
	found := false
	for _, job := range jobs {
		if job.ID == "active" {
			found = true
			if job.Paths[0] != "original" {
				t.Fatal("paths aliased")
			}
			job.Paths[0] = "mutated"
		}
	}
	if !found {
		t.Fatal("active job pruned")
	}
	for _, job := range r.snapshot() {
		if job.ID == "active" && job.Paths[0] != "original" {
			t.Fatal("snapshot aliased")
		}
	}
}
