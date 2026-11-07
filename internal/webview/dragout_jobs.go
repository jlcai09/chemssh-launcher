package webview

import (
	"context"
	"sort"
	"sync"
	"time"

	"chemssh-launcher/internal/dragtransfer"
)

// A job survives the pointer gesture. Delivery needs an explicit system result:
// EndOperation or the Shell's completed-copy Performed DropEffect feedback.
type dragJob struct {
	ID        string                      `json:"id"`
	Session   string                      `json:"session"`
	Paths     []string                    `json:"paths"`
	State     string                      `json:"state"`
	Error     string                      `json:"error,omitempty"`
	Revision  uint64                      `json:"revision"`
	CreatedAt int64                       `json:"createdAt"`
	Loaded    int64                       `json:"loaded"`
	Total     int64                       `json:"total"`
	Paused    bool                        `json:"paused"`
	EndedAt   int64                       `json:"endedAt,omitempty"`
	Files     []dragtransfer.FileProgress `json:"files,omitempty"`
}

// Slice backing arrays must never escape the registry lock.
func cloneDragJob(job dragJob) dragJob {
	job.Paths = append([]string(nil), job.Paths...)
	job.Files = append([]dragtransfer.FileProgress(nil), job.Files...)
	return job
}

type dragJobEntry struct {
	job          dragJob
	cancel       context.CancelFunc
	wake         chan struct{}
	lastProgress time.Time
	fileIndex    map[string]int
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
	r.jobs[id] = &dragJobEntry{job: dragJob{ID: id, Session: session, Paths: append([]string(nil), paths...), State: "queued", Revision: r.revision, CreatedAt: time.Now().UnixMilli()}, cancel: cancel, wake: make(chan struct{})}
	r.trimHistoryLocked()
	ctx = dragtransfer.WithHooks(ctx, dragtransfer.Hooks{
		Wait:      func(ctx context.Context) error { return r.wait(ctx, id) },
		AddTotal:  func(n int64) { r.progress(id, n, 0) },
		AddLoaded: func(n int64) { r.progress(id, 0, n) },
		File:      func(p dragtransfer.FileProgress) { r.fileProgress(id, p) },
	})
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
	if state == "delivering" && entry.job.Paused {
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
	job := cloneDragJob(entry.job)
	terminal := dragJobTerminal(state)
	if terminal {
		entry.job.Paused = false
		entry.job.EndedAt = time.Now().UnixMilli()
		job = cloneDragJob(entry.job)
	}
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
	entry.job.Paused = false
	entry.job.EndedAt = time.Now().UnixMilli()
	job, cancel, changed := cloneDragJob(entry.job), entry.cancel, r.changed
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
		job := cloneDragJob(entry.job)
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

// Pause preserves the partially cached files and the accepted system drop.
func (r *dragJobRegistry) control(id, action string) bool {
	if action == "cancel" {
		return r.cancel(id)
	}
	if action != "pause" && action != "resume" {
		return false
	}
	r.mu.Lock()
	entry := r.jobs[id]
	if r.closed || entry == nil || (entry.job.State != "accepted" && entry.job.State != "caching") {
		r.mu.Unlock()
		return false
	}
	paused := action == "pause"
	if entry.job.Paused == paused {
		r.mu.Unlock()
		return true
	}
	entry.job.Paused = paused
	close(entry.wake)
	entry.wake = make(chan struct{})
	r.revision++
	entry.job.Revision = r.revision
	job, changed := cloneDragJob(entry.job), r.changed
	r.mu.Unlock()
	if changed != nil {
		changed(job)
	}
	return true
}

func (r *dragJobRegistry) wait(ctx context.Context, id string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		entry := r.jobs[id]
		if r.closed || entry == nil || dragJobTerminal(entry.job.State) {
			r.mu.Unlock()
			return context.Canceled
		}
		paused, wake := entry.job.Paused, entry.wake
		r.mu.Unlock()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (r *dragJobRegistry) finishCaching(ctx context.Context, id string) bool {
	for {
		if r.wait(ctx, id) != nil {
			return false
		}
		if r.update(id, "delivering", nil) {
			return true
		}
		if ctx.Err() != nil || r.isClosed() {
			return false
		}
	}
}

// fileProgress merges absolute snapshots by remote path. Byte notifications share
// the aggregate throttle; completion and failure flush the final path state.
func (r *dragJobRegistry) fileProgress(id string, p dragtransfer.FileProgress) {
	if p.Path == "" {
		return
	}
	r.mu.Lock()
	entry := r.jobs[id]
	if r.closed || entry == nil || dragJobTerminal(entry.job.State) || entry.job.State == "delivering" {
		r.mu.Unlock()
		return
	}
	if entry.fileIndex == nil {
		entry.fileIndex = make(map[string]int)
	}
	index, exists := entry.fileIndex[p.Path]
	if exists {
		if entry.job.Files[index] == p {
			r.mu.Unlock()
			return
		}
		entry.job.Files[index] = p
	} else {
		entry.fileIndex[p.Path] = len(entry.job.Files)
		entry.job.Files = append(entry.job.Files, p)
	}
	r.revision++
	entry.job.Revision = r.revision
	send := time.Since(entry.lastProgress) >= 100*time.Millisecond || p.State == "done" || p.State == "error"
	var job dragJob
	if send {
		entry.lastProgress = time.Now()
		job = cloneDragJob(entry.job)
	}
	changed := r.changed
	r.mu.Unlock()
	if send && changed != nil {
		changed(job)
	}
}

// Counters stay current for snapshots; coalesce UI events during fast local IO.
func (r *dragJobRegistry) progress(id string, total, loaded int64) {
	r.mu.Lock()
	entry := r.jobs[id]
	if r.closed || entry == nil || dragJobTerminal(entry.job.State) || entry.job.State == "delivering" {
		r.mu.Unlock()
		return
	}
	if total > 0 {
		entry.job.Total += total
	}
	if loaded > 0 {
		entry.job.Loaded += loaded
	}
	r.revision++
	entry.job.Revision = r.revision
	send := time.Since(entry.lastProgress) >= 100*time.Millisecond
	var job dragJob
	if send {
		entry.lastProgress = time.Now()
		job = cloneDragJob(entry.job)
	}
	changed := r.changed
	r.mu.Unlock()
	if send && changed != nil {
		changed(job)
	}
}
