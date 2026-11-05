package gui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chemssh-launcher/internal/dragtransfer"
	"chemssh-launcher/internal/sftpclient"
)

// testDragPause models a host gate. Pausing creates a new resume signal; Wait
// blocks on that signal or cancellation, without blocking the control thread.
type testDragPause struct {
	mu       sync.Mutex
	resume   chan struct{}
	waiting  chan struct{}
	signaled bool
}

func (p *testDragPause) pause() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resume = make(chan struct{})
	p.waiting = make(chan struct{})
	p.signaled = false
	return p.waiting
}

func (p *testDragPause) run() {
	p.mu.Lock()
	if p.resume != nil {
		close(p.resume)
		p.resume = nil
	}
	p.mu.Unlock()
}

func (p *testDragPause) wait(ctx context.Context) error {
	p.mu.Lock()
	resume := p.resume
	if resume != nil && !p.signaled {
		close(p.waiting)
		p.signaled = true
	}
	p.mu.Unlock()
	if resume == nil {
		return ctx.Err()
	}
	select {
	case <-resume:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitDragSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(label)
	}
}

func TestDragProgressCountsNestedFilesOnce(t *testing.T) {
	source := fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{
			"/file": {Name: "file", Size: 999},
			"/dir":  {Name: "dir", IsDir: true, Size: 999},
		},
		lists: map[string]sftpclient.ListResult{
			"/dir": {Entries: []sftpclient.RemoteEntry{
				{Name: "nested", Path: "/dir/nested", IsDir: true, Size: 999},
				{Name: "loop", Path: "/dir/loop", IsDir: true, IsSymlink: true, Size: 999},
				{Name: "empty", Path: "/dir/empty", Size: 999},
			}},
			"/dir/nested": {Entries: []sftpclient.RemoteEntry{
				{Name: "second", Path: "/dir/nested/second", Size: 999},
			}},
		},
		files: map[string]string{"/file": "abc", "/dir/nested/second": "de", "/dir/empty": ""},
	}
	var total, loaded int64
	var totals, loads []int64
	ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{
		AddTotal:  func(n int64) { total += n; totals = append(totals, n) },
		AddLoaded: func(n int64) { loaded += n; loads = append(loads, n) },
	})
	staged, err := stageDragPathsContext(ctx, source, "session", []string{"/file", "/dir"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || loaded != 5 || len(totals) != 2 || len(loads) != 2 {
		t.Fatalf("progress = total %d %v, loaded %d %v; want only stream bytes counted once", total, totals, loaded, loads)
	}
	assertStagedFile(t, staged[0], "abc")
	assertStagedFile(t, filepath.Join(staged[1], "nested", "second"), "de")
	assertStagedFile(t, filepath.Join(staged[1], "empty"), "")
}

// progressStageSource can pause at precise boundaries and limits each read so
// the test distinguishes the first completed write from the next read.
type progressStageSource struct {
	fakeDragStageSource
	calls  atomic.Int32
	reads  atomic.Int32
	onStat func()
	onList func()
}

func (s *progressStageSource) Stat(session, remote string) (sftpclient.FileInfo, error) {
	s.calls.Add(1)
	if s.onStat != nil {
		s.onStat()
	}
	return s.fakeDragStageSource.Stat(session, remote)
}

func (s *progressStageSource) List(session, remote string) (sftpclient.ListResult, error) {
	s.calls.Add(1)
	if s.onList != nil {
		s.onList()
	}
	return s.fakeDragStageSource.List(session, remote)
}

func (s *progressStageSource) Download(session, remote string) (io.ReadCloser, sftpclient.FileInfo, error) {
	s.calls.Add(1)
	content, ok := s.files[remote]
	if !ok {
		return nil, sftpclient.FileInfo{}, errors.New("missing file")
	}
	return &progressStageReader{source: s, reader: strings.NewReader(content)}, sftpclient.FileInfo{Size: int64(len(content))}, nil
}

type progressStageReader struct {
	source *progressStageSource
	reader *strings.Reader
}

func (r *progressStageReader) Read(p []byte) (int, error) {
	r.source.reads.Add(1)
	if len(p) > 2 {
		p = p[:2]
	}
	return r.reader.Read(p)
}
func (*progressStageReader) Close() error { return nil }

func TestDragPauseBeforeMetadataAndResume(t *testing.T) {
	var gate testDragPause
	waiting := gate.pause()
	defer gate.run()
	source := &progressStageSource{fakeDragStageSource: fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{"/file": {Name: "file"}}, files: map[string]string{"/file": "abcd"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = dragtransfer.WithHooks(ctx, dragtransfer.Hooks{Wait: gate.wait})
	done := make(chan error, 1)
	dir := t.TempDir()
	go func() { _, err := stageDragPathsContext(ctx, source, "session", []string{"/file"}, dir); done <- err }()
	waitDragSignal(t, waiting, "staging did not pause before metadata")
	if source.calls.Load() != 0 {
		t.Fatal("paused staging performed remote IO")
	}
	gate.run()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not finish staging")
	}
	assertStagedFile(t, filepath.Join(dir, "file"), "abcd")
}

func TestDragPauseDuringReadsResumeOrCancel(t *testing.T) {
	for _, cancelPaused := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancelPaused], func(t *testing.T) {
			var gate testDragPause
			defer gate.run()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &progressStageSource{fakeDragStageSource: fakeDragStageSource{
				stats: map[string]sftpclient.FileInfo{"/file": {Name: "file"}}, files: map[string]string{"/file": "abcdef"},
			}}
			var loaded, total atomic.Int64
			paused := make(chan (<-chan struct{}), 1)
			ctx = dragtransfer.WithHooks(ctx, dragtransfer.Hooks{
				Wait:     gate.wait,
				AddTotal: func(n int64) { total.Add(n) },
				AddLoaded: func(n int64) {
					if loaded.Add(n) == 2 {
						paused <- gate.pause()
					}
				},
			})
			dir := t.TempDir()
			done := make(chan error, 1)
			go func() { _, err := stageDragPathsContext(ctx, source, "session", []string{"/file"}, dir); done <- err }()
			var waiting <-chan struct{}
			select {
			case waiting = <-paused:
			case <-time.After(time.Second):
				t.Fatal("first write did not report progress")
			}
			waitDragSignal(t, waiting, "next read did not pause")
			if source.reads.Load() != 1 || loaded.Load() != 2 || total.Load() != 6 {
				t.Fatalf("pause boundary = reads %d, loaded %d, total %d", source.reads.Load(), loaded.Load(), total.Load())
			}
			if cancelPaused {
				cancel()
			} else {
				gate.run()
			}
			select {
			case err := <-done:
				if cancelPaused {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("paused cancel = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("paused staging did not unblock")
			}
			if cancelPaused {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancel left partial stage: %v", err)
				}
				if loaded.Load() != 2 || source.reads.Load() != 1 {
					t.Fatal("cancel advanced the paused stream")
				}
			} else {
				if loaded.Load() != 6 {
					t.Fatalf("resumed loaded = %d", loaded.Load())
				}
				assertStagedFile(t, filepath.Join(dir, "file"), "abcdef")
			}
		})
	}
}

func TestDragPauseAtDirectoryTraversalPreventsNextDownload(t *testing.T) {
	var gate testDragPause
	defer gate.run()
	waiting := make(chan (<-chan struct{}), 1)
	source := &progressStageSource{fakeDragStageSource: fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{"/dir": {Name: "dir", IsDir: true}},
		lists: map[string]sftpclient.ListResult{"/dir": {Entries: []sftpclient.RemoteEntry{{Name: "file", Path: "/dir/file"}}}},
		files: map[string]string{"/dir/file": "bytes"},
	}}
	source.onList = func() { waiting <- gate.pause() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = dragtransfer.WithHooks(ctx, dragtransfer.Hooks{Wait: gate.wait})
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { _, err := stageDragPathsContext(ctx, source, "session", []string{"/dir"}, dir); done <- err }()
	var blocked <-chan struct{}
	select {
	case blocked = <-waiting:
	case <-time.After(time.Second):
		t.Fatal("directory was not listed")
	}
	waitDragSignal(t, blocked, "directory traversal did not pause")
	if source.calls.Load() != 2 {
		t.Fatal("paused traversal opened the next file")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock traversal")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel left directory stage: %v", err)
	}
}

type dragShortWriter struct {
	n   int
	err error
}

func (w dragShortWriter) Write([]byte) (int, error) { return w.n, w.err }

func TestDragProgressWriterCountsOnlySuccessfulBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"full", 4, nil}, {"short", 2, nil}, {"partial failure", 2, io.ErrClosedPipe}, {"failed", 0, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loaded int64
			ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{AddLoaded: func(n int64) { loaded += n }})
			writer := &dragProgressWriter{ctx: ctx, writer: dragShortWriter{n: tc.n, err: tc.err}}
			n, err := io.CopyBuffer(writer, strings.NewReader("abcd"), make([]byte, 8))
			if n != int64(tc.n) || loaded != int64(tc.n) {
				t.Fatalf("written %d loaded %d, want %d", n, loaded, tc.n)
			}
			want := tc.err
			if tc.n < 4 && want == nil {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v want %v", err, want)
			}
		})
	}
}

func TestDragProgressKeepsSizeMismatchFailure(t *testing.T) {
	source := shortDragSource{fakeDragStageSource{files: map[string]string{"/file": "abc"}}}
	var loaded, total int64
	ctx := dragtransfer.WithHooks(context.Background(), dragtransfer.Hooks{AddLoaded: func(n int64) { loaded += n }, AddTotal: func(n int64) { total += n }})
	target := filepath.Join(t.TempDir(), "file")
	if err := copyDragFileContext(ctx, source, "session", "/file", target); err == nil {
		t.Fatal("truncated stream reported success")
	}
	if loaded != 3 || total != 4 {
		t.Fatalf("truncated progress = %d/%d", loaded, total)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains: %v", err)
	}
}
