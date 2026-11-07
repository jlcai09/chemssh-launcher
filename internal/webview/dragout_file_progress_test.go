package webview

import (
	"context"
	"sync"
	"testing"
	"time"

	"chemssh-launcher/internal/dragtransfer"
)

func TestDragJobFileProgressMergesPathsWithoutAggregateDuplication(t *testing.T) {
	var events []dragJob
	r := newDragJobRegistry(func(job dragJob) { events = append(events, job) })
	defer r.close()
	ctx, _ := r.add("job", "session", []string{"/a", "/b"})
	r.update("job", "caching", nil)
	dragtransfer.ReportFile(ctx, dragtransfer.FileProgress{Path: "/a", Kind: "folder", State: "caching"})
	a := dragtransfer.WithFile(ctx, "/a/file", 5)
	b := dragtransfer.WithFile(ctx, "/b/file", 7)
	dragtransfer.AddTotal(a, 5)
	dragtransfer.AddTotal(b, 7)
	dragtransfer.AddLoaded(a, 2)
	dragtransfer.AddLoaded(b, 3)
	dragtransfer.AddLoaded(a, 3)
	dragtransfer.ReportFile(a, dragtransfer.FileProgress{Path: "/a/file", Kind: "file", Loaded: 5, Total: 5, State: "done"})
	job := r.snapshot()[0]
	if len(job.Files) != 3 || job.Loaded != 8 || job.Total != 12 {
		t.Fatalf("job = %+v", job)
	}
	if job.Files[1].Path != "/a/file" || job.Files[1].Loaded != 5 || job.Files[1].State != "done" || job.Files[2].Path != "/b/file" || job.Files[2].Loaded != 3 {
		t.Fatalf("merged paths: %+v", job.Files)
	}
	if last := events[len(events)-1]; last.Files[1].State != "done" || last.Files[1].Loaded != 5 {
		t.Fatalf("completion not flushed: %+v", last)
	}
}

func TestDragJobFilesSnapshotAndAllEventsAreIsolated(t *testing.T) {
	var events []dragJob
	r := newDragJobRegistry(func(job dragJob) { events = append(events, job) })
	defer r.close()
	ctx, _ := r.add("job", "session", []string{"/file"})
	r.update("job", "caching", nil)
	p := dragtransfer.FileProgress{Path: "/file", Kind: "file", Total: 10, State: "caching"}
	dragtransfer.ReportFile(ctx, p)
	for _, emit := range []func(){
		func() { r.update("job", "caching", nil) },
		func() { r.control("job", "pause") },
		func() { r.control("job", "resume") },
		func() {
			r.mu.Lock()
			r.jobs["job"].lastProgress = time.Time{}
			r.mu.Unlock()
			dragtransfer.AddLoaded(ctx, 1)
		},
		func() { p.Loaded = 5; p.State = "done"; dragtransfer.ReportFile(ctx, p) },
	} {
		emit()
		last := events[len(events)-1]
		last.Paths[0] = "mutated event path"
		last.Files[0].Path = "mutated event file"
		last.Files[0].Loaded = -1
		if got := r.snapshot()[0]; got.Paths[0] != "/file" || got.Files[0].Path != "/file" || got.Files[0].Loaded < 0 {
			t.Fatalf("event aliases registry: %+v", got)
		}
	}
	job := r.snapshot()[0]
	job.Files[0].Path = "mutated snapshot"
	job.Files[0].Error = "mutated"
	if got := r.snapshot()[0].Files[0]; got.Path != "/file" || got.Error != "" {
		t.Fatalf("snapshot aliases registry: %+v", got)
	}
	// Terminal update and cancel events must be detached too.
	for _, terminal := range []func(){func() { r.update("job", "error", context.Canceled) }} {
		terminal()
		last := events[len(events)-1]
		last.Files[0].Path = "terminal mutation"
		if r.snapshot()[0].Files[0].Path != "/file" {
			t.Fatal("terminal event aliases registry")
		}
	}
	r2 := newDragJobRegistry(func(job dragJob) {
		if len(job.Files) > 0 {
			job.Files[0].Path = "cancel mutation"
		}
	})
	defer r2.close()
	ctx2, _ := r2.add("cancel", "session", nil)
	dragtransfer.ReportFile(ctx2, p)
	r2.cancel("cancel")
	if r2.snapshot()[0].Files[0].Path != "/file" {
		t.Fatal("cancel event aliases registry")
	}
}

func TestDragJobFileUpdatesThrottleAndIgnoreLateReports(t *testing.T) {
	var events []dragJob
	r := newDragJobRegistry(func(job dragJob) { events = append(events, job) })
	defer r.close()
	ctx, _ := r.add("job", "session", nil)
	r.update("job", "caching", nil)
	p := dragtransfer.FileProgress{Path: "/file", Kind: "file", Total: 1000, State: "caching"}
	dragtransfer.ReportFile(ctx, p)
	before := len(events)
	for i := 1; i <= 100; i++ {
		p.Loaded = int64(i)
		dragtransfer.ReportFile(ctx, p)
	}
	if len(events) != before {
		t.Fatalf("unthrottled file updates: %d", len(events)-before)
	}
	if r.snapshot()[0].Files[0].Loaded != 100 {
		t.Fatal("throttle lost snapshot progress")
	}
	r.update("job", "delivering", nil)
	if last := events[len(events)-1]; last.Files[0].Loaded != 100 {
		t.Fatal("delivery did not flush file progress")
	}
	p.Loaded = 200
	dragtransfer.ReportFile(ctx, p)
	if r.snapshot()[0].Files[0].Loaded != 100 {
		t.Fatal("late file update changed delivery")
	}
	r.update("job", "done", nil)
	dragtransfer.ReportFile(ctx, p)
	if r.snapshot()[0].Files[0].Loaded != 100 {
		t.Fatal("late file update changed terminal job")
	}
}

func TestDragJobConcurrentFileSnapshotsAndEvents(t *testing.T) {
	var mu sync.Mutex
	var retained []dragJob
	r := newDragJobRegistry(func(job dragJob) { mu.Lock(); retained = append(retained, job); mu.Unlock() })
	defer r.close()
	ctx, _ := r.add("job", "session", nil)
	r.update("job", "caching", nil)
	dragtransfer.ReportFile(ctx, dragtransfer.FileProgress{Path: "/file", Kind: "file", State: "caching"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			dragtransfer.ReportFile(ctx, dragtransfer.FileProgress{Path: "/file", Kind: "file", Loaded: int64(i), State: "caching"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			job := r.snapshot()[0]
			job.Files[0].Loaded = -1
			mu.Lock()
			for _, old := range retained {
				if len(old.Files) > 0 {
					old.Files[0].Loaded = -1
				}
			}
			mu.Unlock()
		}
	}()
	wg.Wait()
	if r.snapshot()[0].Files[0].Loaded != 199 {
		t.Fatal("snapshot mutation reached registry")
	}
}
