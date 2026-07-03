package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	posixpath "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/chemssh"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/sshclient"
)

type chemSSHBridgeCapabilities struct {
	Enabled       bool                        `json:"enabled"`
	Version       int                         `json:"version"`
	Launcher      string                      `json:"launcher"`
	SessionID     string                      `json:"session_id,omitempty"`
	WorkspaceRoot string                      `json:"workspace_root,omitempty"`
	Features      chemSSHBridgeFeatures       `json:"features"`
	Endpoints     chemSSHBridgeEndpointConfig `json:"endpoints"`
}

type chemSSHBridgeFeatures struct {
	SystemIcons    bool `json:"system_icons"`
	OpenDefault    bool `json:"open_default"`
	OpenText       bool `json:"open_text"`
	OpenSyncEvents bool `json:"open_sync_events"`
}

type chemSSHBridgeEndpointConfig struct {
	Icon       string `json:"icon,omitempty"`
	Open       string `json:"open,omitempty"`
	OpenText   string `json:"open_text,omitempty"`
	SyncEvents string `json:"sync_events,omitempty"`
}

type chemSSHBridgeOpenRequest struct {
	Path string `json:"path"`
}

type chemSSHBridgeOpenResponse struct {
	OK         bool   `json:"ok"`
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
}

type chemSSHBridgeSyncEvent struct {
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"time"`
	RemotePath string    `json:"remote_path"`
	LocalPath  string    `json:"local_path"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

type chemSSHBridgeHTTPError struct {
	status int
	err    error
}

func (e *chemSSHBridgeHTTPError) Error() string {
	return e.err.Error()
}

func (e *chemSSHBridgeHTTPError) Unwrap() error {
	return e.err
}

func (s *Server) handleChemSSHBridgeCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	profile, workspaceRoot, err := s.chemSSHBridgeContext(r, false)
	if err != nil {
		writeJSON(w, newChemSSHBridgeCapabilities(false, false, false, "", "", false), nil)
		return
	}
	writeJSON(w, newChemSSHBridgeCapabilities(true, true, !profile.IsLocal(), profile.ID, workspaceRoot, profile.IsLocal()), nil)
}

func (s *Server) handleChemSSHBridgeClientIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	identity, err := s.ensureLauncherClientIdentity()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, chemSSHBridgeClientIdentity{
		Enabled:  true,
		Version:  identity.Version,
		ClientID: identity.ClientID,
		Source:   "launcher",
	}, nil)
}

func (s *Server) handleChemSSHBridgeOpen(w http.ResponseWriter, r *http.Request) {
	s.handleChemSSHBridgeOpenMode(w, r, false)
}

func (s *Server) handleChemSSHBridgeOpenText(w http.ResponseWriter, r *http.Request) {
	s.handleChemSSHBridgeOpenMode(w, r, true)
}

func (s *Server) handleChemSSHBridgeOpenMode(w http.ResponseWriter, r *http.Request, textMode bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req chemSSHBridgeOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	localPath, err := s.openRemoteFileWithLocalApp(r, req.Path, textMode)
	if err != nil {
		writeChemSSHBridgeError(w, err)
		return
	}
	writeJSON(w, chemSSHBridgeOpenResponse{
		OK:         true,
		RemotePath: cleanRemotePath(req.Path),
		LocalPath:  localPath,
	}, nil)
}

func (s *Server) handleChemSSHBridgeOpenSyncEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	s.mu.Lock()
	sessionID := s.bridge.sftpSessionID
	s.mu.Unlock()

	events := []chemSSHBridgeSyncEvent{}
	if sessionID != "" {
		for _, event := range s.sftpSync.EventsSince(after) {
			if event.Session != sessionID {
				continue
			}
			events = append(events, chemSSHBridgeSyncEvent{
				Seq:        event.Seq,
				Time:       event.Time,
				RemotePath: event.RemotePath,
				LocalPath:  event.LocalPath,
				Status:     event.Status,
				Error:      event.Error,
			})
		}
	}
	writeJSON(w, map[string][]chemSSHBridgeSyncEvent{"events": events}, nil)
}

func newChemSSHBridgeCapabilities(enabled, openAvailable, syncAvailable bool, sessionID, workspaceRoot string, local bool) chemSSHBridgeCapabilities {
	features := chemSSHBridgeFeatures{
		SystemIcons:    enabled,
		OpenDefault:    enabled && openAvailable,
		OpenText:       enabled && openAvailable,
		OpenSyncEvents: enabled && syncAvailable,
	}
	endpoints := chemSSHBridgeEndpointConfig{}
	if enabled {
		endpoints.Icon = "/api/file-icon"
		if openAvailable {
			endpoints.Open = "/api/chemssh-bridge/open"
			endpoints.OpenText = "/api/chemssh-bridge/open-text"
		}
		if syncAvailable {
			endpoints.SyncEvents = "/api/chemssh-bridge/open-sync-events"
		}
	}
	cleanedWorkspaceRoot := cleanRemotePath(workspaceRoot)
	if local {
		cleanedWorkspaceRoot = cleanLocalPath(workspaceRoot)
	}
	return chemSSHBridgeCapabilities{
		Enabled:       enabled,
		Version:       1,
		Launcher:      "chemssh-launcher",
		SessionID:     sessionID,
		WorkspaceRoot: cleanedWorkspaceRoot,
		Features:      features,
		Endpoints:     endpoints,
	}
}

func (s *Server) openRemoteFileWithLocalApp(r *http.Request, remotePath string, textMode bool) (string, error) {
	profile, workspaceRoot, err := s.chemSSHBridgeContext(r, true)
	if err != nil {
		return "", err
	}
	if profile.IsLocal() {
		localPath, err := validateChemSSHBridgeLocalPath(remotePath, workspaceRoot)
		if err != nil {
			return "", err
		}
		if textMode {
			if err := browser.OpenTextFile(localPath); err != nil {
				return "", err
			}
			s.profileLog(profile.ID, "ChemSSH bridge open text local file: "+localPath)
		} else {
			if err := browser.OpenFile(localPath); err != nil {
				return "", err
			}
			s.profileLog(profile.ID, "ChemSSH bridge open local file: "+localPath)
		}
		return localPath, nil
	}
	if err := validateChemSSHBridgeRemotePath(remotePath, workspaceRoot); err != nil {
		return "", err
	}
	cleanedRemotePath := cleanRemotePath(remotePath)
	sessionID, workspaceRoot, err := s.ensureChemSSHBridgeSFTPSession(r)
	if err != nil {
		return "", err
	}
	if err := validateChemSSHBridgeRemotePath(cleanedRemotePath, workspaceRoot); err != nil {
		return "", err
	}
	cachePath, err := s.cacheSFTPFile(sessionID, cleanedRemotePath)
	if err != nil {
		return "", err
	}
	if err := s.sftpSync.Register(sessionID, cleanedRemotePath, cachePath); err != nil {
		return "", err
	}
	if textMode {
		if err := browser.OpenTextFile(cachePath); err != nil {
			return "", err
		}
		s.profileLog(profile.ID, "ChemSSH bridge open text cached file: "+cleanedRemotePath+" -> "+cachePath)
	} else {
		if err := browser.OpenFile(cachePath); err != nil {
			return "", err
		}
		s.profileLog(profile.ID, "ChemSSH bridge open cached file: "+cleanedRemotePath+" -> "+cachePath)
	}
	return cachePath, nil
}

func (s *Server) ensureChemSSHBridgeSFTPSession(r *http.Request) (string, string, error) {
	profile, workspaceRoot, err := s.chemSSHBridgeContext(r, true)
	if err != nil {
		return "", "", err
	}
	if profile.IsLocal() {
		return "", "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "local ChemSSH profiles do not provide SFTP bridge open")
	}
	return s.ensureChemSSHBridgeSFTPSessionForProfile(profile, workspaceRoot)
}

func (s *Server) ensureChemSSHBridgeSFTPSessionForProfile(profile config.Profile, workspaceRoot string) (string, string, error) {
	workspaceRoot = cleanRemotePath(workspaceRoot)
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "ChemSSH identity did not include workspace_root")
	}
	for {
		connect, stateToClear, wait := s.reserveChemSSHBridgeSFTPConnect(profile.ID, workspaceRoot)
		if wait {
			<-connect.done
			if connect.err != nil {
				return "", "", connect.err
			}
			return connect.sessionID, connect.workspaceRoot, nil
		}
		if connect == nil {
			return stateToClear.sftpSessionID, workspaceRoot, nil
		}
		s.clearChemSSHBridgeState(stateToClear)
		sessionID, err := s.connectChemSSHBridgeSFTP(connect, profile, workspaceRoot)
		if err != nil {
			return "", "", err
		}
		return sessionID, workspaceRoot, nil
	}
}

func (s *Server) reserveChemSSHBridgeSFTPConnect(profileID, workspaceRoot string) (*chemSSHBridgeConnect, chemSSHBridgeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.bridge
	if current.sftpSessionID != "" && current.profileID == profileID && current.workspaceRoot == workspaceRoot {
		if s.sftp.IsConnected(current.sftpSessionID) {
			return nil, current, false
		}
		connect := &chemSSHBridgeConnect{profileID: profileID, workspaceRoot: workspaceRoot, done: make(chan struct{})}
		s.bridge = chemSSHBridgeState{
			profileID:     profileID,
			workspaceRoot: workspaceRoot,
			connecting:    connect,
		}
		return connect, current, false
	}
	if current.connecting != nil && current.connecting.profileID == profileID && current.connecting.workspaceRoot == workspaceRoot {
		return current.connecting, chemSSHBridgeState{}, true
	}
	if current.sftpSessionID != "" || current.connecting != nil {
		s.bridge = chemSSHBridgeState{}
	}
	connect := &chemSSHBridgeConnect{profileID: profileID, workspaceRoot: workspaceRoot, done: make(chan struct{})}
	s.bridge = chemSSHBridgeState{
		profileID:     profileID,
		workspaceRoot: workspaceRoot,
		connecting:    connect,
	}
	return connect, current, false
}

func (s *Server) connectChemSSHBridgeSFTP(connect *chemSSHBridgeConnect, profile config.Profile, workspaceRoot string) (string, error) {
	session, err := s.sftp.ConnectDedicated(profile, s.rt.Secrets, sshclient.HostKeyStrict)
	if err != nil {
		s.finishChemSSHBridgeSFTPConnect(connect, "", err)
		return "", err
	}

	s.mu.Lock()
	active := s.sessionForProfileLocked(profile.ID)
	if active == nil || !active.forwarding || active.profile.ID != profile.ID {
		s.mu.Unlock()
		_ = s.sftp.Disconnect(session.ID)
		err := newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "active ChemSSH session changed while opening bridge SFTP session")
		s.finishChemSSHBridgeSFTPConnect(connect, "", err)
		return "", err
	}
	s.mu.Unlock()

	if err := s.finishChemSSHBridgeSFTPConnect(connect, session.ID, nil); err != nil {
		_ = s.sftp.Disconnect(session.ID)
		return "", err
	}
	s.profileLog(profile.ID, "ChemSSH bridge SFTP connection OK for "+profile.Name)
	return session.ID, nil
}

func (s *Server) finishChemSSHBridgeSFTPConnect(connect *chemSSHBridgeConnect, sessionID string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.bridge.connecting != connect {
		if err == nil {
			err = newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "active ChemSSH bridge changed while opening SFTP session")
		}
		connect.err = err
		close(connect.done)
		return err
	}
	if err != nil {
		s.bridge = chemSSHBridgeState{}
		connect.err = err
		close(connect.done)
		return err
	}
	s.bridge = chemSSHBridgeState{
		sftpSessionID: sessionID,
		profileID:     connect.profileID,
		workspaceRoot: connect.workspaceRoot,
	}
	connect.sessionID = sessionID
	close(connect.done)
	return nil
}

func (s *Server) warmChemSSHBridgeSFTP(session *activeSession, profile config.Profile, workspaceRoot string) {
	if profile.IsLocal() || strings.TrimSpace(workspaceRoot) == "" {
		return
	}
	time.Sleep(chemsshBridgeSFTPWarmDelay)
	s.mu.Lock()
	active := s.sessionStillActiveLocked(session) && session.forwarding
	s.mu.Unlock()
	if !active {
		return
	}
	if _, _, err := s.ensureChemSSHBridgeSFTPSessionForProfile(profile, workspaceRoot); err != nil {
		s.profileLog(profile.ID, "warning: ChemSSH bridge SFTP preconnect failed: "+err.Error())
		return
	}
	s.profileLog(profile.ID, "ChemSSH bridge SFTP preconnected for "+profile.Name)
}

func (s *Server) chemSSHBridgeContext(r *http.Request, requireWorkspace bool) (config.Profile, string, error) {
	s.mu.Lock()
	session := s.sessionForProxyRequestLocked(r)
	if session == nil || !session.forwarding {
		s.mu.Unlock()
		return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "chemssh service is not available; start forwarding first")
	}
	profile := session.profile
	workspaceRoot := cleanChemSSHBridgeWorkspace(session.profile, session.workspaceRoot)
	client := session.client
	s.mu.Unlock()

	if workspaceRoot != "" {
		return profile, workspaceRoot, nil
	}
	if !requireWorkspace && !profile.IsLocal() {
		return profile, workspaceRoot, nil
	}
	if profile.IsLocal() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		identity, err := chemssh.FetchLocalIdentity(ctx, profile, s.rt.Secrets)
		if err != nil {
			s.profileLog(profile.ID, "warning: could not read local ChemSSH identity for bridge: "+err.Error())
			if !requireWorkspace {
				return profile, "", nil
			}
			return config.Profile{}, "", err
		}
		workspaceRoot = cleanLocalPath(identity.WorkspaceRoot)
		if workspaceRoot == "" {
			if !requireWorkspace {
				return profile, "", nil
			}
			return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "local ChemSSH identity did not include workspace_root")
		}
		s.mu.Lock()
		if !s.sessionStillActiveLocked(session) || !session.forwarding {
			s.mu.Unlock()
			return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "active ChemSSH session changed while reading identity")
		}
		session.remotePID = identity.PID
		session.workspaceRoot = workspaceRoot
		s.mu.Unlock()
		s.profileLog(profile.ID, "ChemSSH bridge local identity OK: workspace "+workspaceRoot)
		return profile, workspaceRoot, nil
	}
	if client == nil {
		return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "active ChemSSH session has no SSH client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	identity, err := chemssh.FetchIdentity(ctx, client, profile, s.rt.Secrets)
	if err != nil {
		s.profileLog(profile.ID, "warning: could not read ChemSSH identity for bridge: "+err.Error())
		return config.Profile{}, "", err
	}
	workspaceRoot = cleanRemotePath(identity.WorkspaceRoot)
	if workspaceRoot == "" {
		return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "remote ChemSSH identity did not include workspace_root")
	}

	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) || !session.forwarding {
		s.mu.Unlock()
		return config.Profile{}, "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "active ChemSSH session changed while reading identity")
	}
	session.remotePID = identity.PID
	session.workspaceRoot = workspaceRoot
	s.mu.Unlock()

	s.profileLog(profile.ID, "ChemSSH bridge identity OK: workspace "+workspaceRoot)
	return profile, workspaceRoot, nil
}

func newChemSSHBridgeHTTPError(status int, message string) error {
	return &chemSSHBridgeHTTPError{status: status, err: errors.New(message)}
}

func writeChemSSHBridgeError(w http.ResponseWriter, err error) {
	var statusErr *chemSSHBridgeHTTPError
	if errors.As(err, &statusErr) {
		writeError(w, statusErr.status, statusErr.err)
		return
	}
	writeSFTPError(w, err)
}

func (s *Server) closeChemSSHBridgeSession() {
	s.mu.Lock()
	state := s.bridge
	s.bridge = chemSSHBridgeState{}
	s.mu.Unlock()
	s.clearChemSSHBridgeState(state)
}

func (s *Server) closeChemSSHBridgeSessionForProfile(profileID string) {
	s.mu.Lock()
	state := s.bridge
	if state.profileID == "" || state.profileID != profileID {
		s.mu.Unlock()
		return
	}
	s.bridge = chemSSHBridgeState{}
	s.mu.Unlock()
	s.clearChemSSHBridgeState(state)
}

func (s *Server) clearChemSSHBridgeState(state chemSSHBridgeState) {
	if state.sftpSessionID == "" {
		return
	}
	s.sftpSync.UnregisterSession(state.sftpSessionID)
	_ = s.sftp.Disconnect(state.sftpSessionID)
	s.profileLog(state.profileID, "ChemSSH bridge SFTP disconnected")
}

func cleanChemSSHBridgeWorkspace(profile config.Profile, value string) string {
	if profile.IsLocal() {
		return cleanLocalPath(value)
	}
	return cleanRemotePath(value)
}

func validateChemSSHBridgeRemotePath(remotePath, workspaceRoot string) error {
	remotePath = cleanRemotePath(remotePath)
	workspaceRoot = cleanRemotePath(workspaceRoot)
	if remotePath == "" {
		return newChemSSHBridgeHTTPError(http.StatusBadRequest, "missing remote path")
	}
	if workspaceRoot == "" {
		return newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "ChemSSH workspace root is not available")
	}
	if !strings.HasPrefix(remotePath, "/") {
		return newChemSSHBridgeHTTPError(http.StatusBadRequest, "remote path must be absolute")
	}
	if !strings.HasPrefix(workspaceRoot, "/") {
		return newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "ChemSSH workspace root must be absolute")
	}
	if workspaceRoot == "/" || remotePath == workspaceRoot || strings.HasPrefix(remotePath, workspaceRoot+"/") {
		return nil
	}
	return newChemSSHBridgeHTTPError(http.StatusForbidden, "remote path is outside the active ChemSSH workspace")
}

func validateChemSSHBridgeLocalPath(value, workspaceRoot string) (string, error) {
	workspaceRoot = cleanLocalPath(workspaceRoot)
	if workspaceRoot == "" {
		return "", newChemSSHBridgeHTTPError(http.StatusServiceUnavailable, "ChemSSH workspace root is not available")
	}
	localPath := cleanLocalPath(value)
	if localPath == "" {
		return "", newChemSSHBridgeHTTPError(http.StatusBadRequest, "missing local path")
	}
	if !filepath.IsAbs(localPath) {
		localPath = filepath.Join(workspaceRoot, localPath)
	}
	workspaceAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	localAbs, err := filepath.Abs(localPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(workspaceAbs, localAbs)
	if err != nil {
		return "", newChemSSHBridgeHTTPError(http.StatusForbidden, "local path is outside the active ChemSSH workspace")
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
		return localAbs, nil
	}
	return "", newChemSSHBridgeHTTPError(http.StatusForbidden, "local path is outside the active ChemSSH workspace")
}

func cleanRemotePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := posixpath.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func cleanLocalPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}
