package gui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/sshclient"
)

const (
	shellAttachTimeout = 30 * time.Second
	shellInputLimit    = 1 << 20
	shellOutputBuffer  = 32 * 1024
)

type shellManager struct {
	mu       sync.Mutex
	sessions map[string]*launcherShell
}

type launcherShell struct {
	id        string
	profileID string
	name      string
	shell     *sshclient.InteractiveShell

	mu          sync.Mutex
	attached    bool
	closed      bool
	attachTimer *time.Timer
}

type shellOpenRequest struct {
	ProfileID     string `json:"profile_id"`
	Cols          int    `json:"cols"`
	Rows          int    `json:"rows"`
	AcceptHostKey bool   `json:"accept_host_key"`
}

type shellIDRequest struct {
	ID string `json:"id"`
}

type shellResizeRequest struct {
	ID   string `json:"id"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func newShellManager() *shellManager {
	return &shellManager{sessions: map[string]*launcherShell{}}
}

func newShellID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func (m *shellManager) add(profile config.Profile, sh *sshclient.InteractiveShell) (*launcherShell, error) {
	id, err := newShellID()
	if err != nil {
		return nil, err
	}
	session := &launcherShell{
		id:        id,
		profileID: profile.ID,
		name:      profile.Name,
		shell:     sh,
	}
	session.attachTimer = time.AfterFunc(shellAttachTimeout, func() {
		if m.closeIfUnattached(id) {
			_ = sh.Close()
		}
	})
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	return session, nil
}

func (m *shellManager) get(id string) *launcherShell {
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *shellManager) closeIfUnattached(id string) bool {
	m.mu.Lock()
	session := m.sessions[id]
	if session == nil {
		m.mu.Unlock()
		return false
	}
	session.mu.Lock()
	if session.attached || session.closed {
		session.mu.Unlock()
		m.mu.Unlock()
		return false
	}
	session.closed = true
	if session.attachTimer != nil {
		session.attachTimer.Stop()
		session.attachTimer = nil
	}
	delete(m.sessions, id)
	session.mu.Unlock()
	m.mu.Unlock()
	return true
}

func (m *shellManager) close(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	session.close()
}

func (m *shellManager) closeProfile(profileID string) {
	if profileID == "" {
		return
	}
	m.mu.Lock()
	var closing []*launcherShell
	for id, session := range m.sessions {
		if session.profileID == profileID {
			closing = append(closing, session)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, session := range closing {
		session.close()
	}
}

func (m *shellManager) closeAll() {
	m.mu.Lock()
	closing := make([]*launcherShell, 0, len(m.sessions))
	for id, session := range m.sessions {
		closing = append(closing, session)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, session := range closing {
		session.close()
	}
}

func (s *launcherShell) attach() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.attached {
		return false
	}
	s.attached = true
	if s.attachTimer != nil {
		s.attachTimer.Stop()
		s.attachTimer = nil
	}
	return true
}

func (s *launcherShell) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.attachTimer != nil {
		s.attachTimer.Stop()
		s.attachTimer = nil
	}
	s.mu.Unlock()
	_ = s.shell.Close()
}

func (s *Server) handleShellOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req shellOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	profile, err := s.rt.Profiles.Get(req.ProfileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if profile.IsLocal() {
		writeError(w, http.StatusBadRequest, errors.New("local ChemSSH profiles do not provide an SSH terminal"))
		return
	}
	policy := sshclient.HostKeyStrict
	if req.AcceptHostKey {
		policy = sshclient.HostKeyAcceptNew
	}
	s.profileLog(profile.ID, "opening SSH terminal for "+profile.Name)
	shell, err := sshclient.StartInteractiveShell(profile, s.rt.Secrets, policy, req.Cols, req.Rows)
	if err != nil {
		s.profileLog(profile.ID, "SSH terminal failed for "+profile.Name+": "+err.Error())
		writeSSHError(w, err)
		return
	}
	if shell.IntegrationError != nil {
		s.profileLog(profile.ID, "SSH terminal shell integration unavailable: "+shell.IntegrationError.Error())
	}
	session, err := s.shells.add(profile, shell)
	if err != nil {
		_ = shell.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.profileLog(profile.ID, "SSH terminal opened for "+profile.Name)
	writeJSON(w, map[string]any{"id": session.id, "profile_id": profile.ID, "name": profile.Name}, nil)
}

func (s *Server) handleShellOutput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if err := s.validateLauncherAPIRequest(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	id := r.URL.Query().Get("id")
	session := s.shells.get(id)
	if session == nil {
		writeError(w, http.StatusNotFound, errors.New("SSH terminal session not found"))
		return
	}
	if !session.attach() {
		writeError(w, http.StatusConflict, errors.New("SSH terminal is already attached"))
		return
	}
	defer s.shells.close(id)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unsupported"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-r.Context().Done():
			s.shells.close(id)
		case <-done:
		}
	}()

	buffer := make([]byte, shellOutputBuffer)
	for {
		n, err := session.shell.Output.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) handleShellInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	session := s.shells.get(r.URL.Query().Get("id"))
	if session == nil {
		writeError(w, http.StatusNotFound, errors.New("SSH terminal session not found"))
		return
	}
	body := http.MaxBytesReader(w, r.Body, shellInputLimit)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(data) > 0 {
		if err := session.shell.WriteInput(data); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleShellResize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req shellResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	session := s.shells.get(req.ID)
	if session == nil {
		writeError(w, http.StatusNotFound, errors.New("SSH terminal session not found"))
		return
	}
	if err := session.shell.Resize(req.Cols, req.Rows); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleShellClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req shellIDRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.shells.close(req.ID)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}
