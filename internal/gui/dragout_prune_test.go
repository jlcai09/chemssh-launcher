package gui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"chemssh-launcher/internal/sftpclient"
)

func TestPruneDragStagePreservesActiveDirectoryUntilReleased(t *testing.T) {
	root := t.TempDir()
	active, err := createDragStage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragStage(active)
	stale := filepath.Join(root, dragStageDirPrefix+"completed")
	fresh := filepath.Join(root, dragStageDirPrefix+"fresh")
	unrelated := filepath.Join(root, "other-cache")
	for _, dir := range []string{stale, fresh, unrelated} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{active, stale, fresh, unrelated} {
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte("contents"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-dragStageMaxAge - time.Hour)
	for _, dir := range []string{active, stale, unrelated} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Several background jobs can start (and prune) at the same time.
	var prunes sync.WaitGroup
	for range 8 {
		prunes.Add(1)
		go func() {
			defer prunes.Done()
			pruneDragStage(root)
		}()
	}
	prunes.Wait()
	for _, dir := range []string{active, fresh, unrelated} {
		assertStagedFile(t, filepath.Join(dir, "file"), "contents")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired completed directory remains: %v", err)
	}

	// Finishing only unregisters the directory; the existing completed-copy
	// age policy then applies on a later prune, without resetting its age.
	releaseDragStage(active)
	assertStagedFile(t, filepath.Join(active, "file"), "contents")
	pruneDragStage(root)
	if _, err := os.Stat(active); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released expired directory was not pruned: %v", err)
	}
}

type pruneBlockedSource struct {
	fakeDragStageSource
	started chan struct{}
	ctx     context.Context
}

func (s *pruneBlockedSource) Download(_, _ string) (io.ReadCloser, sftpclient.FileInfo, error) {
	return &pruneBlockedReader{ctx: s.ctx, started: s.started}, sftpclient.FileInfo{Size: 8}, nil
}

type pruneBlockedReader struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (r *pruneBlockedReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (*pruneBlockedReader) Close() error { return nil }

func TestPruneDragStageDoesNotDeleteBlockedDownload(t *testing.T) {
	root := t.TempDir()
	dir, err := createDragStage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragStage(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &pruneBlockedSource{
		fakeDragStageSource: fakeDragStageSource{
			stats: map[string]sftpclient.FileInfo{"/blocked": {Name: "blocked"}},
		},
		ctx: ctx, started: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		defer releaseDragStage(dir)
		_, err := stageDragPathsContext(ctx, source, "session", []string{"/blocked"}, dir)
		done <- err
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("download did not block")
	}
	old := time.Now().Add(-dragStageMaxAge - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	pruneDragStage(root)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("prune deleted the active blocked staging directory: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("staging did not cancel")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled directory remains: %v", err)
	}
}

func TestSuccessfulDragStageProtectedUntilContextEnds(t *testing.T) {
	root := t.TempDir()
	dir, err := createDragStage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDragStage(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{"/file": {Name: "file"}},
		files: map[string]string{"/file": "complete"},
	}
	staged, err := stageDragPathsContext(ctx, source, "session", []string{"/file"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-dragStageMaxAge - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	finishDragStage(ctx, dir, true)
	// Staging has returned successfully, but the Shell still owns the files.
	pruneDragStage(root)
	assertStagedFile(t, staged[0], "complete")
	after, err := os.Stat(dir)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("successful handoff changed directory age: %v", err)
	}
	cancel()
	waitForDragStageRelease(t, dir)
	// Context completion only drops protection; the existing prune policy owns
	// deletion, and still sees the original old mtime.
	assertStagedFile(t, staged[0], "complete")
	after, err = os.Stat(dir)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("context completion changed directory age: %v", err)
	}
	pruneDragStage(root)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed drag was not eligible for pruning: %v", err)
	}
}

func TestFinishDragStageReleasesImmediatelyWithoutHandoff(t *testing.T) {
	for _, name := range []string{"failed", "background", "already cancelled"} {
		t.Run(name, func(t *testing.T) {
			dir, err := createDragStage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer releaseDragStage(dir)
			ctx := context.Background()
			completed := true
			if name != "background" {
				cancellable, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = cancellable
				completed = name != "failed"
				if name == "already cancelled" {
					cancel()
				}
			}
			finishDragStage(ctx, dir, completed)
			activeDragStages.Lock()
			_, active := activeDragStages.dirs[dir]
			activeDragStages.Unlock()
			if active {
				t.Fatal("directory retained protection without a context handoff")
			}
		})
	}
}

func waitForDragStageRelease(t *testing.T, dir string) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		activeDragStages.Lock()
		_, active := activeDragStages.dirs[dir]
		activeDragStages.Unlock()
		if !active {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("context completion did not release stage protection")
		}
	}
}
