package dragtransfer

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestFileScopesKeepHooksAndIsolatePaths(t *testing.T) {
	type key struct{}
	var total, loaded int64
	var waits int
	var reports []FileProgress
	ctx := WithHooks(context.WithValue(context.Background(), key{}, "value"), Hooks{
		Wait:      func(context.Context) error { waits++; return nil },
		AddTotal:  func(n int64) { total += n },
		AddLoaded: func(n int64) { loaded += n },
		File:      func(p FileProgress) { reports = append(reports, p) },
	})
	a := WithFile(ctx, "/a/file", 5)
	b := WithFile(a, "/b/file", 9)
	AddTotal(a, 5)
	AddTotal(b, 9)
	AddLoaded(a, 2)
	AddLoaded(b, 4)
	AddLoaded(a, 3)
	AddLoaded(ctx, 1)
	AddLoaded(a, 0)
	AddLoaded(b, -1)
	if err := Wait(b); err != nil || waits != 1 || b.Value(key{}) != "value" {
		t.Fatal("file scope lost parent values or pause hooks")
	}
	if total != 14 || loaded != 10 {
		t.Fatalf("aggregate counted more than once: %d/%d", loaded, total)
	}
	if len(reports) != 5 || reports[2].Path != "/a/file" || reports[2].Loaded != 2 || reports[3].Path != "/b/file" || reports[3].Loaded != 4 || reports[4].Loaded != 5 {
		t.Fatalf("path counters leaked: %+v", reports)
	}
}

func TestReportFileOptionalAndJSON(t *testing.T) {
	ReportFile(context.Background(), FileProgress{})
	AddLoaded(WithFile(context.Background(), "/file", 1), 1)
	p := FileProgress{Path: "/empty", Kind: "file", State: "done"}
	var got FileProgress
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithHooks(ctx, Hooks{File: func(p FileProgress) { got = p }})
	cancel()
	ReportFile(ctx, p)
	if got != p {
		t.Fatal("final reports must survive cancellation")
	}
	data, err := json.Marshal(p)
	if err != nil || string(data) != `{"path":"/empty","kind":"file","loaded":0,"total":0,"state":"done"}` {
		t.Fatalf("JSON contract: %s, %v", data, err)
	}
	p = FileProgress{Path: "/folder", Kind: "folder", State: "error", Error: "listing failed"}
	data, err = json.Marshal(p)
	if err != nil || string(data) != `{"path":"/folder","kind":"folder","loaded":0,"total":0,"state":"error","error":"listing failed"}` {
		t.Fatalf("JSON error contract: %s, %v", data, err)
	}
}

func TestFileScopeConcurrentWrites(t *testing.T) {
	var mu sync.Mutex
	var max int64
	ctx := WithHooks(context.Background(), Hooks{File: func(p FileProgress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Loaded > max {
			max = p.Loaded
		}
	}})
	ctx = WithFile(ctx, "/file", 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); AddLoaded(ctx, 1) }()
	}
	wg.Wait()
	if max != 100 {
		t.Fatalf("loaded = %d", max)
	}
}
