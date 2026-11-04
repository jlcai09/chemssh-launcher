package webview

import (
	"context"
	"sort"
	"sync"
	"time"
)

// A job survives the pointer gesture. Delivery needs an explicit system result:
// EndOperation or the Shell's completed-copy Performed DropEffect feedback.
type dragJob struct {
	ID        string   `json:"id"`
	Session   string   `json:"session"`
	Paths     []string `json:"paths"`
	State     string   `json:"state"`
	Error     string   `json:"error,omitempty"`
	Revision  uint64   `json:"revision"`
	CreatedAt int64    `json:"createdAt"`
}

type dragJobEntry struct {
	job    dragJob
	cancel context.CancelFunc
}
type dragJobRegistry struct {
	mu       sync.Mutex
	jobs     map[string]*dragJobEntry
	closed   bool
	revision uint64
	changed  func(dragJob)
}

func newDragJobRegistry(changed func(dragJob)) *dragJobRegistry {
	return &dragJobRegistry{jobs: make(map[string]*dragJobEntry), changed: changed}
}

func dragJobTerminal(state string) bool {
	return state == "done" || state == "error" || state == "cancelled"
}

func (r *dragJobRegistry) add(id, session string, paths []string) (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.jobs[id] != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.revision++
	r.jobs[id] = &dragJobEntry{job: dragJob{ID: id, Session: session, Paths: append([]string(nil), paths...), State: "queued", Revision: r.revision, CreatedAt: time.Now().UnixMilli()}, cancel: cancel}
	r.trimHistoryLocked()
	return ctx, true
}

// Bound completed history without discarding any active job. Caller holds mu.
func (r *dragJobRegistry) trimHistoryLocked() {
	for len(r.jobs) > 50 {
		var oldest *dragJobEntry
		for _, entry := range r.jobs {
			if dragJobTerminal(entry.job.State) && (oldest == nil || entry.job.Revision < oldest.job.Revision) {
				oldest = entry
			}
		}
		if oldest == nil {
			return
		}
		delete(r.jobs, oldest.job.ID)
	}
}

func (r *dragJobRegistry) update(id, state string, err error) bool {
	r.mu.Lock()
	entry := r.jobs[id]
	if r.closed || entry == nil || dragJobTerminal(entry.job.State) {
		r.mu.Unlock()
		return false
	}
	// A late StartOperation notification cannot regress a running copy.
	if state == "accepted" && entry.job.State != "queued" {
		r.mu.Unlock()
		return false
	}
	if state == "caching" && entry.job.State == "delivering" {
		r.mu.Unlock()
		return false
	}
	r.revision++
	entry.job.State, entry.job.Revision = state, r.revision
	if err != nil {
		entry.job.Error = err.Error()
	}
	job := entry.job
	terminal := dragJobTerminal(state)
	cancel := entry.cancel
	changed := r.changed
	r.trimHistoryLocked()
	r.mu.Unlock()
	if terminal {
		cancel()
	}
	if changed != nil {
		changed(job)
	}
	return true
}

// Once paths were handed to the Shell, its own copy operation owns delivery.
func (r *dragJobRegistry) cancel(id string) bool {
	r.mu.Lock()
	entry := r.jobs[id]
	allowed := !r.closed && entry != nil && (entry.job.State == "queued" || entry.job.State == "accepted" || entry.job.State == "caching")
	if !allowed {
		r.mu.Unlock()
		return false
	}
	r.revision++
	entry.job.State, entry.job.Revision = "cancelled", r.revision
	job, cancel, changed := entry.job, entry.cancel, r.changed
	r.trimHistoryLocked()
	r.mu.Unlock()
	cancel()
	if changed != nil {
		changed(job)
	}
	return true
}

func (r *dragJobRegistry) snapshot() []dragJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs := make([]dragJob, 0, len(r.jobs))
	for _, entry := range r.jobs {
		job := entry.job
		job.Paths = append([]string(nil), job.Paths...)
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt < jobs[j].CreatedAt || jobs[i].CreatedAt == jobs[j].CreatedAt && jobs[i].ID < jobs[j].ID
	})
	return jobs
}

func (r *dragJobRegistry) hasActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	for _, entry := range r.jobs {
		if !dragJobTerminal(entry.job.State) {
			return true
		}
	}
	return false
}

func (r *dragJobRegistry) isClosed() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.closed }
func (r *dragJobRegistry) close() {
	r.mu.Lock()
	r.closed = true
	for _, entry := range r.jobs {
		entry.cancel()
	}
	r.mu.Unlock()
}
