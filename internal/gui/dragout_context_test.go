package gui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chemssh-launcher/internal/sftpclient"
)

type contextDragStageSource struct {
	fakeDragStageSource
	started chan struct{}
	calls   atomic.Int32
	blocked string
	cancel  context.CancelFunc
}

func (s *contextDragStageSource) StatContext(ctx context.Context, session, remote string) (sftpclient.FileInfo, error) {
	s.calls.Add(1)
	return s.Stat(session, remote)
}
func (s *contextDragStageSource) ListContext(ctx context.Context, session, remote string) (sftpclient.ListResult, error) {
	s.calls.Add(1)
	return s.List(session, remote)
}
func (s *contextDragStageSource) DownloadContext(ctx context.Context, session, remote string) (io.ReadCloser, sftpclient.FileInfo, error) {
	s.calls.Add(1)
	if remote == s.blocked {
		reader, writer := io.Pipe()
		go func() {
			close(s.started)
			<-ctx.Done()
			_ = writer.CloseWithError(ctx.Err())
		}()
		return reader, sftpclient.FileInfo{Name: filepath.Base(remote), Size: 8}, nil
	}
	if s.cancel != nil {
		s.cancel()
		return io.NopCloser(strings.NewReader("")), sftpclient.FileInfo{Size: 8}, nil
	}
	return s.Download(session, remote)
}

func TestStageDragPathsContextCancelsBlockedReadAndRemovesAttempt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := &contextDragStageSource{
		fakeDragStageSource: fakeDragStageSource{
			stats: map[string]sftpclient.FileInfo{"/first": {Name: "first"}, "/blocked": {Name: "blocked"}, "/later": {Name: "later"}},
			files: map[string]string{"/first": "complete", "/later": "must not copy"},
		},
		blocked: "/blocked", started: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := stageDragPathsContext(ctx, source, "session", []string{"/first", "/blocked", "/later"}, dir)
		done <- err
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation rather than size error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked read was not cancelled")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial stage remains: %v", err)
	}
	if source.calls.Load() != 4 {
		t.Fatalf("later selected item was touched: %d calls", source.calls.Load())
	}
}

func TestStageDragPathsContextStopsRecursiveEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &contextDragStageSource{
		fakeDragStageSource: fakeDragStageSource{
			stats: map[string]sftpclient.FileInfo{"/root": {Name: "root", IsDir: true}},
			lists: map[string]sftpclient.ListResult{
				"/root":     {Entries: []sftpclient.RemoteEntry{{Name: "sub", Path: "/root/sub", IsDir: true}, {Name: "later", Path: "/root/later"}}},
				"/root/sub": {Entries: []sftpclient.RemoteEntry{{Name: "first", Path: "/root/sub/first"}, {Name: "later", Path: "/root/sub/later"}}},
			},
		},
		cancel: cancel,
	}
	if _, err := stageDragPathsContext(ctx, source, "session", []string{"/root"}, dir); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if source.calls.Load() != 4 {
		t.Fatalf("recursion continued after cancellation: %d calls", source.calls.Load())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage remains: %v", err)
	}
}

func TestStageDragPathsContextPreCancelledDoesNoIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &contextDragStageSource{}
	if _, err := stageDragPathsContext(ctx, source, "session", []string{"/file"}, filepath.Join(t.TempDir(), "stage")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if source.calls.Load() != 0 {
		t.Fatal("pre-cancelled stage performed remote IO")
	}
	// The server must check cancellation before resolving an SFTP manager or
	// creating a staging directory, so a nil manager is safe here.
	if _, err := (&Server{}).stageDragFiles(ctx, "session", []string{"/file"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStageDragPathsContextSuccessPreservesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := &contextDragStageSource{fakeDragStageSource: fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{"/file": {Name: "file"}}, files: map[string]string{"/file": "complete"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	staged, err := stageDragPathsContext(ctx, source, "session", []string{"/file"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	data, err := os.ReadFile(staged[0])
	if err != nil || string(data) != "complete" {
		t.Fatalf("successful stage lost: %q %v", data, err)
	}
}
