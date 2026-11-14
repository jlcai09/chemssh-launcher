package gui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// eventsQueueSize is the per-connection outbound buffer. Terminal chunks
	// block their own session when full; progress frames are dropped instead.
	eventsQueueSize      = 512
	transferPushInterval = 150 * time.Millisecond
	eventsWriteTimeout   = 10 * time.Second
	eventsReadLimit      = 2 << 20
)

type eventsClientMessage struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`
	Since   uint64 `json:"since,omitempty"`
	Data    string `json:"data,omitempty"`
	ID      string `json:"id,omitempty"`
}

type eventsServerMessage struct {
	Type     string            `json:"type"`
	Session  string            `json:"session,omitempty"`
	Seq      uint64            `json:"seq,omitempty"`
	Data     string            `json:"data,omitempty"`
	ID       string            `json:"id,omitempty"`
	Progress *transferProgress `json:"progress,omitempty"`
}

type eventsTerminal struct {
	session *launcherShell
	sub     *shellSub
	stop    chan struct{}
	once    sync.Once
}

func (t *eventsTerminal) close() {
	t.once.Do(func() { close(t.stop) })
}

type eventsClient struct {
	server    *Server
	send      chan eventsServerMessage
	closed    chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	terminals map[string]*eventsTerminal
	watched   map[string]transferProgress
}

// validateEventsRequest authenticates the websocket handshake. Browsers cannot
// set custom headers on a websocket, so the cookie token plus the same-origin
// checks are the proof; the header token is not available here.
func (s *Server) validateEventsRequest(r *http.Request) error {
	if err := validateLauncherAPIOrigin(r); err != nil {
		return err
	}
	if s.apiToken == "" {
		return errors.New("launcher API token is not configured")
	}
	cookie, err := r.Cookie(launcherAPITokenCookie)
	if err != nil {
		return errors.New("missing launcher API token")
	}
	if !constantTimeTokenEqual(cookie.Value, s.apiToken) {
		return errors.New("invalid launcher API token")
	}
	return nil
}

// handleEventsWS is the single multiplexed channel used by the launcher pages:
// terminal output, terminal input and transfer progress share it so open
// terminals and tabs never consume the browser's HTTP connection budget.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	if err := s.validateEventsRequest(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(eventsReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	client := &eventsClient{
		server:    s,
		send:      make(chan eventsServerMessage, eventsQueueSize),
		closed:    make(chan struct{}),
		terminals: map[string]*eventsTerminal{},
		watched:   map[string]transferProgress{},
	}
	go client.writeLoop(ctx, conn)
	go client.watchLoop(ctx)
	client.readLoop(ctx, conn)
	client.close()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (c *eventsClient) writeLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case msg := <-c.send:
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, eventsWriteTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.close()
				return
			}
		case <-c.closed:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *eventsClient) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg eventsClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "terminal:attach":
			c.attachTerminal(ctx, msg)
		case "terminal:detach":
			c.detachTerminal(msg.Session)
		case "terminal:input":
			c.inputTerminal(msg)
		case "transfer:watch":
			c.watchTransfer(msg.ID)
		case "transfer:unwatch":
			c.unwatchTransfer(msg.ID)
		}
	}
}

func (c *eventsClient) attachTerminal(ctx context.Context, msg eventsClientMessage) {
	session := c.server.shells.get(msg.Session)
	if session == nil {
		c.enqueue(eventsServerMessage{Type: "terminal:exit", Session: msg.Session})
		return
	}
	session.markAttached()
	sub, replay, gap, closed := session.hub.attach(msg.Since)
	if closed {
		c.enqueue(eventsServerMessage{Type: "terminal:exit", Session: msg.Session})
		return
	}

	c.mu.Lock()
	previous := c.terminals[msg.Session]
	entry := &eventsTerminal{session: session, sub: sub, stop: make(chan struct{})}
	c.terminals[msg.Session] = entry
	c.mu.Unlock()
	if previous != nil {
		previous.close()
		previous.session.hub.detach(previous.sub)
	}

	if gap {
		c.enqueue(eventsServerMessage{Type: "terminal:gap", Session: msg.Session})
	}
	for _, chunk := range replay {
		ok := c.enqueue(eventsServerMessage{
			Type:    "terminal:data",
			Session: msg.Session,
			Seq:     chunk.seq,
			Data:    base64.StdEncoding.EncodeToString(chunk.data),
		})
		if !ok {
			return
		}
	}
	go c.pumpTerminal(ctx, msg.Session, entry)
}

func (c *eventsClient) pumpTerminal(ctx context.Context, sessionID string, entry *eventsTerminal) {
	defer entry.close()
	for {
		select {
		case chunk := <-entry.sub.ch:
			msg := eventsServerMessage{
				Type:    "terminal:data",
				Session: sessionID,
				Seq:     chunk.seq,
				Data:    base64.StdEncoding.EncodeToString(chunk.data),
			}
			select {
			case c.send <- msg:
			case <-entry.stop:
				return
			case <-c.closed:
				return
			case <-ctx.Done():
				return
			}
		case <-entry.sub.done:
			// The shell ended (remote exit or an explicit close); tell the page
			// so the tab can show the disconnect state.
			c.enqueueLossy(eventsServerMessage{Type: "terminal:exit", Session: sessionID})
			return
		case <-entry.stop:
			return
		case <-c.closed:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *eventsClient) detachTerminal(sessionID string) {
	c.mu.Lock()
	entry := c.terminals[sessionID]
	delete(c.terminals, sessionID)
	c.mu.Unlock()
	if entry == nil {
		return
	}
	entry.close()
	entry.session.hub.detach(entry.sub)
}

func (c *eventsClient) inputTerminal(msg eventsClientMessage) {
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil || len(data) == 0 {
		return
	}
	session := c.server.shells.get(msg.Session)
	if session == nil {
		c.enqueueLossy(eventsServerMessage{Type: "terminal:exit", Session: msg.Session})
		return
	}
	if err := session.shell.WriteInput(data); err != nil {
		c.enqueueLossy(eventsServerMessage{Type: "terminal:exit", Session: msg.Session})
	}
}

func (c *eventsClient) watchTransfer(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	if _, ok := c.watched[id]; !ok {
		c.watched[id] = transferProgress{}
	}
	c.mu.Unlock()
	c.pushTransfer(id, true)
}

func (c *eventsClient) unwatchTransfer(id string) {
	c.mu.Lock()
	delete(c.watched, id)
	c.mu.Unlock()
}

func (c *eventsClient) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(transferPushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			ids := make([]string, 0, len(c.watched))
			for id := range c.watched {
				ids = append(ids, id)
			}
			c.mu.Unlock()
			for _, id := range ids {
				c.pushTransfer(id, false)
			}
		case <-c.closed:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *eventsClient) pushTransfer(id string, force bool) {
	progress, ok := c.server.transfers.snapshot(id)
	if !ok {
		progress = transferProgress{ID: id}
	}
	c.mu.Lock()
	previous, watched := c.watched[id]
	if !watched {
		c.mu.Unlock()
		return
	}
	if !force && progress == previous {
		c.mu.Unlock()
		return
	}
	c.watched[id] = progress
	c.mu.Unlock()
	c.enqueueLossy(eventsServerMessage{Type: "transfer:progress", ID: id, Progress: &progress})
}

func (c *eventsClient) enqueue(msg eventsServerMessage) bool {
	select {
	case c.send <- msg:
		return true
	case <-c.closed:
		return false
	}
}

// enqueueLossy is for frames that may be regenerated on the next tick
// (progress) or are advisory (exit); it never stalls a shared ticker.
func (c *eventsClient) enqueueLossy(msg eventsServerMessage) {
	select {
	case c.send <- msg:
	case <-c.closed:
	default:
	}
}

func (c *eventsClient) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		terminals := c.terminals
		c.terminals = map[string]*eventsTerminal{}
		c.mu.Unlock()
		for _, entry := range terminals {
			entry.close()
			entry.session.hub.detach(entry.sub)
		}
	})
}
