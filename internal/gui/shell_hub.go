package gui

import "sync"

const (
	// shellBufferLimit caps the replay window kept for one terminal. A
	// reconnecting page can resume from its last sequence number; older output
	// is dropped instead of growing without bound.
	shellBufferLimit = 1 << 20
	// shellSubBuffer is how many chunks one websocket subscriber may queue
	// before the hub starts dropping for that subscriber only.
	shellSubBuffer = 128
)

type shellChunk struct {
	seq  uint64
	data []byte
}

type shellSub struct {
	ch   chan shellChunk
	done chan struct{}
	once sync.Once
}

func (s *shellSub) stop() {
	s.once.Do(func() {
		close(s.done)
	})
}

// shellHub owns the single reader of a shell's output. It fans chunks out to
// websocket subscribers and keeps a bounded buffer so a page that reconnects can
// replay what it missed.
type shellHub struct {
	mu      sync.Mutex
	nextSeq uint64
	chunks  []shellChunk
	bytes   int
	subs    map[*shellSub]struct{}
	closed  bool
	dropped uint64
}

func newShellHub() *shellHub {
	return &shellHub{nextSeq: 1, subs: map[*shellSub]struct{}{}}
}

func (h *shellHub) publish(data []byte) {
	if len(data) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	chunk := shellChunk{seq: h.nextSeq, data: append([]byte(nil), data...)}
	h.nextSeq++
	h.chunks = append(h.chunks, chunk)
	h.bytes += len(chunk.data)
	for h.bytes > shellBufferLimit && len(h.chunks) > 1 {
		h.bytes -= len(h.chunks[0].data)
		h.chunks = h.chunks[1:]
	}
	for sub := range h.subs {
		select {
		case sub.ch <- chunk:
		default:
			h.dropped++
		}
	}
}

// attach registers a subscriber and returns the buffered chunks after `since`.
// gap reports that the requested position fell out of the replay window.
func (h *shellHub) attach(since uint64) (sub *shellSub, replay []shellChunk, gap, closed bool) {
	sub = &shellSub{ch: make(chan shellChunk, shellSubBuffer), done: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return sub, nil, false, true
	}
	if len(h.chunks) > 0 {
		first := h.chunks[0].seq
		// since=0 means "nothing received yet"; a first sequence above 1 means
		// the buffer wrapped and the client cannot get a complete replay.
		if since+1 < first {
			gap = true
		}
		for _, chunk := range h.chunks {
			if chunk.seq > since {
				replay = append(replay, chunk)
			}
		}
	}
	h.subs[sub] = struct{}{}
	return sub, replay, gap, false
}

func (h *shellHub) detach(sub *shellSub) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.stop()
}

func (h *shellHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := h.subs
	h.subs = map[*shellSub]struct{}{}
	h.mu.Unlock()
	for sub := range subs {
		sub.stop()
	}
}
