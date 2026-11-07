package gui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chemssh-launcher/internal/dragtransfer"
	"chemssh-launcher/internal/sftpclient"
)

func TestDragFileReportsNestedAndEmptySelectedFolders(t *testing.T) {
	source := &progressStageSource{fakeDragStageSource: fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{
			"/root":  {Name: "root", IsDir: true},
			"/empty": {Name: "empty", IsDir: true},
		},
		lists: map[string]sftpclient.ListResult{
			"/root":        {Entries: []sftpclient.RemoteEntry{{Name: "nested", Path: "/root/nested", IsDir: true}, {Name: "zero", Path: "/root/zero", Size: 99}}},
			"/root/nested": {Entries: []sftpclient.RemoteEntry{{Name: "file", Path: "/root/nested/file", Size: 99}}},
			"/empty":       {},
		},
		files: map[string]string{"/root/nested/file": "abcdef", "/root/zero": ""},
	}}
	var reports []dragtransfer.FileProgress
	var total, loaded int64
	ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{
		File:      func(p dragtransfer.FileProgress) { reports = append(reports, p) },
		AddTotal:  func(n int64) { total += n },
		AddLoaded: func(n int64) { loaded += n },
	})
	if _, err := stageDragPathsContext(ctx, source, "session", []string{"/root", "/empty"}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if total != 6 || loaded != 6 {
		t.Fatalf("aggregate = %d/%d", loaded, total)
	}
	last := make(map[string]dragtransfer.FileProgress)
	var fileLoads []int64
	for _, p := range reports {
		last[p.Path] = p
		if p.Path == "/root/nested/file" && p.State == "caching" && p.Loaded > 0 {
			fileLoads = append(fileLoads, p.Loaded)
		}
	}
	for _, remote := range []string{"/root", "/root/nested", "/empty"} {
		if p := last[remote]; p.Kind != "folder" || p.State != "done" || p.Loaded != 0 || p.Total != 0 {
			t.Fatalf("folder report %s: %+v", remote, p)
		}
	}
	if p := last["/root/nested/file"]; p.Kind != "file" || p.State != "done" || p.Loaded != 6 || p.Total != 6 {
		t.Fatalf("nested file: %+v", p)
	}
	if len(fileLoads) != 3 || fileLoads[0] != 2 || fileLoads[1] != 4 || fileLoads[2] != 6 {
		t.Fatalf("write reports: %v", fileLoads)
	}
	if p := last["/root/zero"]; p.Kind != "file" || p.State != "done" || p.Loaded != 0 || p.Total != 0 {
		t.Fatalf("zero byte completion: %+v", p)
	}
	if reports[0].Path != "/root" || reports[0].Kind != "folder" || reports[0].State != "caching" {
		t.Fatalf("root not discovered first: %+v", reports[0])
	}
}

type failedDragFileSource struct{ fakeDragStageSource }

func (s failedDragFileSource) Download(_, remote string) (io.ReadCloser, sftpclient.FileInfo, error) {
	return io.NopCloser(&failedDragFileReader{}), sftpclient.FileInfo{Size: 9}, nil
}

type failedDragFileReader struct{ read bool }

func (r *failedDragFileReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.ErrUnexpectedEOF
	}
	r.read = true
	return copy(p, "abc"), nil
}

func TestDragFileReportsFailureAndPartialWrites(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		source                dragStageSource
		remote                string
		wantLoaded, wantTotal int64
	}{
		{"read failure", failedDragFileSource{}, "/file", 3, 9},
		{"download failure", fakeDragStageSource{}, "/missing", 0, 0},
		{"size mismatch", shortDragSource{fakeDragStageSource{files: map[string]string{"/file": "abc"}}}, "/file", 3, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reports []dragtransfer.FileProgress
			ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{File: func(p dragtransfer.FileProgress) { reports = append(reports, p) }})
			err := copyDragFileContext(ctx, tc.source, "session", tc.remote, filepath.Join(t.TempDir(), "file"))
			if err == nil {
				t.Fatal("expected failure")
			}
			p := reports[len(reports)-1]
			if p.Path != tc.remote || p.Kind != "file" || p.State != "error" || p.Error != err.Error() || p.Loaded != tc.wantLoaded || p.Total != tc.wantTotal {
				t.Fatalf("failure report: %+v, err %v", p, err)
			}
		})
	}
}

func TestDragFileReportsStatFailure(t *testing.T) {
	var reports []dragtransfer.FileProgress
	ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{File: func(p dragtransfer.FileProgress) { reports = append(reports, p) }})
	dir := t.TempDir()
	_, err := stageDragPathsContext(ctx, fakeDragStageSource{}, "session", []string{"/missing"}, dir)
	if err == nil || len(reports) != 1 {
		t.Fatalf("stat failure = %v, reports %+v", err, reports)
	}
	p := reports[0]
	if p.Path != "/missing" || p.Kind != "file" || p.State != "error" || p.Error != err.Error() || p.Loaded != 0 || p.Total != 0 {
		t.Fatalf("stat error report: %+v", p)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat failure left stage: %v", err)
	}
}

func TestDragFileReportsCancellationWhilePaused(t *testing.T) {
	var gate testDragPause
	defer gate.run()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &progressStageSource{fakeDragStageSource: fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{"/root": {Name: "root", IsDir: true}},
		lists: map[string]sftpclient.ListResult{"/root": {Entries: []sftpclient.RemoteEntry{
			{Name: "file", Path: "/root/file"}, {Name: "later", Path: "/root/later"},
		}}},
		files: map[string]string{"/root/file": "abcdef", "/root/later": "later"},
	}}
	var reports []dragtransfer.FileProgress
	var loaded int64
	paused := make(chan (<-chan struct{}), 1)
	ctx = dragtransfer.WithHooks(ctx, dragtransfer.Hooks{
		Wait: gate.wait,
		AddLoaded: func(n int64) {
			loaded += n
			if loaded == 2 {
				paused <- gate.pause()
			}
		},
		File: func(p dragtransfer.FileProgress) { reports = append(reports, p) },
	})
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { _, err := stageDragPathsContext(ctx, source, "session", []string{"/root"}, dir); done <- err }()
	var waiting <-chan struct{}
	select {
	case waiting = <-paused:
	case <-time.After(time.Second):
		t.Fatal("first write did not pause")
	}
	waitDragSignal(t, waiting, "next file read did not pause")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock pause")
	}
	last := make(map[string]dragtransfer.FileProgress)
	for _, p := range reports {
		last[p.Path] = p
		if p.State == "done" {
			t.Fatalf("cancelled tree reported completion: %+v", p)
		}
	}
	file, folder := last["/root/file"], last["/root"]
	if file.Kind != "file" || file.State != "error" || file.Error != context.Canceled.Error() || file.Loaded != 2 || file.Total != 6 {
		t.Fatalf("cancelled file snapshot: %+v", file)
	}
	if folder.Kind != "folder" || folder.State != "error" || folder.Error != context.Canceled.Error() || folder.Loaded != 0 || folder.Total != 0 {
		t.Fatalf("cancelled folder snapshot: %+v", folder)
	}
	if _, ok := last["/root/later"]; ok {
		t.Fatal("cancel reported undiscovered sibling")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel left stage: %v", err)
	}
}

func TestDragFileAndFolderReportLocalCreationFailures(t *testing.T) {
	for _, kind := range []string{"file", "folder"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "occupied")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			var reports []dragtransfer.FileProgress
			ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{File: func(p dragtransfer.FileProgress) { reports = append(reports, p) }})
			var err error
			if kind == "file" {
				err = copyDragFileContext(ctx, fakeDragStageSource{files: map[string]string{"/path": "abc"}}, "session", "/path", target)
			} else {
				err = stageDragDirectoryContext(ctx, fakeDragStageSource{}, "session", "/path", target)
			}
			if err == nil || len(reports) == 0 {
				t.Fatalf("creation error = %v, reports %+v", err, reports)
			}
			p := reports[len(reports)-1]
			if p.Path != "/path" || p.Kind != kind || p.State != "error" || p.Error != err.Error() || p.Loaded != 0 {
				t.Fatalf("creation failure: %+v", p)
			}
			assertStagedFile(t, target, "keep")
		})
	}
}

func TestDragFolderReportsListingFailure(t *testing.T) {
	source := fakeDragStageSource{stats: map[string]sftpclient.FileInfo{"/root": {Name: "root", IsDir: true}}}
	var reports []dragtransfer.FileProgress
	ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{File: func(p dragtransfer.FileProgress) { reports = append(reports, p) }})
	_, err := stageDragPathsContext(ctx, source, "session", []string{"/root"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "missing listing") {
		t.Fatal(err)
	}
	p := reports[len(reports)-1]
	if p.Path != "/root" || p.Kind != "folder" || p.State != "error" || p.Error != err.Error() {
		t.Fatalf("folder failure: %+v", p)
	}
}
