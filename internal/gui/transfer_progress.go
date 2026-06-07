package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

type transferProgressStore struct {
	mu    sync.RWMutex
	items map[string]*transferProgressState
}

type transferProgress struct {
	ID        string `json:"id"`
	Loaded    int64  `json:"loaded"`
	Total     int64  `json:"total"`
	Done      bool   `json:"done"`
	Paused    bool   `json:"paused"`
	Cancelled bool   `json:"cancelled"`
	Error     string `json:"error"`
}

type transferProgressState struct {
	progress transferProgress
	cond     *sync.Cond
}

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
	if item.Cancelled {
		if item.Error == "" {
			item.Error = "transfer cancelled"
		}
	} else if err != nil {
		item.Error = err.Error()
	} else if item.Total > 0 {
		item.Loaded = item.Total
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
		state.progress.Error = "transfer cancelled"
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
