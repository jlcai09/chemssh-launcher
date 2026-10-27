package gui

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type offsetReader struct {
	data   []byte
	offset int
}

func (r *offsetReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

type failingReader struct {
	reader    io.Reader
	remaining int
	failed    bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.failed {
		return 0, errors.New("simulated connection drop")
	}
	if r.remaining <= 0 {
		r.failed = true
		return 0, errors.New("simulated connection drop")
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= n
	return n, err
}

func (r *failingReader) Close() error { return nil }

type flakySource struct {
	data          []byte
	modTime       time.Time
	failures      int
	failAfter     int
	changeOnRetry bool
	mu            sync.Mutex
	statCalls     int
	openOffsets   []int64
}

func newFlakySource(data []byte) *flakySource {
	return &flakySource{data: data, modTime: time.Unix(1700000000, 0)}
}

func (s *flakySource) Stat() (int64, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statCalls++
	if s.changeOnRetry && s.statCalls > 1 {
		s.modTime = s.modTime.Add(time.Second)
	}
	return int64(len(s.data)), s.modTime, nil
}

func (s *flakySource) OpenAt(offset int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.openOffsets = append(s.openOffsets, offset)
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()

	reader := &offsetReader{data: s.data, offset: int(offset)}
	if !fail {
		return io.NopCloser(reader), nil
	}
	return &failingReader{reader: reader, remaining: s.failAfter}, nil
}

func (s *flakySource) Reconnect() error { return nil }

func (s *flakySource) openedAt() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.openOffsets...)
}

func newTestTransferServer() *Server {
	return &Server{transfers: newTransferProgressStore(), logs: &safeLog{}}
}

func withFastTransferRetries(t *testing.T) {
	t.Helper()
	previous := transferRetryBaseDelay
	transferRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { transferRetryBaseDelay = previous })
}

func TestResumableTransferRetriesFromConfirmedOffset(t *testing.T) {
	withFastTransferRetries(t)
	server := newTestTransferServer()
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	src := newFlakySource(data)
	src.failures = 1
	src.failAfter = 1024
	targetPath := filepath.Join(t.TempDir(), "target.bin")
	dst := newLocalFileTarget(targetPath)

	if err := server.runResumableTransfer("retry-from-offset", false, "target.bin", src, dst); err != nil {
		t.Fatalf("runResumableTransfer: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("target content mismatch: got %d bytes, want %d", len(got), len(data))
	}
	offsets := src.openedAt()
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 1024 {
		t.Fatalf("open offsets = %v, want [0 1024]", offsets)
	}
	progress, ok := server.transfers.snapshot("retry-from-offset")
	if !ok || !progress.Done || progress.Error != "" || progress.Loaded != int64(len(data)) {
		t.Fatalf("progress = %+v, want done at full size", progress)
	}
	if progress.Retrying {
		t.Fatalf("retrying flag still set after success: %+v", progress)
	}
}

func TestResumableTransferResumesPartialTarget(t *testing.T) {
	withFastTransferRetries(t)
	server := newTestTransferServer()
	data := bytes.Repeat([]byte("resume-me"), 2000)
	src := newFlakySource(data)
	targetPath := filepath.Join(t.TempDir(), "partial.bin")
	if err := os.WriteFile(targetPath, data[:4000], 0o644); err != nil {
		t.Fatalf("seed partial target: %v", err)
	}
	if _, err := server.transfers.begin("partial-resume", int64(len(data)), false); err != nil {
		t.Fatalf("begin: %v", err)
	}
	server.transfers.add("partial-resume", 4000)
	server.transfers.finish("partial-resume", errors.New("simulated interruption"))

	dst := newLocalFileTarget(targetPath)
	if err := server.runResumableTransfer("partial-resume", true, "partial.bin", src, dst); err != nil {
		t.Fatalf("runResumableTransfer: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("target content mismatch after resume: got %d bytes, want %d", len(got), len(data))
	}
	offsets := src.openedAt()
	if len(offsets) != 1 || offsets[0] != 4000 {
		t.Fatalf("open offsets = %v, want [4000]", offsets)
	}
}

func TestResumableTransferStopsWhenSourceChanges(t *testing.T) {
	withFastTransferRetries(t)
	server := newTestTransferServer()
	data := bytes.Repeat([]byte("x"), 8192)
	src := newFlakySource(data)
	src.failures = 1
	src.failAfter = 2048
	src.changeOnRetry = true
	targetPath := filepath.Join(t.TempDir(), "changed.bin")
	dst := newLocalFileTarget(targetPath)

	err := server.runResumableTransfer("source-changed", false, "changed.bin", src, dst)
	if !errors.Is(err, errTransferSourceChanged) {
		t.Fatalf("error = %v, want errTransferSourceChanged", err)
	}
	if attempts := len(src.openedAt()); attempts != 1 {
		t.Fatalf("open attempts = %d, want 1 (no retry on changed source)", attempts)
	}
	progress, _ := server.transfers.snapshot("source-changed")
	if !progress.Done || progress.Error == "" {
		t.Fatalf("progress = %+v, want a completed transfer with an error", progress)
	}
}

func TestResumableTransferManualRetryRestartsWhenSourceChanged(t *testing.T) {
	withFastTransferRetries(t)
	server := newTestTransferServer()
	first := bytes.Repeat([]byte("a"), 2048)
	targetPath := filepath.Join(t.TempDir(), "restart.bin")
	if _, err := server.transfers.begin("source-restart", int64(len(first)), false); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := os.WriteFile(targetPath, first[:1024], 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	server.transfers.add("source-restart", 1024)
	server.transfers.setSourceIdentity("source-restart", int64(len(first)), time.Unix(1700000000, 0))
	server.transfers.finish("source-restart", errors.New("simulated interruption"))

	replacement := bytes.Repeat([]byte("b"), 1500)
	src := newFlakySource(replacement)
	dst := newLocalFileTarget(targetPath)
	if err := server.runResumableTransfer("source-restart", true, "restart.bin", src, dst); err != nil {
		t.Fatalf("runResumableTransfer: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("target was not restarted: got %d bytes", len(got))
	}
	offsets := src.openedAt()
	if len(offsets) != 1 || offsets[0] != 0 {
		t.Fatalf("open offsets = %v, want [0] after source change", offsets)
	}
}

func TestTransferProgressStoreRejectsConcurrentStart(t *testing.T) {
	store := newTransferProgressStore()
	if _, err := store.begin("job", 100, false); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := store.begin("job", 100, true); !errors.Is(err, errTransferAlreadyRunning) {
		t.Fatalf("second begin error = %v, want errTransferAlreadyRunning", err)
	}
	store.finish("job", errors.New("failed"))
	if _, err := store.begin("job", 100, true); err != nil {
		t.Fatalf("begin after finish: %v", err)
	}
}

func TestTransferProgressBeginPreservesResumeOffset(t *testing.T) {
	store := newTransferProgressStore()
	if _, err := store.begin("job", 500, false); err != nil {
		t.Fatalf("begin: %v", err)
	}
	store.add("job", 200)
	store.finish("job", errors.New("interrupted"))

	offset, err := store.begin("job", 500, true)
	if err != nil {
		t.Fatalf("resume begin: %v", err)
	}
	if offset != 200 {
		t.Fatalf("resume offset = %d, want 200", offset)
	}
	store.finish("job", errors.New("interrupted again"))
	offset, err = store.begin("job", 500, false)
	if err != nil {
		t.Fatalf("fresh begin: %v", err)
	}
	if offset != 0 {
		t.Fatalf("fresh offset = %d, want 0", offset)
	}
}

func TestTransferProgressResetClearsTerminalState(t *testing.T) {
	store := newTransferProgressStore()
	if _, err := store.begin("job", 500, false); err != nil {
		t.Fatalf("begin: %v", err)
	}
	store.add("job", 200)
	store.finish("job", errors.New("connection reset"))
	if err := store.control("job", "reset"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	progress, _ := store.snapshot("job")
	if progress.Done || progress.Cancelled || progress.Error != "" {
		t.Fatalf("terminal state survived reset: %+v", progress)
	}
	if progress.Loaded != 200 {
		t.Fatalf("reset dropped the resume offset: loaded = %d, want 200", progress.Loaded)
	}
	if _, err := store.begin("job", 500, true); err != nil {
		t.Fatalf("begin after reset: %v", err)
	}
	if err := store.control("job", "reset"); !errors.Is(err, errTransferAlreadyRunning) {
		t.Fatalf("reset while active = %v, want errTransferAlreadyRunning", err)
	}
}

type statOnlySource struct {
	calls int
	err   error
}

func (s *statOnlySource) Stat() (int64, time.Time, error) {
	s.calls++
	return 0, time.Time{}, s.err
}

func (s *statOnlySource) OpenAt(int64) (io.ReadCloser, error) {
	return nil, errors.New("unexpected open")
}

func (s *statOnlySource) Reconnect() error { return nil }

func TestDirectoryTransferErrorIsNotRetryable(t *testing.T) {
	_, _, err := newLocalFileSource(t.TempDir()).Stat()
	if !errors.Is(err, errTransferUnsupported) {
		t.Fatalf("Stat error = %v, want errTransferUnsupported", err)
	}
	if isRetryableTransferError(err) {
		t.Fatal("directory transfer error must not be retried")
	}
}

func TestUnsupportedTransferIsNotRetried(t *testing.T) {
	withFastTransferRetries(t)
	server := newTestTransferServer()
	_, _, statErr := newLocalFileSource(t.TempDir()).Stat()
	src := &statOnlySource{err: statErr}
	dst := newLocalFileTarget(filepath.Join(t.TempDir(), "target.bin"))

	err := server.runResumableTransfer("unsupported", false, "project", src, dst)
	if !errors.Is(err, errTransferUnsupported) {
		t.Fatalf("error = %v, want errTransferUnsupported", err)
	}
	if src.calls != 1 {
		t.Fatalf("Stat calls = %d, want 1 (no retries)", src.calls)
	}
}
