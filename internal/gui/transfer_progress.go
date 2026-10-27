package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type transferProgressStore struct {
	mu    sync.RWMutex
	items map[string]*transferProgressState
}

type transferProgress struct {
	ID          string    `json:"id"`
	Loaded      int64     `json:"loaded"`
	Total       int64     `json:"total"`
	Done        bool      `json:"done"`
	Paused      bool      `json:"paused"`
	Cancelled   bool      `json:"cancelled"`
	Error       string    `json:"error"`
	Retrying    bool      `json:"retrying,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	MaxAttempts int       `json:"max_attempts,omitempty"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
}

type transferProgressState struct {
	progress      transferProgress
	cond          *sync.Cond
	active        bool
	sourceSize    int64
	sourceModTime time.Time
	hasSource     bool
}

var errTransferAlreadyRunning = errors.New("transfer is already running")

func newTransferProgressStore() *transferProgressStore {
	return &transferProgressStore{items: map[string]*transferProgressState{}}
}

func (s *transferProgressStore) start(id string, total int64) {
	if id == "" {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	existing := state.progress
	state.active = true
	next := transferProgress{
		ID:        id,
		Total:     total,
		Paused:    existing.Paused,
		Cancelled: existing.Cancelled,
	}
	if existing.Cancelled {
		next.Done = true
		next.Error = existing.Error
		if next.Error == "" {
			next.Error = "transfer cancelled"
		}
	}
	state.progress = next
	state.cond.Broadcast()
	s.mu.Unlock()
}

// begin prepares a resumable transfer and returns the byte offset that is
// already present at the destination. With resume set, the previous byte count
// is preserved so a failed or interrupted transfer continues instead of
// restarting. A cancelled transfer never resumes.
func (s *transferProgressStore) begin(id string, total int64, resume bool) (int64, error) {
	if id == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.stateLocked(id)
	if state.active {
		return 0, errTransferAlreadyRunning
	}
	state.active = true
	offset := int64(0)
	if resume && !state.progress.Cancelled {
		offset = state.progress.Loaded
	}
	if total > 0 && offset > total {
		offset = total
	}
	if offset < 0 {
		offset = 0
	}
	state.progress = transferProgress{ID: id, Total: total, Loaded: offset}
	state.cond.Broadcast()
	return offset, nil
}

func (s *transferProgressStore) setTotal(id string, total int64) {
	if id == "" || total <= 0 {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	if state.progress.ID == "" {
		state.progress.ID = id
	}
	state.progress.Total = total
	if state.progress.Loaded > total {
		state.progress.Loaded = total
	}
	s.mu.Unlock()
}

func (s *transferProgressStore) setLoaded(id string, loaded int64) {
	if id == "" {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	state.progress.ID = id
	if loaded < 0 {
		loaded = 0
	}
	state.progress.Loaded = loaded
	if state.progress.Total < loaded {
		state.progress.Total = loaded
	}
	s.mu.Unlock()
}

func (s *transferProgressStore) loaded(id string) int64 {
	if id == "" {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.items[id]
	if state == nil {
		return 0
	}
	return state.progress.Loaded
}

func (s *transferProgressStore) sourceIdentity(id string) (int64, time.Time, bool) {
	if id == "" {
		return 0, time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.items[id]
	if state == nil || !state.hasSource {
		return 0, time.Time{}, false
	}
	return state.sourceSize, state.sourceModTime, true
}

func (s *transferProgressStore) setSourceIdentity(id string, size int64, modTime time.Time) {
	if id == "" {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	state.sourceSize = size
	state.sourceModTime = modTime
	state.hasSource = true
	s.mu.Unlock()
}

func (s *transferProgressStore) retryScheduled(id string, attempt, maxAttempts int, next time.Time) {
	if id == "" {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	state.progress.ID = id
	state.progress.Retrying = true
	state.progress.Attempt = attempt
	state.progress.MaxAttempts = maxAttempts
	state.progress.NextRetryAt = next
	s.mu.Unlock()
}

func (s *transferProgressStore) add(id string, n int) {
	if id == "" || n <= 0 {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	state.progress.ID = id
	state.progress.Loaded += int64(n)
	if state.progress.Total < state.progress.Loaded {
		state.progress.Total = state.progress.Loaded
	}
	state.progress.Retrying = false
	state.progress.NextRetryAt = time.Time{}
	s.mu.Unlock()
}

func (s *transferProgressStore) finish(id string, err error) {
	if id == "" {
		return
	}
	s.mu.Lock()
	state := s.stateLocked(id)
	item := state.progress
	item.ID = id
	item.Done = true
	item.Paused = false
	item.Retrying = false
	item.NextRetryAt = time.Time{}
	state.active = false
	if item.Cancelled {
		if item.Error == "" {
			item.Error = "transfer cancelled"
		}
	} else if err != nil {
		item.Error = err.Error()
	} else {
		item.Error = ""
		if item.Total > 0 {
			item.Loaded = item.Total
		}
	}
	state.progress = item
	state.cond.Broadcast()
	s.mu.Unlock()
}

func (s *transferProgressStore) snapshot(id string) (transferProgress, bool) {
	s.mu.RLock()
	state, ok := s.items[id]
	var item transferProgress
	if ok {
		item = state.progress
	}
	s.mu.RUnlock()
	return item, ok
}

func (s *transferProgressStore) control(id, action string) error {
	if id == "" {
		return errors.New("missing transfer id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.stateLocked(id)
	switch action {
	case "pause":
		if !state.progress.Done && !state.progress.Cancelled {
			state.progress.Paused = true
		}
	case "resume":
		state.progress.Paused = false
		state.cond.Broadcast()
	case "cancel":
		state.progress.Cancelled = true
		state.progress.Paused = false
		state.progress.Done = true
		state.progress.Retrying = false
		state.progress.NextRetryAt = time.Time{}
		state.progress.Error = "transfer cancelled"
		state.cond.Broadcast()
	case "reset":
		// A manual retry must clear the previous attempt's terminal state first:
		// the UI polls this progress before re-issuing the transfer, and a stale
		// done+error would make it abort before the new request ever starts.
		if state.active {
			return errTransferAlreadyRunning
		}
		state.progress.Done = false
		state.progress.Cancelled = false
		state.progress.Paused = false
		state.progress.Retrying = false
		state.progress.Attempt = 0
		state.progress.NextRetryAt = time.Time{}
		state.progress.Error = ""
		state.cond.Broadcast()
	default:
		return fmt.Errorf("unknown transfer action: %s", action)
	}
	return nil
}

func (s *transferProgressStore) waitAllowed(id string) error {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.stateLocked(id)
	for state.progress.Paused && !state.progress.Cancelled && !state.progress.Done {
		state.cond.Wait()
	}
	if state.progress.Cancelled {
		return errors.New("transfer cancelled")
	}
	return nil
}

func (s *transferProgressStore) stateLocked(id string) *transferProgressState {
	state := s.items[id]
	if state != nil {
		return state
	}
	state = &transferProgressState{progress: transferProgress{ID: id}}
	state.cond = sync.NewCond(&s.mu)
	s.items[id] = state
	return state
}

type progressReader struct {
	id    string
	store *transferProgressStore
	src   io.Reader
}

func (r progressReader) Read(p []byte) (int, error) {
	if err := r.store.waitAllowed(r.id); err != nil {
		return 0, err
	}
	n, err := r.src.Read(p)
	r.store.add(r.id, n)
	return n, err
}

func (s *Server) handleTransferProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing transfer id"))
		return
	}
	item, ok := s.transfers.snapshot(id)
	if !ok {
		item = transferProgress{ID: id}
	}
	writeJSON(w, item, nil)
}

func (s *Server) handleTransferControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.transfers.control(req.ID, req.Action); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) progressReader(id string, total int64, src io.Reader) io.Reader {
	if id == "" {
		return src
	}
	s.transfers.start(id, total)
	return progressReader{id: id, store: s.transfers, src: src}
}

func (s *Server) finishProgress(id string, err error) {
	s.transfers.finish(id, err)
}

// handleTransferCount receives the current running transfer count from the frontend.
// The WebView2 close interceptor reads this value to decide whether to prompt the user.
func (s *Server) handleTransferCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Count int32 `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Count < 0 {
		req.Count = 0
	}
	s.activeTransferCount.Store(req.Count)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}
