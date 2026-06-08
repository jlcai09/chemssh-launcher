package gui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/chemssh"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/fileicon"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sftpclient"
	"chemssh-launcher/internal/sshclient"
	"chemssh-launcher/internal/version"
	"chemssh-launcher/internal/webview"

	"golang.org/x/crypto/ssh"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	rt             *runtime.Runtime
	mux            *http.ServeMux
	logs           *safeLog
	profileLogs    *profileLogStore
	localLog       *safeLog
	sftp           *sftpclient.Manager
	sftpSync       *sftpOpenSyncManager
	icons          *fileicon.Service
	transfers      *transferProgressStore
	options        Options
	baseURL        string
	mu             sync.Mutex
	session        *activeSession
	sessions       map[string]*activeSession
	proxyProfileID string
	sessionOpenSeq uint64
	bridge         chemSSHBridgeState
	shutdown       context.CancelFunc
	// activeTransferCount is updated by the frontend via /api/transfer-count.
	// It tracks how many transfer tasks are currently running.
	activeTransferCount atomic.Int32
}

type Options struct {
	UseWebView   bool
	ForceBrowser bool
	DevTools     bool
}

type activeSession struct {
	id            string
	name          string
	profile       config.Profile
	client        *ssh.Client
	process       *sshclient.RemoteProcess
	localProcess  *exec.Cmd
	tunnel        *sshclient.Tunnel
	healthStop    context.CancelFunc
	forwarding    bool
	ready         bool
	openSeq       uint64
	stopping      bool
	remotePID     int
	workspaceRoot string
}

type chemSSHBridgeState struct {
	sftpSessionID string
	profileID     string
	workspaceRoot string
}

type safeLog struct {
	mu    sync.Mutex
	lines []string
}

type profileLogStore struct {
	mu   sync.Mutex
	logs map[string]*safeLog
}

type profileLogWriter struct {
	store     *profileLogStore
	profileID string
}

func Run(stdout, stderr io.Writer) error {
	return RunWithOptions(stdout, stderr, Options{})
}

func RunWithOptions(stdout, stderr io.Writer, options Options) error {
	rt, err := runtime.New()
	if err != nil {
		return err
	}
	server := NewServer(rt)
	server.options = options
	defer cleanupSFTPOpenCache(server.localLog)
	defer server.sftp.CloseAll()
	defer server.sftpSync.CloseAll()
	defer server.closeChemSSHBridgeSession()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := "http://" + ln.Addr().String()
	server.baseURL = addr
	applyPendingCacheCleanup(server.localLog)
	startLine := "ChemSSH Launcher " + version.String() + " GUI: " + addr
	server.localLog.add(startLine)
	fmt.Fprintln(stdout, startLine)

	httpServer := &http.Server{Handler: server.mux}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	if options.UseWebView && !options.ForceBrowser {
		shellAddr := addr + "/shell"
		dataPath, err := config.DefaultWebViewDataDir()
		if err != nil {
			return err
		}
		if err := webview.Open(context.Background(), webview.Options{
			Title:            "ChemSSH Launcher",
			URL:              shellAddr,
			Width:            1240,
			Height:           820,
			Debug:            options.DevTools,
			CopyOnCtrlShiftC: true,
			DisableDevTools:  !options.DevTools,
			Fullscreen:       true,
			DataPath:         dataPath,
			CloseInterceptor: func() bool {
				return server.activeTransferCount.Load() > 0
			},
		}); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(ctx)
			err = <-errCh
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(ctx)
			serveErr := <-errCh
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return fmt.Errorf("open WebView2: %w; stop GUI server: %v", err, serveErr)
			}
			return fmt.Errorf("open WebView2: %w", err)
		}
	}

	if err := browser.Open(addr); err != nil {
		server.localLog.add("warning: could not open browser: " + err.Error())
		fmt.Fprintln(stderr, "warning: could not open browser:", err)
	}

	err = <-errCh
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func NewServer(rt *runtime.Runtime) *Server {
	s := &Server{
		rt:          rt,
		mux:         http.NewServeMux(),
		logs:        &safeLog{},
		profileLogs: newProfileLogStore(),
		localLog:    &safeLog{},
		sftp:        sftpclient.NewManager(),
		icons:       fileicon.NewService(),
		transfers:   newTransferProgressStore(),
		sessions:    map[string]*activeSession{},
	}
	s.sftpSync = newSFTPOpenSyncManager(s.sftp, s.logs)
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/shell", s.handleShell)
	s.mux.HandleFunc("/sftp", s.handleSFTPPage)
	s.mux.HandleFunc("/launcher-logs", s.handleLauncherLogsPage)
	s.mux.HandleFunc("/chemssh", s.handleChemSSHProxy)
	s.mux.HandleFunc("/chemssh/", s.handleChemSSHProxy)
	s.mux.HandleFunc("/assets/", s.handleChemSSHProxy)
	s.mux.Handle("/static/", http.FileServer(http.FS(assets)))
	s.mux.HandleFunc("/api/profiles", s.handleProfiles)
	s.mux.HandleFunc("/api/profiles/", s.handleProfileByID)
	s.mux.HandleFunc("/api/profile-test", s.handleProfileTest)
	s.mux.HandleFunc("/api/session/start", s.handleStart)
	s.mux.HandleFunc("/api/session/stop", s.handleStop)
	s.mux.HandleFunc("/api/session/stop-service", s.handleStopService)
	s.mux.HandleFunc("/api/session/status", s.handleStatus)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/sftp/connect", s.handleSFTPConnect)
	s.mux.HandleFunc("/api/sftp/disconnect", s.handleSFTPDisconnect)
	s.mux.HandleFunc("/api/sftp/list", s.handleSFTPList)
	s.mux.HandleFunc("/api/sftp/mkdir", s.handleSFTPMkdir)
	s.mux.HandleFunc("/api/sftp/create-file", s.handleSFTPCreateFile)
	s.mux.HandleFunc("/api/sftp/rename", s.handleSFTPRename)
	s.mux.HandleFunc("/api/sftp/delete", s.handleSFTPDelete)
	s.mux.HandleFunc("/api/sftp/upload", s.handleSFTPUpload)
	s.mux.HandleFunc("/api/sftp/download", s.handleSFTPDownload)
	s.mux.HandleFunc("/api/sftp/open", s.handleSFTPOpen)
	s.mux.HandleFunc("/api/sftp/open-text", s.handleSFTPOpenText)
	s.mux.HandleFunc("/api/sftp/open-sync-events", s.handleSFTPOpenSyncEvents)
	s.mux.HandleFunc("/api/chemssh-bridge/capabilities", s.handleChemSSHBridgeCapabilities)
	s.mux.HandleFunc("/api/chemssh-bridge/open", s.handleChemSSHBridgeOpen)
	s.mux.HandleFunc("/api/chemssh-bridge/open-text", s.handleChemSSHBridgeOpenText)
	s.mux.HandleFunc("/api/chemssh-bridge/open-sync-events", s.handleChemSSHBridgeOpenSyncEvents)
	s.mux.HandleFunc("/api/sftp/upload-local", s.handleSFTPUploadLocal)
	s.mux.HandleFunc("/api/sftp/download-local", s.handleSFTPDownloadLocal)
	s.mux.HandleFunc("/api/sftp/copy-remote", s.handleSFTPCopyRemote)
	s.mux.HandleFunc("/api/local/home", s.handleLocalHome)
	s.mux.HandleFunc("/api/local/list", s.handleLocalList)
	s.mux.HandleFunc("/api/local/mkdir", s.handleLocalMkdir)
	s.mux.HandleFunc("/api/local/create-file", s.handleLocalCreateFile)
	s.mux.HandleFunc("/api/local/rename", s.handleLocalRename)
	s.mux.HandleFunc("/api/local/delete", s.handleLocalDelete)
	s.mux.HandleFunc("/api/local/copy", s.handleLocalCopy)
	s.mux.HandleFunc("/api/local/open", s.handleLocalOpen)
	s.mux.HandleFunc("/api/local/open-text", s.handleLocalOpenText)
	s.mux.HandleFunc("/api/file-icon", s.handleFileIcon)
	s.mux.HandleFunc("/api/transfer-progress", s.handleTransferProgress)
	s.mux.HandleFunc("/api/transfer-control", s.handleTransferControl)
	s.mux.HandleFunc("/api/launcher-logs", s.handleLauncherLogs)
	s.mux.HandleFunc("/api/backend", s.handleBackendInfo)
	s.mux.HandleFunc("/api/backend/open-config-dir", s.handleOpenConfigDir)
	s.mux.HandleFunc("/api/backend/open-sftp-cache-dir", s.handleOpenSFTPCacheDir)
	s.mux.HandleFunc("/api/backend/export", s.handleExportProfiles)
	s.mux.HandleFunc("/api/backend/import", s.handleImportProfiles)
	s.mux.HandleFunc("/api/backend/clear-cache", s.handleClearCache)
	s.mux.HandleFunc("/api/version", s.handleVersion)
	s.mux.HandleFunc("/api/defaults", s.handleDefaults)
	s.mux.HandleFunc("/api/transfer-count", s.handleTransferCount)
	s.mux.HandleFunc("/api/", s.handleChemSSHProxy)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.isProfileProxyPath(r.URL.Path) {
		s.handleChemSSHProxy(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.serveVueApp(w, r)
}

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/shell" {
		http.NotFound(w, r)
		return
	}
	s.serveVueApp(w, r)
}

func (s *Server) handleSFTPPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/sftp" {
		http.NotFound(w, r)
		return
	}
	s.serveVueApp(w, r)
}

func (s *Server) handleLauncherLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/launcher-logs" {
		http.NotFound(w, r)
		return
	}
	s.serveVueApp(w, r)
}

func (s *Server) serveVueApp(w http.ResponseWriter, r *http.Request) {
	if file, err := assets.Open("static/vue/index.html"); err == nil {
		_ = file.Close()
		http.ServeFileFS(w, r, assets, "static/vue/index.html")
		return
	}
	http.Error(w, "frontend assets are not built; run go run ./tools/build", http.StatusInternalServerError)
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		profiles, err := s.rt.Profiles.List()
		writeJSON(w, profiles, err)
	case http.MethodPost:
		var req profileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		p := req.Profile
		if p.ID == "" {
			id, err := config.NewID()
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			p.ID = id
		}
		if err := s.saveProfileWithSecrets(p, req.Secrets); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, p, nil)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleProfileByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing profile id"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, err := s.rt.Profiles.Get(id)
		writeJSON(w, p, err)
	case http.MethodPut:
		var req profileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Profile.ID = id
		if err := s.saveProfileWithSecrets(req.Profile, req.Secrets); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, req.Profile, nil)
	case http.MethodDelete:
		if err := s.rt.Profiles.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		_ = s.rt.Secrets.Delete(id, secret.KeyPassword)
		_ = s.rt.Secrets.Delete(id, secret.KeyPrivatePassphrase)
		writeJSON(w, map[string]bool{"ok": true}, nil)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		ID            string `json:"id"`
		AcceptHostKey bool   `json:"accept_host_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.rt.Profiles.Get(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sessionLog := s.profileLogWriter(p.ID)
	s.profileLog(p.ID, "start requested for "+p.Name)
	if p.IsLocal() {
		s.startLocalSession(w, p)
		return
	}
	if warning := sshclient.PortWarning(p); warning != "" {
		s.profileLog(p.ID, "warning: "+warning)
	}
	if p.UsesNonLoopbackLocalHost() {
		s.profileLog(p.ID, "warning: local bind host "+p.LocalHost+" may expose the tunnel")
	}
	s.stopActiveForwardingBeforeStart(p.ID)
	if err := s.blockActiveForwardingConflict(p); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.checkLocalPort(p); err != nil {
		s.profileLog(p.ID, err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	if handled, err := s.restartForwardingIfPaused(p); handled {
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true}, nil)
		return
	}
	policy := sshclient.HostKeyStrict
	if req.AcceptHostKey {
		policy = sshclient.HostKeyAcceptNew
	}
	client, err := sshclient.DialWithHostKeyPolicy(p, s.rt.Secrets, policy)
	if err != nil {
		s.profileLog(p.ID, "SSH connection failed for "+p.Name+": "+err.Error())
		writeSSHError(w, err)
		return
	}
	s.profileLog(p.ID, "SSH connection OK for "+p.Name)
	check, err := sshclient.RunCheckPortCommand(client, p, sessionLog, sessionLog)
	if err != nil {
		_ = client.Close()
		s.profileLog(p.ID, "ChemSSH port check failed for "+p.Name+": "+err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	if check.Reusable {
		s.profileLog(p.ID, "remote ChemSSH is reusable; starting tunnel without launching another server")
	} else {
		s.profileLog(p.ID, "remote ChemSSH port is available; starting configured command")
	}

	s.mu.Lock()
	if s.sessionForProfileLocked(p.ID) != nil {
		s.mu.Unlock()
		_ = client.Close()
		writeError(w, http.StatusConflict, errors.New("a session is already running"))
		return
	}

	var process *sshclient.RemoteProcess
	if !check.Reusable {
		process, err = sshclient.StartRemoteCommand(client, p, sessionLog, sessionLog)
		if err != nil {
			s.mu.Unlock()
			_ = client.Close()
			writeError(w, http.StatusBadGateway, err)
			return
		}
	}
	tunnel, err := sshclient.StartTunnel(context.Background(), client, p)
	if err != nil {
		if process != nil {
			_ = process.Stop()
		}
		s.mu.Unlock()
		_ = client.Close()
		writeError(w, http.StatusConflict, err)
		return
	}

	healthCtx, healthStop := context.WithCancel(context.Background())
	session := &activeSession{
		id:         p.ID,
		name:       p.Name,
		profile:    p,
		client:     client,
		process:    process,
		tunnel:     tunnel,
		healthStop: healthStop,
		forwarding: true,
	}
	s.setSessionLocked(session)
	s.mu.Unlock()

	s.profileLog(p.ID, "starting "+p.Name+" at "+p.BrowserURL())
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
	if process != nil {
		go s.watchRemoteProcess(session)
	}
	go s.waitForRemoteHealth(session, p, healthCtx)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) startLocalSession(w http.ResponseWriter, p config.Profile) {
	if p.LocalHost == "" || p.LocalPort <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("local ChemSSH target host and port are required"))
		return
	}
	sessionLog := s.profileLogWriter(p.ID)
	if p.UsesNonLoopbackLocalHost() {
		s.profileLog(p.ID, "warning: local ChemSSH target "+p.LocalAddress()+" is not loopback")
	}
	s.stopActiveForwardingBeforeStart(p.ID)
	if err := s.blockActiveForwardingConflict(p); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if handled, err := s.restartLocalSessionIfPaused(p); handled {
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true}, nil)
		return
	}

	var cmd *exec.Cmd
	if !s.localHealthReady(p, 700*time.Millisecond) {
		var err error
		cmd, err = startLocalCommand(p, sessionLog, sessionLog)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.profileLog(p.ID, "local ChemSSH command started for "+p.Name)
	} else {
		s.profileLog(p.ID, "local ChemSSH is already reachable at "+p.BrowserURL())
	}

	healthCtx, healthStop := context.WithCancel(context.Background())
	session := &activeSession{
		id:           p.ID,
		name:         p.Name,
		profile:      p,
		localProcess: cmd,
		healthStop:   healthStop,
		forwarding:   true,
	}

	s.mu.Lock()
	if s.sessionForProfileLocked(p.ID) != nil {
		s.mu.Unlock()
		healthStop()
		stopLocalProcess(cmd)
		writeError(w, http.StatusConflict, errors.New("a session is already running"))
		return
	}
	s.setSessionLocked(session)
	s.mu.Unlock()

	if cmd != nil {
		go s.watchLocalProcess(session, cmd)
	}
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
	go s.waitForLocalHealth(session, p, healthCtx)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) restartLocalSessionIfPaused(p config.Profile) (bool, error) {
	s.mu.Lock()
	session := s.sessionForProfileLocked(p.ID)
	if session == nil {
		s.mu.Unlock()
		return false, nil
	}
	if session.forwarding {
		s.mu.Unlock()
		return true, errors.New("a session is already running")
	}
	session.profile = p
	session.forwarding = true
	session.ready = false
	healthCtx, healthStop := context.WithCancel(context.Background())
	session.healthStop = healthStop
	s.assignSessionOpenSeqLocked(session)
	s.promoteProxySessionLocked(session)
	s.mu.Unlock()

	go s.waitForLocalHealth(session, p, healthCtx)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
	s.profileLog(p.ID, "local ChemSSH forwarding restored for "+p.BrowserURL())
	return true, nil
}

func (s *Server) waitForLocalHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := netcheck.WaitForURL(ctx, p.HealthURL(), 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "local health check failed: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "local health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshLocalIdentity(session, p)
}

func (s *Server) localHealthReady(p config.Profile, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return netcheck.WaitForURL(ctx, p.HealthURL(), timeout, timeout) == nil
}

func startLocalCommand(p config.Profile, stdout, stderr io.Writer) (*exec.Cmd, error) {
	command := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(p.PreStartCommands)+"\n"+strings.TrimSpace(p.StartCommand), "\r\n"))
	if command == "" {
		return nil, errors.New("local ChemSSH is not reachable and no local start command is configured")
	}
	var cmd *exec.Cmd
	if goruntime.GOOS == "windows" {
		cmd = exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func (s *Server) sessionForProfileLocked(profileID string) *activeSession {
	if profileID != "" && s.sessions != nil {
		if session := s.sessions[profileID]; session != nil {
			return session
		}
	}
	if s.session != nil && (profileID == "" || s.session.id == profileID) {
		return s.session
	}
	return nil
}

func (s *Server) currentProxySessionLocked() *activeSession {
	if s.proxyProfileID != "" {
		if session := s.sessionForProfileLocked(s.proxyProfileID); session != nil {
			return session
		}
	}
	if s.session != nil {
		return s.session
	}
	for _, session := range s.sessions {
		if session != nil && session.forwarding {
			return session
		}
	}
	return nil
}

func (s *Server) setSessionLocked(session *activeSession) {
	if s.sessions == nil {
		s.sessions = map[string]*activeSession{}
	}
	s.assignSessionOpenSeqLocked(session)
	s.sessions[session.id] = session
	s.session = session
	s.proxyProfileID = session.id
}

func (s *Server) assignSessionOpenSeqLocked(session *activeSession) {
	if session == nil {
		return
	}
	s.sessionOpenSeq++
	session.openSeq = s.sessionOpenSeq
}

func (s *Server) removeSessionLocked(session *activeSession) {
	if session == nil {
		return
	}
	if s.sessions != nil && s.sessions[session.id] == session {
		delete(s.sessions, session.id)
	}
	if s.session == session {
		s.session = nil
	}
	if s.proxyProfileID == session.id {
		s.proxyProfileID = ""
	}
	if s.session == nil {
		for _, candidate := range s.sessions {
			if candidate != nil && candidate.forwarding {
				s.session = candidate
				s.proxyProfileID = candidate.id
				break
			}
		}
	}
}

func (s *Server) sessionStillActiveLocked(session *activeSession) bool {
	if session == nil {
		return false
	}
	return s.sessionForProfileLocked(session.id) == session
}

func (s *Server) promoteProxySessionLocked(session *activeSession) {
	if session == nil {
		return
	}
	s.session = session
	s.proxyProfileID = session.id
}

func (s *Server) blockActiveForwardingConflict(p config.Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessionForProfileLocked(p.ID)
	if session == nil {
		return nil
	}
	if session.forwarding {
		return errors.New("a session is already running")
	}
	return nil
}

func (s *Server) stopActiveForwardingBeforeStart(profileID string) {
	s.mu.Lock()
	session := s.sessionForProfileLocked(profileID)
	shouldStop := session != nil && session.forwarding
	s.mu.Unlock()
	if !shouldStop {
		return
	}
	s.profileLog(profileID, "existing forwarding is running; stopping it before start")
	s.stopForwarding(profileID)
}

func (s *Server) restartForwardingIfPaused(p config.Profile) (bool, error) {
	s.mu.Lock()
	session := s.sessionForProfileLocked(p.ID)
	if session == nil {
		s.mu.Unlock()
		return false, nil
	}
	if session.forwarding {
		s.mu.Unlock()
		return true, errors.New("a session is already running")
	}
	client := session.client
	if client == nil {
		s.mu.Unlock()
		return true, errors.New("session is still running, but its SSH connection is no longer available")
	}
	s.mu.Unlock()

	s.profileLog(p.ID, "service is already running; restarting forwarding only for "+p.Name)
	tunnel, err := sshclient.StartTunnel(context.Background(), client, p)
	if err != nil {
		return true, err
	}
	healthCtx, healthStop := context.WithCancel(context.Background())

	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) || session.forwarding {
		s.mu.Unlock()
		healthStop()
		_ = tunnel.Close()
		return true, errors.New("a session is already running")
	}
	session.profile = p
	session.tunnel = tunnel
	session.healthStop = healthStop
	session.forwarding = true
	session.ready = false
	s.assignSessionOpenSeqLocked(session)
	s.promoteProxySessionLocked(session)
	s.mu.Unlock()

	go s.waitForRestartedForwardingHealth(session, p, healthCtx)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
	s.profileLog(p.ID, "forwarding restarted for "+p.BrowserURL())
	return true, nil
}

func (s *Server) waitForRestartedForwardingHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := netcheck.WaitForURL(ctx, p.HealthURL(), 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "health check failed after forwarding restart: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshRemoteIdentity(session, p)
}

func (s *Server) waitForRemoteHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := netcheck.WaitForURL(ctx, p.HealthURL(), 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "health check failed: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshRemoteIdentity(session, p)
}

func (s *Server) markSessionReady(session *activeSession) {
	s.mu.Lock()
	if s.sessionStillActiveLocked(session) && session.forwarding {
		session.ready = true
	}
	s.mu.Unlock()
}

func (s *Server) isSessionReady(session *activeSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionStillActiveLocked(session) && session.forwarding && session.ready
}

func (s *Server) refreshRemoteIdentity(session *activeSession, p config.Profile) {
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	client := session.client
	s.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	identity, err := chemssh.FetchIdentity(ctx, client, p)
	if err != nil {
		s.profileLog(p.ID, "warning: could not read ChemSSH identity: "+err.Error())
		return
	}
	s.mu.Lock()
	if s.sessionStillActiveLocked(session) {
		session.remotePID = identity.PID
		session.workspaceRoot = identity.WorkspaceRoot
	}
	s.mu.Unlock()
	s.profileLog(p.ID, "ChemSSH identity OK: pid "+strconv.Itoa(identity.PID)+", version "+identity.ProjectVersion)
	go s.warmChemSSHBridgeSFTP(session, p, identity.WorkspaceRoot)
}

func (s *Server) refreshLocalIdentity(session *activeSession, p config.Profile) {
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	identity, err := chemssh.FetchLocalIdentity(ctx, p)
	if err != nil {
		s.profileLog(p.ID, "warning: could not read local ChemSSH identity: "+err.Error())
		return
	}
	s.mu.Lock()
	if s.sessionStillActiveLocked(session) {
		session.remotePID = identity.PID
		session.workspaceRoot = identity.WorkspaceRoot
	}
	s.mu.Unlock()
	s.profileLog(p.ID, "local ChemSSH identity OK: pid "+strconv.Itoa(identity.PID)+", version "+identity.ProjectVersion)
}

func (s *Server) openSessionURL(p config.Profile) {
	if s.options.UseWebView && !s.options.ForceBrowser {
		return
	}
	url := s.sessionProxyURL(p, s.sessionOpenSeqForProfile(p.ID))
	if err := browser.Open(url); err != nil {
		s.profileLog(p.ID, "warning: could not open browser: "+err.Error())
	}
}

func (s *Server) sessionOpenSeqForProfile(profileID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessionForProfileLocked(profileID); session != nil {
		return session.openSeq
	}
	return 0
}

func (s *Server) sessionProxyURL(p config.Profile, openSeq uint64) string {
	path := normalizedProxyPath(p.LocalURLPath)
	prefix := s.profileProxyPrefix(p)
	rawURL := s.baseURL + prefix
	if path != "/" {
		rawURL += path
	}
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	query := "profile_id=" + url.QueryEscape(p.ID)
	if openSeq > 0 {
		query += "&launcher_open_seq=" + strconv.FormatUint(openSeq, 10)
	}
	return rawURL + separator + query
}

func (s *Server) profileProxyPrefix(p config.Profile) string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.ID
	}
	return "/" + url.PathEscape(name) + "/chemssh"
}

func normalizedProxyPath(path string) string {
	if path == "" {
		return "/"
	}
	if path[0] != '/' {
		return "/" + path
	}
	return path
}

func (s *Server) activeChemSSHTarget(r *http.Request) (*url.URL, *activeSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessionForProxyRequestLocked(r)
	if session == nil || !session.forwarding {
		return nil, nil
	}
	target, err := url.Parse("http://" + session.profile.LocalAddress())
	if err != nil {
		return nil, nil
	}
	return target, session
}

func (s *Server) sessionForProxyRequestLocked(r *http.Request) *activeSession {
	if r != nil {
		if profileID := strings.TrimSpace(r.URL.Query().Get("profile_id")); profileID != "" {
			if session := s.sessionForProfileLocked(profileID); session != nil {
				return session
			}
		}
		if session := s.sessionFromProxyPathLocked(r.URL.Path); session != nil {
			return session
		}
		if session := s.sessionFromRefererLocked(r.Header.Get("Referer")); session != nil {
			return session
		}
	}
	return s.currentProxySessionLocked()
}

func (s *Server) sessionFromRefererLocked(value string) *activeSession {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil
	}
	return s.sessionFromProxyPathLocked(parsed.Path)
}

func (s *Server) sessionFromProxyPathLocked(path string) *activeSession {
	slug, ok := profileProxySlugFromPath(path)
	if !ok {
		return nil
	}
	for _, session := range s.sessions {
		if session == nil {
			continue
		}
		if strings.EqualFold(profileProxySlug(session.profile), slug) || session.id == slug {
			return session
		}
	}
	if s.session != nil && (strings.EqualFold(profileProxySlug(s.session.profile), slug) || s.session.id == slug) {
		return s.session
	}
	return nil
}

func (s *Server) isProfileProxyPath(path string) bool {
	_, ok := profileProxySlugFromPath(path)
	return ok
}

func profileProxySlugFromPath(path string) (string, bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	switch parts[0] {
	case "api", "assets", "launcher-logs", "shell", "sftp", "static":
		return "", false
	}
	slug, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(slug) == "" {
		return "", false
	}
	return slug, true
}

func profileProxySlug(p config.Profile) string {
	name := strings.TrimSpace(p.Name)
	if name != "" {
		return name
	}
	return p.ID
}

const chemsshProxyMaxRetries = 5
const chemsshProxyRetryBase = 200 * time.Millisecond

func (s *Server) handleChemSSHProxy(w http.ResponseWriter, r *http.Request) {
	target, session := s.activeChemSSHTarget(r)
	if target == nil || session == nil {
		setNoStore(w)
		writeError(w, http.StatusServiceUnavailable, errors.New("chemssh service is not available; start forwarding first"))
		return
	}
	if !s.isSessionReady(session) && wantsHTML(r) {
		writeLoadingPage(w, r.URL.RequestURI())
		return
	}

	var lastErr error
	for attempt := range chemsshProxyMaxRetries {
		if attempt > 0 {
			delay := chemsshProxyRetryBase * time.Duration(1<<(attempt-1))
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		proxy := s.newChemSSHReverseProxy(target, session)
		errCh := make(chan error, 1)
		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			errCh <- err
		}
		proxy.ServeHTTP(w, s.rewriteChemSSHProxyRequest(r, target, session))
		select {
		case err := <-errCh:
			lastErr = err
			if isRetriableProxyError(err) {
				log.Printf("chemssh proxy: retryable error on attempt %d: %v", attempt+1, err)
				continue
			}
			s.writeProxyError(w, r, http.StatusBadGateway, err)
			return
		default:
			return
		}
	}
	s.writeProxyError(w, r, http.StatusBadGateway, fmt.Errorf("proxy ChemSSH request failed after %d retries: %w", chemsshProxyMaxRetries, lastErr))
}

func (s *Server) writeProxyError(w http.ResponseWriter, r *http.Request, status int, err error) {
	if wantsHTML(r) {
		writeLoadingPage(w, r.URL.RequestURI())
		return
	}
	setNoStore(w)
	writeError(w, status, err)
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

func writeLoadingPage(w http.ResponseWriter, refreshURL string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setNoStore(w)
	w.Header().Set("Refresh", "2;url="+refreshURL)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, chemsshLoadingHTML)
}

func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

const chemsshLoadingHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ChemSSH - Loading</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{display:flex;align-items:center;justify-content:center;min-height:100vh;
  background:#f5f6fa;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;color:#333}
.card{text-align:center;padding:48px 64px;background:#fff;border-radius:12px;
  box-shadow:0 2px 12px rgba(0,0,0,.08)}
.spinner{width:40px;height:40px;margin:0 auto 20px;border:4px solid #e0e0e0;
  border-top-color:#4a90d9;border-radius:50%;animation:spin .8s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}
h2{font-size:18px;font-weight:500;margin-bottom:8px;color:#444}
p{font-size:14px;color:#888}
</style>
</head>
<body>
<div class="card">
  <div class="spinner"></div>
  <h2>ChemSSH is starting up...</h2>
  <p>The service is being prepared. This page will refresh automatically.</p>
</div>
</body>
</html>`

func isRetriableProxyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "wsarecv") ||
		strings.Contains(msg, "wsasend") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF") {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func (s *Server) rewriteChemSSHProxyRequest(r *http.Request, target *url.URL, session *activeSession) *http.Request {
	req := r.Clone(r.Context())
	req.Host = target.Host
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path = chemsshProxyTargetPath(r.URL.Path, session)
	req.URL.RawPath = req.URL.Path
	query := req.URL.Query()
	query.Del("profile_id")
	query.Del("launcher_open_seq")
	req.URL.RawQuery = query.Encode()
	return req
}

func chemsshProxyTargetPath(path string, session *activeSession) string {
	if session != nil {
		profilePrefix := "/" + url.PathEscape(profileProxySlug(session.profile))
		chemSSHPrefix := profilePrefix + "/chemssh"
		if path == chemSSHPrefix {
			return "/"
		}
		if strings.HasPrefix(path, chemSSHPrefix+"/") {
			trimmed := strings.TrimPrefix(path, chemSSHPrefix)
			if trimmed == "" {
				return "/"
			}
			return trimmed
		}
		if strings.HasPrefix(path, profilePrefix+"/") {
			return strings.TrimPrefix(path, profilePrefix)
		}
	}
	switch {
	case path == "/chemssh":
		return "/"
	case strings.HasPrefix(path, "/chemssh/"):
		trimmed := strings.TrimPrefix(path, "/chemssh")
		if trimmed == "" {
			return "/"
		}
		return trimmed
	default:
		return path
	}
}

func (s *Server) newChemSSHReverseProxy(target *url.URL, session *activeSession) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		req.Header.Set("X-Forwarded-Host", target.Host)
		req.Header.Set("X-Forwarded-Proto", "http")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy ChemSSH request failed: %w", err))
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		location := resp.Header.Get("Location")
		if location == "" {
			return nil
		}
		rewritten := rewriteChemSSHLocationHeader(location, target, session)
		if rewritten != "" {
			resp.Header.Set("Location", rewritten)
		}
		return nil
	}
	return proxy
}

func rewriteChemSSHLocationHeader(location string, target *url.URL, session *activeSession) string {
	parsed, err := url.Parse(location)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		if !sameHost(parsed, target) {
			return ""
		}
		parsed.Scheme = ""
		parsed.Host = ""
	}
	prefix := "/chemssh"
	if session != nil {
		prefix = "/" + url.PathEscape(profileProxySlug(session.profile)) + "/chemssh"
	}
	if parsed.Path == "/" {
		parsed.Path = prefix
	} else if strings.HasPrefix(parsed.Path, "/") && !strings.HasPrefix(parsed.Path, prefix) {
		parsed.Path = prefix + parsed.Path
	}
	return parsed.String()
}

func sameHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && a.Port() == b.Port()
}

func (s *Server) handleProfileTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		ID            string `json:"id"`
		AcceptHostKey bool   `json:"accept_host_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.rt.Profiles.Get(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if p.IsLocal() {
		if p.LocalHost == "" || p.LocalPort <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("local ChemSSH target host and port are required"))
			return
		}
		if err := netcheck.WaitForURL(r.Context(), p.HealthURL(), 3*time.Second, 500*time.Millisecond); err != nil {
			s.profileLog(p.ID, "local ChemSSH test failed for "+p.Name+": "+err.Error())
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.profileLog(p.ID, "local ChemSSH test OK for "+p.BrowserURL())
		writeJSON(w, map[string]bool{"ok": true}, nil)
		return
	}
	s.profileLog(p.ID, "testing SSH for "+p.Name)
	policy := sshclient.HostKeyStrict
	if req.AcceptHostKey {
		policy = sshclient.HostKeyAcceptNew
	}
	client, err := sshclient.DialWithHostKeyPolicy(p, s.rt.Secrets, policy)
	if err != nil {
		s.profileLog(p.ID, "SSH connection failed for "+p.Name+": "+err.Error())
		writeSSHError(w, err)
		return
	}
	s.profileLog(p.ID, "SSH connection OK for "+p.Name)
	testLog := s.profileLogWriter(p.ID)
	if _, err := sshclient.RunCheckPortCommand(client, p, testLog, testLog); err != nil {
		_ = client.Close()
		s.profileLog(p.ID, "ChemSSH port check failed for "+p.Name+": "+err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	_ = client.Close()
	s.profileLog(p.ID, "ChemSSH port check OK for "+p.RemoteAddress())
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) checkLocalPort(p config.Profile) error {
	ok, err := netcheck.IsPortAvailable(p.LocalHost, p.LocalPort)
	if err != nil {
		return err
	}
	if ok {
		s.profileLog(p.ID, "local port OK: "+p.LocalAddress())
		return nil
	}
	ports, err := netcheck.SuggestFreePorts(p.LocalHost, p.LocalPort, 3)
	if err != nil {
		return err
	}
	return fmt.Errorf("local port occupied: %s is already in use; suggested local ports: %v", p.LocalAddress(), ports)
}

func (s *Server) watchRemoteProcess(session *activeSession) {
	err := <-session.process.Done()
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	if session.stopping {
		session.process = nil
		s.mu.Unlock()
		return
	}
	if err == nil {
		session.process = nil
		s.mu.Unlock()
		s.sessionLog(session, "remote command exited normally; keeping forwarding open")
		return
	}
	s.removeSessionLocked(session)
	tunnel := session.tunnel
	client := session.client
	healthStop := session.healthStop
	session.tunnel = nil
	session.client = nil
	session.healthStop = nil
	session.forwarding = false
	session.ready = false
	s.mu.Unlock()

	s.closeChemSSHBridgeSessionForProfile(session.id)
	s.sessionLog(session, "remote command stopped with error: "+err.Error())
	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (s *Server) watchLocalProcess(session *activeSession, cmd *exec.Cmd) {
	err := cmd.Wait()
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	if session.stopping {
		session.localProcess = nil
		s.mu.Unlock()
		return
	}
	s.removeSessionLocked(session)
	healthStop := session.healthStop
	session.healthStop = nil
	session.localProcess = nil
	session.forwarding = false
	session.ready = false
	s.mu.Unlock()

	s.closeChemSSHBridgeSessionForProfile(session.id)
	if healthStop != nil {
		healthStop()
	}
	if err == nil {
		s.sessionLog(session, "local ChemSSH command exited")
		return
	}
	s.sessionLog(session, "local ChemSSH command stopped with error: "+err.Error())
}

func (s *Server) stopForwarding(profileID string) {
	s.mu.Lock()
	session := s.sessionForProfileLocked(profileID)
	if session == nil && profileID == "" {
		session = s.currentProxySessionLocked()
	}
	if session == nil || !session.forwarding {
		s.mu.Unlock()
		return
	}
	tunnel := session.tunnel
	healthStop := session.healthStop
	client := session.client
	session.healthStop = nil
	session.tunnel = nil
	session.forwarding = false
	session.ready = false
	clearSession := session.process == nil && session.localProcess == nil && session.remotePID <= 0 && client == nil
	if clearSession {
		s.removeSessionLocked(session)
		session.client = nil
	}
	s.mu.Unlock()

	s.closeChemSSHBridgeSessionForProfile(session.id)
	s.sessionLog(session, "stopping forwarding for "+session.name)
	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if session.profile.IsLocal() {
		if clearSession {
			s.sessionLog(session, "local ChemSSH session stopped")
		} else {
			s.sessionLog(session, "local ChemSSH proxy stopped; local service is still running")
		}
		return
	}
	if session.process == nil && session.remotePID <= 0 && client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		identity, err := chemssh.FetchIdentity(ctx, client, session.profile)
		cancel()
		if err == nil {
			s.mu.Lock()
			if s.sessionStillActiveLocked(session) {
				session.remotePID = identity.PID
			}
			s.mu.Unlock()
			s.sessionLog(session, "ChemSSH identity OK: pid "+strconv.Itoa(identity.PID)+", version "+identity.ProjectVersion)
		} else {
			s.sessionLog(session, "warning: could not read ChemSSH identity after stopping forwarding: "+err.Error())
		}
	}
	if clearSession && client != nil {
		_ = client.Close()
	}
	if clearSession {
		s.sessionLog(session, "forwarding stopped")
	} else {
		s.sessionLog(session, "forwarding stopped; remote service is still running")
	}
}

func (s *Server) stopSessionResources(session *activeSession, stopRemote bool) {
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	s.removeSessionLocked(session)
	session.stopping = true
	tunnel := session.tunnel
	process := session.process
	localProcess := session.localProcess
	client := session.client
	healthStop := session.healthStop
	profile := session.profile
	session.tunnel = nil
	session.process = nil
	session.localProcess = nil
	session.client = nil
	session.healthStop = nil
	session.forwarding = false
	session.ready = false
	s.mu.Unlock()

	s.closeChemSSHBridgeSessionForProfile(profile.ID)
	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if profile.IsLocal() {
		if stopRemote && localProcess != nil {
			stopLocalProcess(localProcess)
			s.profileLog(profile.ID, "local ChemSSH command stopped")
		} else if stopRemote {
			s.profileLog(profile.ID, "local ChemSSH was external; leaving service running")
		}
		if stopRemote {
			s.profileLog(profile.ID, "local service and proxy stopped")
		} else {
			s.profileLog(profile.ID, "local session resources stopped")
		}
		return
	}
	if stopRemote {
		pid := session.remotePID
		if pid <= 0 && client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			identity, err := chemssh.FetchIdentity(ctx, client, profile)
			cancel()
			if err != nil {
				s.profileLog(profile.ID, "warning: could not read ChemSSH identity before stopping: "+err.Error())
			} else {
				pid = identity.PID
			}
		}
		if pid > 0 && client != nil {
			if err := sshclient.StopRemotePID(client, pid); err != nil {
				s.profileLog(profile.ID, "warning: could not kill remote ChemSSH pid "+strconv.Itoa(pid)+": "+err.Error())
			} else {
				s.profileLog(profile.ID, "remote ChemSSH pid stopped: "+strconv.Itoa(pid))
			}
		} else if process != nil {
			_ = process.Stop()
		} else {
			s.profileLog(profile.ID, "warning: remote ChemSSH pid is unknown; remote service may still be running")
		}
	}
	if client != nil {
		_ = client.Close()
	}
	if stopRemote {
		s.profileLog(profile.ID, "remote service and forwarding stopped")
	} else {
		s.profileLog(profile.ID, "session resources stopped")
	}
}

func stopLocalProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

type sessionActionRequest struct {
	ID string `json:"id"`
}

func decodeSessionActionRequest(r *http.Request) sessionActionRequest {
	var req sessionActionRequest
	if r.Body == nil {
		return req
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	return req
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	req := decodeSessionActionRequest(r)
	s.stopForwarding(req.ID)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleStopService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	req := decodeSessionActionRequest(r)
	s.mu.Lock()
	session := s.sessionForProfileLocked(req.ID)
	if session == nil && req.ID == "" {
		session = s.currentProxySessionLocked()
	}
	s.mu.Unlock()
	if session != nil {
		s.sessionLog(session, "stopping service and forwarding for "+session.name)
		s.stopSessionResources(session, true)
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	profileID := strings.TrimSpace(r.URL.Query().Get("profile_id"))
	s.mu.Lock()
	session := s.sessionForProfileLocked(profileID)
	if session == nil && profileID == "" {
		session = s.currentProxySessionLocked()
	}
	if session != nil && profileID != "" && session.forwarding && r.URL.Query().Get("activate") == "1" {
		s.promoteProxySessionLocked(session)
	}
	running := session != nil
	forwarding := false
	name := ""
	id := ""
	openSeq := uint64(0)
	profile := config.Profile{}
	openBrowser := false
	if running {
		name = session.name
		id = session.id
		forwarding = session.forwarding
		openSeq = session.openSeq
		profile = session.profile
		openBrowser = session.profile.OpenBrowser
	}
	s.mu.Unlock()

	url := ""
	if running && forwarding {
		url = s.sessionProxyURL(profile, openSeq)
	}
	writeJSON(w, map[string]any{"running": running, "forwarding": forwarding, "id": id, "name": name, "url": url, "open_browser": openBrowser, "open_seq": openSeq}, nil)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	profileID := strings.TrimSpace(r.URL.Query().Get("profile_id"))
	if profileID != "" && s.profileLogs != nil {
		writeJSON(w, map[string][]string{"lines": s.profileLogs.snapshot(profileID)}, nil)
		return
	}
	writeJSON(w, map[string][]string{"lines": s.logs.snapshot()}, nil)
}

func (s *Server) handleLauncherLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string][]string{"lines": s.localLog.snapshot()}, nil)
}

func (s *Server) handleBackendInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	info, err := backendInfo()
	writeJSON(w, info, err)
}

func (s *Server) handleOpenConfigDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	info, err := backendInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(info.ConfigDir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := browser.OpenPath(info.ConfigDir); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.localLog.add("opened config directory: " + info.ConfigDir)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleOpenSFTPCacheDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	info, err := backendInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(info.SFTPOpenCacheDir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := browser.OpenPath(info.SFTPOpenCacheDir); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.localLog.add("opened SFTP file cache directory: " + info.SFTPOpenCacheDir)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleExportProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	profiles, err := s.rt.Profiles.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="chemssh-launcher-profiles.json"`)
	if err := exportProfiles(w, profiles); err != nil {
		s.localLog.add("export profiles failed: " + err.Error())
	}
}

func (s *Server) handleImportProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	profiles, err := importProfilesFromReader(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, profile := range profiles {
		_ = s.rt.Secrets.Delete(profile.ID, secret.KeyPassword)
		_ = s.rt.Secrets.Delete(profile.ID, secret.KeyPrivatePassphrase)
		if err := s.rt.Profiles.Save(profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.localLog.add("imported profiles: " + strconv.Itoa(len(profiles)))
	writeJSON(w, map[string]any{"ok": true, "count": len(profiles)}, nil)
}

func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	info, messages, err := clearBackendCache()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, message := range messages {
		s.localLog.add(message)
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"info":     info,
		"messages": messages,
	}, nil)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": version.String()}, nil)
}

func (s *Server) handleDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, config.NewProfileDefaults(), nil)
}

type profileRequest struct {
	Profile config.Profile `json:"profile"`
	Secrets secretRequest  `json:"secrets"`
}

type secretRequest struct {
	PasswordAction   string `json:"password_action"`
	Password         string `json:"password"`
	PassphraseAction string `json:"passphrase_action"`
	Passphrase       string `json:"passphrase"`
}

func (s *Server) saveProfileWithSecrets(p config.Profile, req secretRequest) error {
	if p.IsLocal() {
		p.AuthMethod = ""
		p.SSHHost = ""
		p.SSHPort = 0
		p.SSHUser = ""
		p.PrivateKeyPath = ""
		p.HasPassword = false
		p.HasPrivateKeyPassphrase = false
		_ = s.rt.Secrets.Delete(p.ID, secret.KeyPassword)
		_ = s.rt.Secrets.Delete(p.ID, secret.KeyPrivatePassphrase)
		return s.rt.Profiles.Save(p)
	}
	if p.AuthMethod == config.AuthPassword {
		switch req.PasswordAction {
		case "replace":
			started := time.Now()
			if err := s.rt.Secrets.Set(p.ID, secret.KeyPassword, req.Password); err != nil {
				return err
			}
			s.profileLog(p.ID, "password saved to secret store in "+time.Since(started).Round(time.Millisecond).String())
			p.HasPassword = true
		case "clear":
			started := time.Now()
			if err := s.rt.Secrets.Delete(p.ID, secret.KeyPassword); err != nil {
				return err
			}
			s.profileLog(p.ID, "password cleared from secret store in "+time.Since(started).Round(time.Millisecond).String())
			p.HasPassword = false
		case "keep":
			p.HasPassword = p.HasPassword
		}
		_ = s.rt.Secrets.Delete(p.ID, secret.KeyPrivatePassphrase)
		p.HasPrivateKeyPassphrase = false
		p.PrivateKeyPath = ""
	}
	if p.AuthMethod == config.AuthPrivateKey {
		switch req.PassphraseAction {
		case "replace":
			started := time.Now()
			if err := s.rt.Secrets.Set(p.ID, secret.KeyPrivatePassphrase, req.Passphrase); err != nil {
				return err
			}
			s.profileLog(p.ID, "private key passphrase saved to secret store in "+time.Since(started).Round(time.Millisecond).String())
			p.HasPrivateKeyPassphrase = true
		case "clear":
			started := time.Now()
			if err := s.rt.Secrets.Delete(p.ID, secret.KeyPrivatePassphrase); err != nil {
				return err
			}
			s.profileLog(p.ID, "private key passphrase cleared from secret store in "+time.Since(started).Round(time.Millisecond).String())
			p.HasPrivateKeyPassphrase = false
		case "keep":
			p.HasPrivateKeyPassphrase = p.HasPrivateKeyPassphrase
		}
		_ = s.rt.Secrets.Delete(p.ID, secret.KeyPassword)
		p.HasPassword = false
	}
	return s.rt.Profiles.Save(p)
}

func (l *safeLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			l.add(line)
		}
	}
	return len(p), nil
}

func (l *safeLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, time.Now().Format("15:04:05")+"  "+line)
	if len(l.lines) > 500 {
		l.lines = l.lines[len(l.lines)-500:]
	}
}

func (l *safeLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func newProfileLogStore() *profileLogStore {
	return &profileLogStore{logs: map[string]*safeLog{}}
}

func (s *profileLogStore) forProfile(profileID string) *safeLog {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		profileID = "_unspecified"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[profileID]
	if log == nil {
		log = &safeLog{}
		s.logs[profileID] = log
	}
	return log
}

func (s *profileLogStore) add(profileID, line string) {
	s.forProfile(profileID).add(line)
}

func (s *profileLogStore) snapshot(profileID string) []string {
	return s.forProfile(profileID).snapshot()
}

func (w profileLogWriter) Write(p []byte) (int, error) {
	w.store.forProfile(w.profileID).Write(p)
	return len(p), nil
}

func (s *Server) profileLog(profileID, line string) {
	if s.profileLogs == nil {
		s.logs.add(line)
		return
	}
	s.profileLogs.add(profileID, line)
}

func (s *Server) sessionLog(session *activeSession, line string) {
	if session == nil {
		s.logs.add(line)
		return
	}
	s.profileLog(session.id, line)
}

func (s *Server) profileLogWriter(profileID string) io.Writer {
	if s.profileLogs == nil {
		return s.logs
	}
	return profileLogWriter{store: s.profileLogs, profileID: profileID}
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "status": strconv.Itoa(status)}); encErr != nil {
		log.Printf("write json error response: %v", encErr)
	}
}

func writeSSHError(w http.ResponseWriter, err error) {
	var hostKeyErr *sshclient.HostKeyVerificationError
	if errors.As(err, &hostKeyErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionRequired)
		body := map[string]any{
			"error":  hostKeyErr.Error(),
			"status": strconv.Itoa(http.StatusPreconditionRequired),
			"host_key": map[string]any{
				"host":             hostKeyErr.Host,
				"address":          hostKeyErr.Address,
				"key_type":         hostKeyErr.KeyType,
				"fingerprint":      hostKeyErr.Fingerprint,
				"known_hosts_path": hostKeyErr.KnownHostsPath,
				"mismatch":         hostKeyErr.Mismatch,
			},
		}
		if encErr := json.NewEncoder(w).Encode(body); encErr != nil {
			log.Printf("write ssh error response: %v", encErr)
		}
		return
	}
	writeError(w, http.StatusBadGateway, err)
}
