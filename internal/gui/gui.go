package gui

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
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
	"chemssh-launcher/internal/health"
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
	rt                       *runtime.Runtime
	mux                      *http.ServeMux
	logs                     *safeLog
	profileLogs              *profileLogStore
	localLog                 *safeLog
	sftp                     *sftpclient.Manager
	sftpSync                 *sftpOpenSyncManager
	icons                    *fileicon.Service
	iconsOnce                sync.Once
	transfers                *transferProgressStore
	shells                   *shellManager
	apiToken                 string
	options                  Options
	baseURL                  string
	mu                       sync.Mutex
	session                  *activeSession
	sessions                 map[string]*activeSession
	pendingStart             map[string]bool
	lastFailure              map[string]sessionFailure
	retiredProfiles          map[string]bool
	proxyProfileID           string
	sessionOpenSeq           uint64
	bridge                   chemSSHBridgeState
	clientIdentity           launcherClientIdentity
	shutdown                 context.CancelFunc
	browserShutdownOnce      sync.Once
	browserClients           map[string]time.Time
	browserClientSeen        bool
	browserClientsEmptySince time.Time
	// activeTransferCount is updated by the frontend via /api/transfer-count.
	// It tracks how many transfer tasks are currently running.
	activeTransferCount atomic.Int32
}

const (
	browserClientCheckInterval = time.Second
	browserClientStaleAfter    = 75 * time.Second
	browserClientGoneGrace     = 10 * time.Second
	launcherAPITokenCookie     = "chemssh_launcher_token"
	launcherAPITokenHeader     = "X-ChemSSH-Launcher-Token"
)

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
	proxyChannel  *sshclient.LocalProxyChannel
	proxyErr      string
	healthStop    context.CancelFunc
	forwarding    bool
	ready         bool
	openSeq       uint64
	stopping      bool
	remotePID     int
	workspaceRoot string
}

// sessionFailure records why the most recent start attempt for a profile
// failed. It lives on the Server rather than on activeSession because some
// failures happen before a session exists or after one has been removed.
type sessionFailure struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type chemSSHBridgeState struct {
	sftpSessionID string
	profileID     string
	workspaceRoot string
	connecting    *chemSSHBridgeConnect
}

type chemSSHBridgeConnect struct {
	profileID     string
	workspaceRoot string
	done          chan struct{}
	sessionID     string
	err           error
}

type safeLog struct {
	mu        sync.Mutex
	lines     []string
	listeners []chan string // SSE listeners for real-time log streaming
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
	serverCtx, serverCancel := context.WithCancel(context.Background())
	server.shutdown = serverCancel
	defer serverCancel()
	clientIdentity, err := loadOrCreateDefaultLauncherClientIdentity()
	if err != nil {
		return err
	}
	server.clientIdentity = clientIdentity
	defer cleanupSFTPOpenCache(server.localLog)
	defer server.sftp.CloseAll()
	defer server.sftpSync.CloseAll()
	defer server.shells.closeAll()
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
			server.shells.closeAll()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(ctx)
			err = <-errCh
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		} else {
			server.shells.closeAll()
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

	server.startBrowserClientWatchdog(serverCtx)

	select {
	case err = <-errCh:
	case <-serverCtx.Done():
		server.shells.closeAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = httpServer.Shutdown(ctx)
		cancel()
		err = <-errCh
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func NewServer(rt *runtime.Runtime) *Server {
	s := &Server{
		rt:              rt,
		mux:             http.NewServeMux(),
		logs:            &safeLog{},
		profileLogs:     newProfileLogStore(),
		localLog:        &safeLog{},
		sftp:            sftpclient.NewManager(),
		transfers:       newTransferProgressStore(),
		shells:          newShellManager(),
		apiToken:        newLauncherAPIToken(),
		sessions:        map[string]*activeSession{},
		pendingStart:    map[string]bool{},
		lastFailure:     map[string]sessionFailure{},
		retiredProfiles: map[string]bool{},
		browserClients:  map[string]time.Time{},
	}
	s.sftpSync = newSFTPOpenSyncManager(s.sftp, s.logs)
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/shell", s.handleShell)
	s.mux.HandleFunc("/terminal", s.handleTerminalPage)
	s.mux.HandleFunc("/sftp", s.handleSFTPPage)
	s.mux.HandleFunc("/launcher-logs", s.handleLauncherLogsPage)
	s.mux.HandleFunc("/chemssh", s.handleChemSSHProxy)
	s.mux.HandleFunc("/chemssh/", s.handleChemSSHProxy)
	s.mux.HandleFunc("/assets/", s.handleChemSSHProxy)

	// Static files with gzip compression
	s.mux.Handle("/static/", staticGzipHandler(http.FileServer(http.FS(assets))))

	// Launcher API endpoints with gzip compression (for JSON responses)
	s.mux.HandleFunc("/api/profiles", s.apiHandler(gzipHandler(s.handleProfiles)))
	s.mux.HandleFunc("/api/profiles/reorder", s.apiHandler(gzipHandler(s.handleReorderProfiles)))
	s.mux.HandleFunc("/api/profiles/duplicate", s.apiHandler(gzipHandler(s.handleDuplicateProfile)))
	s.mux.HandleFunc("/api/profiles/", s.apiHandler(gzipHandler(s.handleProfileByID)))
	s.mux.HandleFunc("/api/profile-test", s.apiHandler(gzipHandler(s.handleProfileTest)))
	s.mux.HandleFunc("/api/session/start", s.apiHandler(gzipHandler(s.handleStart)))
	s.mux.HandleFunc("/api/session/stop", s.apiHandler(gzipHandler(s.handleStop)))
	s.mux.HandleFunc("/api/session/stop-service", s.apiHandler(gzipHandler(s.handleStopService)))
	s.mux.HandleFunc("/api/session/status", s.apiHandler(gzipHandler(s.handleStatus)))
	s.mux.HandleFunc("/api/session/statuses", s.apiHandler(gzipHandler(s.handleStatuses)))
	s.mux.HandleFunc("/api/session/activate", s.apiHandler(gzipHandler(s.handleActivate)))
	s.mux.HandleFunc("/api/session/shell/open", s.apiHandler(gzipHandler(s.handleShellOpen)))
	s.mux.HandleFunc("/api/session/shell/output", s.handleShellOutput)
	s.mux.HandleFunc("/api/session/shell/input", s.apiHandler(s.handleShellInput))
	s.mux.HandleFunc("/api/session/shell/resize", s.apiHandler(gzipHandler(s.handleShellResize)))
	s.mux.HandleFunc("/api/session/shell/close", s.apiHandler(gzipHandler(s.handleShellClose)))
	s.mux.HandleFunc("/api/logs", s.apiHandler(s.handleLogs))              // No gzip: small payload, high frequency
	s.mux.HandleFunc("/api/logs/stream", s.apiHandler(s.handleLogsStream)) // SSE endpoint for real-time log streaming

	// SFTP API - list operations benefit from compression
	s.mux.HandleFunc("/api/sftp/connect", s.apiHandler(gzipHandler(s.handleSFTPConnect)))
	s.mux.HandleFunc("/api/sftp/disconnect", s.apiHandler(gzipHandler(s.handleSFTPDisconnect)))
	s.mux.HandleFunc("/api/sftp/reconnect", s.apiHandler(gzipHandler(s.handleSFTPReconnect)))
	s.mux.HandleFunc("/api/sftp/list", s.apiHandler(gzipHandler(s.handleSFTPList)))
	s.mux.HandleFunc("/api/sftp/realpath", s.apiHandler(gzipHandler(s.handleSFTPRealPath)))
	s.mux.HandleFunc("/api/sftp/mkdir", s.apiHandler(gzipHandler(s.handleSFTPMkdir)))
	s.mux.HandleFunc("/api/sftp/create-file", s.apiHandler(gzipHandler(s.handleSFTPCreateFile)))
	s.mux.HandleFunc("/api/sftp/rename", s.apiHandler(gzipHandler(s.handleSFTPRename)))
	s.mux.HandleFunc("/api/sftp/delete", s.apiHandler(gzipHandler(s.handleSFTPDelete)))

	// SFTP upload/download - no compression (binary data)
	s.mux.HandleFunc("/api/sftp/upload", s.apiHandler(s.handleSFTPUpload))
	s.mux.HandleFunc("/api/sftp/download", s.apiHandler(s.handleSFTPDownload))

	s.mux.HandleFunc("/api/sftp/open", s.apiHandler(gzipHandler(s.handleSFTPOpen)))
	s.mux.HandleFunc("/api/sftp/open-text", s.apiHandler(gzipHandler(s.handleSFTPOpenText)))
	s.mux.HandleFunc("/api/sftp/open-sync-events", s.apiHandler(gzipHandler(s.handleSFTPOpenSyncEvents)))

	// ChemSSH Bridge API
	s.mux.HandleFunc("/api/chemssh-bridge/capabilities", s.apiHandler(gzipHandler(s.handleChemSSHBridgeCapabilities)))
	s.mux.HandleFunc("/api/chemssh-bridge/client-identity", s.apiHandler(gzipHandler(s.handleChemSSHBridgeClientIdentity)))
	s.mux.HandleFunc("/api/chemssh-bridge/open", s.apiHandler(gzipHandler(s.handleChemSSHBridgeOpen)))
	s.mux.HandleFunc("/api/chemssh-bridge/open-text", s.apiHandler(gzipHandler(s.handleChemSSHBridgeOpenText)))
	s.mux.HandleFunc("/api/chemssh-bridge/open-sync-events", s.apiHandler(gzipHandler(s.handleChemSSHBridgeOpenSyncEvents)))

	// SFTP local operations
	s.mux.HandleFunc("/api/sftp/upload-local", s.apiHandler(s.handleSFTPUploadLocal))
	s.mux.HandleFunc("/api/sftp/download-local", s.apiHandler(s.handleSFTPDownloadLocal))
	s.mux.HandleFunc("/api/sftp/copy-remote", s.apiHandler(gzipHandler(s.handleSFTPCopyRemote)))

	// Local file API
	s.mux.HandleFunc("/api/local/home", s.apiHandler(gzipHandler(s.handleLocalHome)))
	s.mux.HandleFunc("/api/local/list", s.apiHandler(gzipHandler(s.handleLocalList)))
	s.mux.HandleFunc("/api/local/mkdir", s.apiHandler(gzipHandler(s.handleLocalMkdir)))
	s.mux.HandleFunc("/api/local/create-file", s.apiHandler(gzipHandler(s.handleLocalCreateFile)))
	s.mux.HandleFunc("/api/local/rename", s.apiHandler(gzipHandler(s.handleLocalRename)))
	s.mux.HandleFunc("/api/local/delete", s.apiHandler(gzipHandler(s.handleLocalDelete)))
	s.mux.HandleFunc("/api/local/copy", s.apiHandler(gzipHandler(s.handleLocalCopy)))
	s.mux.HandleFunc("/api/local/open", s.apiHandler(gzipHandler(s.handleLocalOpen)))
	s.mux.HandleFunc("/api/local/open-text", s.apiHandler(gzipHandler(s.handleLocalOpenText)))

	// File icon - PNG already compressed, no gzip needed (lazy init)
	s.mux.HandleFunc("/api/file-icon", s.apiHandler(s.handleFileIcon))

	// Transfer operations
	s.mux.HandleFunc("/api/transfer-progress", s.apiHandler(gzipHandler(s.handleTransferProgress)))
	s.mux.HandleFunc("/api/transfer-control", s.apiHandler(gzipHandler(s.handleTransferControl)))

	// Backend operations
	s.mux.HandleFunc("/api/launcher-logs", s.apiHandler(gzipHandler(s.handleLauncherLogs)))
	s.mux.HandleFunc("/api/backend", s.apiHandler(gzipHandler(s.handleBackendInfo)))
	s.mux.HandleFunc("/api/backend/open-config-dir", s.apiHandler(gzipHandler(s.handleOpenConfigDir)))
	s.mux.HandleFunc("/api/backend/open-sftp-cache-dir", s.apiHandler(gzipHandler(s.handleOpenSFTPCacheDir)))
	s.mux.HandleFunc("/api/backend/export", s.apiHandler(s.handleExportProfiles)) // JSON download, no gzip
	s.mux.HandleFunc("/api/backend/import", s.apiHandler(gzipHandler(s.handleImportProfiles)))
	s.mux.HandleFunc("/api/backend/clear-cache", s.apiHandler(gzipHandler(s.handleClearCache)))
	s.mux.HandleFunc("/api/ui-settings", s.apiHandler(gzipHandler(s.handleUISettings)))

	// Misc API
	s.mux.HandleFunc("/api/version", s.apiHandler(gzipHandler(s.handleVersion)))
	s.mux.HandleFunc("/api/defaults", s.apiHandler(gzipHandler(s.handleDefaults)))
	s.mux.HandleFunc("/api/transfer-count", s.apiHandler(gzipHandler(s.handleTransferCount)))
	s.mux.HandleFunc("/api/browser-client/heartbeat", s.apiHandler(gzipHandler(s.handleBrowserClientHeartbeat)))
	s.mux.HandleFunc("/api/browser-client/close", s.apiHandler(gzipHandler(s.handleBrowserClientClose)))

	// ChemSSH proxy - keep transparent, no compression
	s.mux.HandleFunc("/api/", s.handleChemSSHProxy)
}

func newLauncherAPIToken() string {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("generate launcher API token: " + err.Error())
	}
	return hex.EncodeToString(data[:])
}

func (s *Server) apiHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.setLauncherAPITokenCookie(w)
		if launcherAPIMethodRequiresToken(r.Method) {
			if err := s.validateLauncherAPIRequest(r); err != nil {
				writeError(w, http.StatusForbidden, err)
				return
			}
		}
		next(w, r)
	}
}

func launcherAPIMethodRequiresToken(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func (s *Server) setLauncherAPITokenCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     launcherAPITokenCookie,
		Value:    s.apiToken,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) validateLauncherAPIRequest(r *http.Request) error {
	if err := validateLauncherAPIOrigin(r); err != nil {
		return err
	}
	headerToken := r.Header.Get(launcherAPITokenHeader)
	cookie, err := r.Cookie(launcherAPITokenCookie)
	if err != nil || headerToken == "" || s.apiToken == "" {
		return errors.New("missing launcher API token")
	}
	if !constantTimeTokenEqual(headerToken, s.apiToken) || !constantTimeTokenEqual(cookie.Value, s.apiToken) {
		return errors.New("invalid launcher API token")
	}
	return nil
}

func validateLauncherAPIOrigin(r *http.Request) error {
	// Browser requests normally include Origin/Sec-Fetch-Site. Empty Origin is
	// allowed for desktop or non-browser clients; the cookie/header token pair
	// remains the required write-authority proof.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
			return errors.New("invalid launcher API origin")
		}
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return nil
	default:
		return errors.New("cross-site launcher API request rejected")
	}
}

func constantTimeTokenEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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

func (s *Server) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/terminal" {
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
	s.setLauncherAPITokenCookie(w)
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
			writeProfileSaveError(w, err)
			return
		}
		writeJSON(w, p, nil)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleReorderProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.rt.Profiles.Reorder(req.IDs); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	profiles, err := s.rt.Profiles.List()
	writeJSON(w, profiles, err)
}

func (s *Server) handleDuplicateProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	source, err := s.rt.Profiles.Get(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, err := config.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	clone := source
	clone.ID = id
	if name := strings.TrimSpace(req.Name); name != "" {
		clone.Name = name
	} else {
		clone.Name = strings.TrimSpace(source.Name + " (copy)")
	}

	copiedKeys := make([]string, 0, 3)
	copySecret := func(key string) (bool, error) {
		value, ok, err := s.rt.Secrets.Get(source.ID, key)
		if err != nil || !ok {
			return false, err
		}
		if err := s.rt.Secrets.Set(id, key, value); err != nil {
			return false, err
		}
		copiedKeys = append(copiedKeys, key)
		return true, nil
	}
	rollback := func() {
		for _, key := range copiedKeys {
			_ = s.rt.Secrets.Delete(id, key)
		}
	}

	hasPassword := false
	hasPassphrase := false
	if source.AuthMethod == config.AuthPassword {
		hasPassword, err = copySecret(secret.KeyPassword)
	} else if source.AuthMethod == config.AuthPrivateKey {
		hasPassphrase, err = copySecret(secret.KeyPrivatePassphrase)
	}
	if err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	hasToken, err := copySecret(secret.KeySecurityToken)
	if err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if source.IsLocal() {
		hasPassword = false
		hasPassphrase = false
	}
	clone.HasPassword = hasPassword
	clone.HasPrivateKeyPassphrase = hasPassphrase
	clone.HasSecurityToken = hasToken
	if err := clone.Validate(); err != nil {
		rollback()
		writeProfileSaveError(w, err)
		return
	}
	if err := s.rt.Profiles.Save(clone); err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.reviveProfile(clone.ID)
	writeJSON(w, clone, nil)
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
			writeProfileSaveError(w, err)
			return
		}
		writeJSON(w, req.Profile, nil)
	case http.MethodDelete:
		existing, err := s.rt.Profiles.Get(id)
		if err != nil {
			if errors.Is(err, config.ErrProfileNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		id = existing.ID
		s.mu.Lock()
		session := s.sessionForProfileLocked(id)
		if s.retiredProfiles == nil {
			s.retiredProfiles = map[string]bool{}
		}
		s.retiredProfiles[id] = true
		delete(s.pendingStart, id)
		delete(s.lastFailure, id)
		s.mu.Unlock()

		if session != nil {
			s.stopSessionResources(session, true)
		}
		if err := s.rt.Profiles.Delete(id); err != nil {
			// The profile is still stored, so it must remain startable; keeping
			// the retired marker would turn every later start into a silent no-op.
			s.reviveProfile(id)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.shells.closeProfile(id)
		_ = s.rt.Secrets.Delete(id, secret.KeyPassword)
		_ = s.rt.Secrets.Delete(id, secret.KeyPrivatePassphrase)
		_ = s.rt.Secrets.Delete(id, secret.KeySecurityToken)
		if s.profileLogs != nil {
			s.profileLogs.delete(id)
		}
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
		ID             string `json:"id"`
		AcceptHostKey  bool   `json:"accept_host_key"`
		ReplaceHostKey bool   `json:"replace_host_key"`
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
	if s.profileRetired(p.ID) {
		writeError(w, http.StatusConflict, errors.New("profile was deleted"))
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
	s.logProfileWarnings(p)
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

	policy := hostKeyPolicyForRequest(req.AcceptHostKey, req.ReplaceHostKey)

	client, err := sshclient.DialWithHostKeyPolicy(p, s.rt.Secrets, policy)
	if err != nil {
		s.profileLog(p.ID, "SSH connection failed for "+p.Name+": "+err.Error())
		writeSSHError(w, err)
		return
	}
	s.profileLog(p.ID, "SSH connection OK for "+p.Name)

	// Continue asynchronously after host-key verification so the frontend can
	// receive and confirm unknown fingerprints before startup is queued.
	s.markStartPending(p.ID)
	go s.startRemoteSessionAsync(p, client, sessionLog)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) startRemoteSessionAsync(p config.Profile, client *ssh.Client, sessionLog io.Writer) {
	defer s.clearStartPending(p.ID)

	var proxyChannel *sshclient.LocalProxyChannel
	sessionStarted := false
	defer func() {
		if sessionStarted {
			return
		}
		if proxyChannel != nil {
			proxyChannel.Close()
			_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
		}
		_ = client.Close()
	}()

	if s.profileRetired(p.ID) {
		return
	}

	proxyChannel, proxyErrText := s.startSessionProxyChannel(p, client)
	var proxyEnv []config.EnvPair
	if proxyChannel != nil {
		proxyEnv = proxyChannel.Env
	}

	check, err := sshclient.RunCheckPortCommand(client, p, proxyEnv, sessionLog, sessionLog)
	if err != nil {
		s.profileLog(p.ID, "ChemSSH port check failed for "+p.Name+": "+err.Error())
		s.recordSessionFailure(p.ID, "ChemSSH port check failed: "+err.Error())
		return
	}
	if check.Reusable {
		s.profileLog(p.ID, "remote ChemSSH is reusable; starting tunnel without launching another server")
	} else {
		s.profileLog(p.ID, "remote ChemSSH port is available; starting configured command")
	}

	s.mu.Lock()
	if s.retiredProfiles[p.ID] {
		s.mu.Unlock()
		return
	}
	if s.sessionForProfileLocked(p.ID) != nil {
		s.mu.Unlock()
		s.profileLog(p.ID, "a session is already running")
		return
	}

	var process *sshclient.RemoteProcess
	if !check.Reusable {
		process, err = sshclient.StartRemoteCommand(client, p, proxyEnv, sessionLog, sessionLog)
		if err != nil {
			s.mu.Unlock()
			s.profileLog(p.ID, "failed to start remote command: "+err.Error())
			s.recordSessionFailure(p.ID, "failed to start remote command: "+err.Error())
			return
		}
	}
	tunnel, err := sshclient.StartTunnel(context.Background(), client, p)
	if err != nil {
		if process != nil {
			_ = process.Stop()
		}
		s.mu.Unlock()
		s.profileLog(p.ID, "failed to start tunnel: "+err.Error())
		s.recordSessionFailure(p.ID, "failed to start tunnel: "+err.Error())
		return
	}

	healthCtx, healthStop := context.WithCancel(context.Background())
	session := &activeSession{
		id:           p.ID,
		name:         p.Name,
		profile:      p,
		client:       client,
		process:      process,
		tunnel:       tunnel,
		proxyChannel: proxyChannel,
		proxyErr:     proxyErrText,
		healthStop:   healthStop,
		forwarding:   true,
	}
	s.setSessionLocked(session)
	// Mark the session as owned before releasing the lock; a concurrent stop can
	// otherwise clean up the channel while the deferred cleanup still thinks the
	// start failed and closes it a second time.
	sessionStarted = true
	s.mu.Unlock()

	s.profileLog(p.ID, "starting "+p.Name+" at "+p.BrowserURL())
	if process != nil {
		go s.watchRemoteProcess(session)
	}
	go s.waitForRemoteHealth(session, p, healthCtx)
}

func (s *Server) startSessionProxyChannel(p config.Profile, client *ssh.Client) (*sshclient.LocalProxyChannel, string) {
	if !p.ProxyActive() {
		return nil, ""
	}
	channel, err := sshclient.StartLocalProxyChannel(context.Background(), client, p)
	if err != nil {
		message := "local network channel unavailable; starting without it: " + err.Error()
		s.profileLog(p.ID, "warning: "+message)
		return nil, message
	}
	s.profileLog(p.ID, "local network channel ready: "+channel.ProxyURL+" on the remote loopback for "+p.LocalProxy.Normalized().Address())
	if channel.BridgeErr != nil {
		s.profileLog(p.ID, "warning: ChemSSH terminal proxy bridge not written; remote commands and launcher terminals still receive the proxy environment: "+channel.BridgeErr.Error())
	}
	return channel, ""
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
	if s.retiredProfiles[p.ID] {
		s.mu.Unlock()
		healthStop()
		stopLocalProcess(cmd)
		writeError(w, http.StatusConflict, errors.New("profile was deleted"))
		return
	}
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
	s.profileLog(p.ID, "local ChemSSH forwarding restored for "+p.BrowserURL())
	return true, nil
}

func (s *Server) waitForLocalHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := s.waitForProfileHealth(ctx, p, 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "local health check failed: "+err.Error())
		s.recordSessionFailure(p.ID, "local health check failed: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "local health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshLocalIdentity(session, p)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
}

func (s *Server) localHealthReady(p config.Profile, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return netcheck.WaitForURL(ctx, s.healthURLWithToken(p), timeout, timeout) == nil
}

func startLocalCommand(p config.Profile, stdout, stderr io.Writer) (*exec.Cmd, error) {
	command := sshclient.AssembleLocalCommand(p)
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
	needsProxyChannel := p.ProxyActive() && session.proxyChannel == nil
	s.mu.Unlock()

	s.profileLog(p.ID, "service is already running; restarting forwarding only for "+p.Name)
	var proxyChannel *sshclient.LocalProxyChannel
	var proxyErrText string
	if needsProxyChannel {
		proxyChannel, proxyErrText = s.startSessionProxyChannel(p, client)
	}
	tunnel, err := sshclient.StartTunnel(context.Background(), client, p)
	if err != nil {
		if proxyChannel != nil {
			proxyChannel.Close()
			_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
		}
		return true, err
	}
	healthCtx, healthStop := context.WithCancel(context.Background())

	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) || session.forwarding {
		s.mu.Unlock()
		healthStop()
		_ = tunnel.Close()
		if proxyChannel != nil {
			proxyChannel.Close()
			_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
		}
		return true, errors.New("a session is already running")
	}
	session.profile = p
	session.tunnel = tunnel
	if proxyChannel != nil {
		session.proxyChannel = proxyChannel
		session.proxyErr = proxyErrText
	} else if proxyErrText != "" {
		session.proxyErr = proxyErrText
	}
	session.healthStop = healthStop
	session.forwarding = true
	session.ready = false
	s.assignSessionOpenSeqLocked(session)
	s.promoteProxySessionLocked(session)
	s.mu.Unlock()

	go s.waitForRestartedForwardingHealth(session, p, healthCtx)
	s.profileLog(p.ID, "forwarding restarted for "+p.BrowserURL())
	return true, nil
}

func (s *Server) waitForRestartedForwardingHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := s.waitForProfileHealth(ctx, p, 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "health check failed after forwarding restart: "+err.Error())
		s.recordSessionFailure(p.ID, "health check failed after forwarding restart: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshRemoteIdentity(session, p)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
}

func (s *Server) waitForRemoteHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := s.waitForProfileHealth(ctx, p, 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.profileLog(p.ID, "health check failed: "+err.Error())
		s.recordSessionFailure(p.ID, "health check failed: "+err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.profileLog(p.ID, "health check OK: "+p.HealthURL())
	s.markSessionReady(session)
	s.refreshRemoteIdentity(session, p)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
}

// markStartPending flags a profile as starting and drops the previous failure.
// It is called before the asynchronous start goroutine is spawned.
func (s *Server) markStartPending(profileID string) {
	if profileID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiredProfiles[profileID] {
		return
	}
	if s.pendingStart == nil {
		s.pendingStart = map[string]bool{}
	}
	s.pendingStart[profileID] = true
	delete(s.lastFailure, profileID)
}

// clearStartPending clears the starting flag once the start attempt has
// finished, whether it succeeded or failed.
func (s *Server) clearStartPending(profileID string) {
	if profileID == "" {
		return
	}
	s.mu.Lock()
	delete(s.pendingStart, profileID)
	retired := s.retiredProfiles[profileID]
	s.mu.Unlock()
	if retired && s.profileLogs != nil {
		// A start attempt that was in flight during deletion may have written
		// log lines after the delete handler cleared them.
		s.profileLogs.delete(profileID)
	}
}

// recordSessionFailure stores the reason a start attempt failed. The record is
// keyed by profile ID and survives session teardown so status polling can still
// report the failure.
func (s *Server) recordSessionFailure(profileID, message string) {
	if profileID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiredProfiles[profileID] {
		return
	}
	if s.lastFailure == nil {
		s.lastFailure = map[string]sessionFailure{}
	}
	s.lastFailure[profileID] = sessionFailure{Message: message, At: time.Now()}
}

func (s *Server) markSessionReady(session *activeSession) {
	s.mu.Lock()
	if s.sessionStillActiveLocked(session) && session.forwarding {
		session.ready = true
		delete(s.lastFailure, session.id)
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
	identity, err := chemssh.FetchIdentity(ctx, client, p, s.rt.Secrets)
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
	identity, err := chemssh.FetchLocalIdentity(ctx, p, s.rt.Secrets)
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
	case "api", "assets", "launcher-logs", "shell", "sftp", "static", "terminal":
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

const chemsshProxyMaxRetries = 3
const chemsshProxyRetryBase = 200 * time.Millisecond
const chemsshBridgeSFTPWarmDelay = 2 * time.Second

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

	// 智能重试：API 请求使用更少的重试次数
	maxRetries := chemsshProxyMaxRetries
	if !wantsHTML(r) {
		maxRetries = 2 // API 请求只重试 2 次，减少延迟
	}

	var lastErr error
	for attempt := range maxRetries {
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
	s.writeProxyError(w, r, http.StatusBadGateway, fmt.Errorf("proxy ChemSSH request failed after %d retries: %w", maxRetries, lastErr))
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
<meta name="color-scheme" content="light">
<title>ChemSSH - Loading</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
html,body{background:#fff}
body{display:flex;align-items:center;justify-content:center;min-height:100vh;
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;color:#333}
.card{text-align:center;padding:48px 64px;background:#fff;border:1px solid rgba(0,0,0,.06);border-radius:12px;
  box-shadow:0 2px 12px rgba(0,0,0,.06)}
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
	s.applyLauncherClientIdentity(req)
	s.applySecurityToken(req, session)
	return req
}

func (s *Server) applyLauncherClientIdentity(req *http.Request) {
	clientID := s.launcherClientID()
	if clientID == "" {
		return
	}
	req.Header.Set("X-ChemSSH-Client-Id", clientID)
	if isTerminalWebSocketPath(req.URL.Path) {
		query := req.URL.Query()
		query.Set("client_id", clientID)
		req.URL.RawQuery = query.Encode()
	}
}

func (s *Server) applySecurityToken(req *http.Request, session *activeSession) {
	if session == nil || s.rt == nil || s.rt.Secrets == nil {
		return
	}
	token, ok, err := s.rt.Secrets.Get(session.profile.ID, secret.KeySecurityToken)
	if err != nil || !ok || token == "" {
		return
	}
	req.Header.Set("X-ChemSSH-Token", token)
	if isTerminalWebSocketPath(req.URL.Path) {
		query := req.URL.Query()
		query.Set("token", token)
		req.URL.RawQuery = query.Encode()
	}
}

func (s *Server) secretStore() secret.Store {
	if s.rt == nil {
		return nil
	}
	return s.rt.Secrets
}

func (s *Server) healthURLWithToken(p config.Profile) string {
	return health.URLWithToken(p, s.secretStore())
}

func (s *Server) waitForProfileHealth(ctx context.Context, p config.Profile, timeout, interval time.Duration) error {
	return health.WaitForProfileHealth(ctx, p, s.secretStore(), timeout, interval)
}

func isTerminalWebSocketPath(path string) bool {
	return path == "/api/terminal/ws" || strings.HasPrefix(path, "/api/terminal/ws/")
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
		// ChemSSH compresses responses with Brotli when the client advertises
		// "br" (see its BrotliMiddleware), and the launcher cannot decode
		// brotli while rewriting proxied documents (see decodeBody). Document
		// navigations therefore ask for gzip; other requests keep the
		// browser's Accept-Encoding so assets still get brotli.
		if strings.Contains(req.Header.Get("Accept"), "text/html") {
			req.Header.Set("Accept-Encoding", "gzip")
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, fmt.Errorf("proxy ChemSSH request failed: %w", err))
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		location := resp.Header.Get("Location")
		if location != "" {
			rewritten := rewriteChemSSHLocationHeader(location, target, session)
			if rewritten != "" {
				resp.Header.Set("Location", rewritten)
			}
		}
		// Inject the launcher client scripts into proxied HTML:
		// 1. a self-healing guard for transient dynamic-import failures, and
		// 2. the launcher API token shim, so the ChemSSH page can call launcher
		//    bridge endpoints whose writes require the per-launch token.
		// Also refresh the launcher API token cookie so the shim reads the
		// current value even before any launcher API request was made.
		injectChemSSHClientScripts(resp, s.apiToken)
		return nil
	}
	return proxy
}

// chemsshImportReloaderScript is injected into the <head> of proxied ChemSSH
// HTML documents. It listens for window "error" events that indicate a failed
// dynamic import() (Vite/ESM surfaces these as uncaught TypeErrors whose
// message contains "Failed to fetch dynamically imported module") and reloads
// the page exactly once, so a settling-upstream blip self-heals instead of
// stranding the user on a blank page.
const chemsshImportReloaderScript = `<script>(function(){if(sessionStorage.getItem('__chemsshReloaded')){sessionStorage.removeItem('__chemsshReloaded');return;}var reloaded=false;function isImportError(ev){var msg=(ev&&ev.message)||'';if(/Failed to fetch dynamically imported module/.test(msg))return true;if(ev&&ev.error&&/Failed to fetch dynamically imported module/.test(ev.error.message||''))return true;return false;}window.addEventListener('error',function(ev){if(reloaded)return;if(!isImportError(ev))return;reloaded=true;try{sessionStorage.setItem('__chemsshReloaded','1');}catch(e){}try{window.location.reload();}catch(e){}},{capture:true});window.addEventListener('unhandledrejection',function(ev){if(reloaded)return;var msg=(ev&&ev.reason&&(ev.reason.message||ev.reason))||'';if(/Failed to fetch dynamically imported module/.test(String(msg))){reloaded=true;try{sessionStorage.setItem('__chemsshReloaded','1');}catch(e){}try{window.location.reload();}catch(e){}}},{capture:true});})();</script>`

// chemsshLauncherTokenScript is injected into the <head> of proxied ChemSSH
// HTML documents, ahead of the ChemSSH application scripts. Launcher GUI API
// writes require both the per-launch token cookie and the matching
// X-ChemSSH-Launcher-Token header; the launcher's own frontend supplies the
// header itself, but a proxied ChemSSH page has no knowledge of the token and
// would be rejected with 403. This shim fills the header in from the cookie for
// same-origin requests to the launcher bridge namespace only, so the token is
// never forwarded to the remote ChemSSH backend.
const chemsshLauncherTokenScript = `<script>(function(){
var C='chemssh_launcher_token',H='X-ChemSSH-Launcher-Token',P='/api/chemssh-bridge/';
function token(){var parts=document.cookie?document.cookie.split(';'):[];for(var i=0;i<parts.length;i++){var item=parts[i].trim();if(item.indexOf(C+'=')===0){return decodeURIComponent(item.slice(C.length+1));}}return '';}
function origin(){try{return window.location.origin||(window.location.protocol+'//'+window.location.host);}catch(e){return '';}}
function inScope(url){try{var target=new URL(url,window.location.href);return target.origin===origin()&&target.pathname.indexOf(P)===0;}catch(e){return false;}}
var originalFetch=window.fetch;
if(typeof originalFetch!=='function'){return;}
window.fetch=function(input,init){
try{
var url=typeof input==='string'?input:(input&&input.url)||'';
var value=token();
if(value&&inScope(url)){
var headers=new Headers((init&&init.headers)||(input&&input.headers)||undefined);
if(!headers.has(H)){headers.set(H,value);var next=Object.assign({},init||{});next.headers=headers;return originalFetch.call(this,input,next);}
}
}catch(e){}
return originalFetch.call(this,input,init);
};
})();</script>`

// injectChemSSHClientScripts inserts the launcher client scripts right after
// the opening <head> tag of a proxied ChemSSH HTML document: the
// dynamic-import self-healing guard plus the launcher API token shim. It also
// refreshes the launcher API token cookie on the document so the shim can read
// the current per-launch token even before the first launcher API response
// arrives (apiHandler sets the same cookie on every API request).
func injectChemSSHClientScripts(resp *http.Response, apiToken string) {
	if !isChemSSHHTMLResponse(resp) {
		return
	}
	if apiToken != "" {
		resp.Header.Add("Set-Cookie", (&http.Cookie{
			Name:     launcherAPITokenCookie,
			Value:    apiToken,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
		}).String())
	}
	injectChemSSHHTML(resp, chemsshImportReloaderScript+chemsshLauncherTokenScript)
}

// isChemSSHHTMLResponse reports whether the proxied response is an HTML
// document the launcher may rewrite.
func isChemSSHHTMLResponse(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	return strings.Contains(resp.Header.Get("Content-Type"), "text/html")
}

// injectChemSSHHTML inserts snippet right after the opening <head> tag of an
// HTML response. Non-HTML responses, compressed encodings we cannot safely
// decompress, and responses without a readable body are passed through
// untouched. It recomputes Content-Length and strips Content-Encoding so the
// browser does not see a truncated body (which would itself look like a failed
// module load).
func injectChemSSHHTML(resp *http.Response, snippet string) {
	if !isChemSSHHTMLResponse(resp) {
		return
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}

	body, err := decodeBody(raw, resp.Header.Get("Content-Encoding"))
	if err != nil {
		// Cannot safely decompress — pass the original bytes through untouched.
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	injected := injectAfterHeadOpen(body, snippet)
	resp.Body = io.NopCloser(bytes.NewReader(injected))
	resp.ContentLength = int64(len(injected))
	resp.Header.Set("Content-Length", strconv.Itoa(len(injected)))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Transfer-Encoding")
}

// decodeBody reverses common HTTP content encodings so the HTML can be
// inspected and rewritten. Identity (or empty) and gzip/deflate are supported;
// anything else (brotli, zstd, ...) returns an error so the caller leaves the
// body untouched rather than corrupting it.
func decodeBody(raw []byte, encoding string) ([]byte, error) {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	switch encoding {
	case "", "identity":
		return raw, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case "deflate":
		return io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	default:
		return nil, errors.New("unsupported content encoding: " + encoding)
	}
}

// injectAfterHeadOpen inserts snippet immediately after the first <head ...>
// opening tag in body. If no <head> tag is present, the snippet is prepended.
func injectAfterHeadOpen(body []byte, snippet string) []byte {
	headIdx := bytes.Index(bytes.ToLower(body), []byte("<head"))
	if headIdx < 0 {
		return append([]byte(snippet), body...)
	}
	closeIdx := bytes.IndexByte(body[headIdx:], '>')
	if closeIdx < 0 {
		return append([]byte(snippet), body...)
	}
	insertAt := headIdx + closeIdx + 1
	out := make([]byte, 0, len(body)+len(snippet))
	out = append(out, body[:insertAt]...)
	out = append(out, []byte(snippet)...)
	out = append(out, body[insertAt:]...)
	return out
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
		ID             string `json:"id"`
		AcceptHostKey  bool   `json:"accept_host_key"`
		ReplaceHostKey bool   `json:"replace_host_key"`
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
		if err := s.waitForProfileHealth(r.Context(), p, 3*time.Second, 500*time.Millisecond); err != nil {
			s.profileLog(p.ID, "local ChemSSH test failed for "+p.Name+": "+err.Error())
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.profileLog(p.ID, "local ChemSSH test OK for "+p.BrowserURL())
		writeJSON(w, map[string]bool{"ok": true}, nil)
		return
	}
	s.profileLog(p.ID, "testing SSH for "+p.Name)
	policy := hostKeyPolicyForRequest(req.AcceptHostKey, req.ReplaceHostKey)
	client, err := sshclient.DialWithHostKeyPolicy(p, s.rt.Secrets, policy)
	if err != nil {
		s.profileLog(p.ID, "SSH connection failed for "+p.Name+": "+err.Error())
		writeSSHError(w, err)
		return
	}
	s.profileLog(p.ID, "SSH connection OK for "+p.Name)
	testLog := s.profileLogWriter(p.ID)
	if _, err := sshclient.RunCheckPortCommand(client, p, nil, testLog, testLog); err != nil {
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
	proxyChannel := session.proxyChannel
	client := session.client
	healthStop := session.healthStop
	session.tunnel = nil
	session.proxyChannel = nil
	session.proxyErr = ""
	session.client = nil
	session.healthStop = nil
	session.forwarding = false
	session.ready = false
	s.mu.Unlock()

	s.closeChemSSHBridgeSessionForProfile(session.id)
	s.sessionLog(session, "remote command stopped with error: "+err.Error())
	s.recordSessionFailure(session.id, "remote command stopped with error: "+err.Error())
	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if proxyChannel != nil {
		proxyChannel.Close()
		_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
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
	s.recordSessionFailure(session.id, "local ChemSSH command stopped with error: "+err.Error())
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
	var proxyChannel *sshclient.LocalProxyChannel
	if clearSession {
		proxyChannel = session.proxyChannel
		session.proxyChannel = nil
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
	if proxyChannel != nil {
		proxyChannel.Close()
		_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
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
		identity, err := chemssh.FetchIdentity(ctx, client, session.profile, s.rt.Secrets)
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
	proxyChannel := session.proxyChannel
	process := session.process
	localProcess := session.localProcess
	client := session.client
	healthStop := session.healthStop
	profile := session.profile
	session.tunnel = nil
	session.proxyChannel = nil
	session.proxyErr = ""
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
	if proxyChannel != nil {
		proxyChannel.Close()
		_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
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
			identity, err := chemssh.FetchIdentity(ctx, client, profile, s.rt.Secrets)
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

func (s *Server) stopAllLocalLauncherResources() {
	s.mu.Lock()
	sessions := make([]*activeSession, 0, len(s.sessions))
	seen := map[*activeSession]struct{}{}
	for _, session := range s.sessions {
		if session == nil {
			continue
		}
		if _, ok := seen[session]; ok {
			continue
		}
		seen[session] = struct{}{}
		sessions = append(sessions, session)
	}
	if s.session != nil {
		if _, ok := seen[s.session]; !ok {
			sessions = append(sessions, s.session)
		}
	}
	s.mu.Unlock()

	for _, session := range sessions {
		s.stopLocalLauncherResources(session)
	}
}

func (s *Server) stopLocalLauncherResources(session *activeSession) {
	s.mu.Lock()
	if !s.sessionStillActiveLocked(session) {
		s.mu.Unlock()
		return
	}
	s.removeSessionLocked(session)
	session.stopping = true
	tunnel := session.tunnel
	proxyChannel := session.proxyChannel
	client := session.client
	localProcess := session.localProcess
	healthStop := session.healthStop
	profile := session.profile
	session.tunnel = nil
	session.proxyChannel = nil
	session.proxyErr = ""
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
	if proxyChannel != nil {
		proxyChannel.Close()
		_ = sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath)
	}
	if profile.IsLocal() && localProcess != nil {
		stopLocalProcess(localProcess)
		s.profileLog(profile.ID, "local ChemSSH command stopped")
	}
	s.profileLog(profile.ID, "launcher resources stopped; remote ChemSSH service was left untouched")
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

// handleActivate makes one session the one the local proxy serves. Promotion is
// a write, so it lives in its own endpoint instead of piggybacking on the status
// poll that runs every couple of seconds.
func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
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
	activated := false
	if session != nil && session.forwarding {
		s.promoteProxySessionLocked(session)
		activated = true
	}
	id := ""
	openSeq := uint64(0)
	if session != nil {
		id = session.id
		openSeq = session.openSeq
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "activated": activated, "id": id, "open_seq": openSeq}, nil)
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
	statusProfileID := profileID
	if statusProfileID == "" && session != nil {
		statusProfileID = session.id
	}
	pendingStart := false
	var lastFailure *sessionFailure
	if statusProfileID != "" {
		pendingStart = s.pendingStart[statusProfileID]
		lastFailure = sessionFailurePtr(s.lastFailure, statusProfileID)
	}
	payload := sessionStatusPayload(session, pendingStart, lastFailure)
	id := ""
	openSeq := uint64(0)
	openBrowser := false
	forwarding := false
	profile := config.Profile{}
	if session != nil {
		id = session.id
		openSeq = session.openSeq
		openBrowser = session.profile.OpenBrowser
		forwarding = session.forwarding
		profile = session.profile
	}
	s.mu.Unlock()

	url := ""
	if forwarding {
		url = s.sessionProxyURL(profile, openSeq)
	}
	payload["id"] = id
	payload["url"] = url
	payload["open_browser"] = openBrowser
	payload["pending_start"] = pendingStart
	writeJSON(w, payload, nil)
}

func sessionStatusPayload(session *activeSession, pendingStart bool, failure *sessionFailure) map[string]any {
	running := session != nil
	forwarding := false
	ready := false
	name := ""
	openSeq := uint64(0)
	localProxy := map[string]any{
		"enabled":   false,
		"active":    false,
		"proxy_url": "",
		"bridge":    false,
		"error":     "",
	}
	if running {
		forwarding = session.forwarding
		ready = session.forwarding && session.ready
		name = session.name
		openSeq = session.openSeq
		localProxy["enabled"] = session.profile.ProxyActive()
		localProxy["error"] = session.proxyErr
		if session.proxyChannel != nil {
			localProxy["active"] = true
			localProxy["proxy_url"] = session.proxyChannel.ProxyURL
			localProxy["bridge"] = session.proxyChannel.BridgePath != ""
		}
	}
	return map[string]any{
		"running":      running,
		"forwarding":   forwarding,
		"name":         name,
		"open_seq":     openSeq,
		"ready":        ready,
		"phase":        deriveSessionPhase(running, forwarding, ready, pendingStart, failure != nil),
		"last_failure": failure,
		"local_proxy":  localProxy,
	}
}

// deriveSessionPhase collapses the session and per-profile start records into
// the single phase the UI renders. A session that still exists but has stopped
// forwarding reports paused rather than stopped, because its remote command may
// still be running.
func deriveSessionPhase(running, forwarding, ready, pendingStart, hasFailure bool) string {
	switch {
	case running && forwarding && ready:
		return "ready"
	case running && forwarding:
		return "starting"
	case running:
		return "paused"
	case pendingStart:
		return "starting"
	case hasFailure:
		return "failed"
	default:
		return "stopped"
	}
}

func sessionFailurePtr(failures map[string]sessionFailure, profileID string) *sessionFailure {
	failure, ok := failures[profileID]
	if !ok {
		return nil
	}
	record := failure
	return &record
}

// handleStatuses reports every profile that has a session or a start record, so
// the launcher can poll all of them with one request instead of one per profile.
func (s *Server) handleStatuses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.mu.Lock()
	statuses := make(map[string]map[string]any, len(s.sessions)+len(s.pendingStart)+len(s.lastFailure))
	for id, session := range s.sessions {
		statuses[id] = sessionStatusPayload(session, s.pendingStart[id], sessionFailurePtr(s.lastFailure, id))
	}
	for id := range s.pendingStart {
		if _, ok := statuses[id]; ok {
			continue
		}
		statuses[id] = sessionStatusPayload(nil, true, sessionFailurePtr(s.lastFailure, id))
	}
	for id := range s.lastFailure {
		if _, ok := statuses[id]; ok {
			continue
		}
		statuses[id] = sessionStatusPayload(nil, false, sessionFailurePtr(s.lastFailure, id))
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"statuses": statuses}, nil)
}

type browserClientRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleBrowserClientHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	id := browserClientIDFromRequest(r)
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing browser client id"))
		return
	}
	s.noteBrowserClient(id, time.Now())
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleBrowserClientClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	id := browserClientIDFromRequest(r)
	if id != "" {
		s.forgetBrowserClient(id, time.Now())
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) noteBrowserClient(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.browserClients == nil {
		s.browserClients = map[string]time.Time{}
	}
	s.browserClients[id] = now
	s.browserClientSeen = true
	s.browserClientsEmptySince = time.Time{}
}

func (s *Server) forgetBrowserClient(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.browserClients, id)
	if s.browserClientSeen && len(s.browserClients) == 0 && s.browserClientsEmptySince.IsZero() {
		s.browserClientsEmptySince = now
	}
}

func (s *Server) startBrowserClientWatchdog(ctx context.Context) {
	ticker := time.NewTicker(browserClientCheckInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if s.browserClientsGoneLongEnough(now) {
					s.shutdownAfterBrowserClientsGone()
					return
				}
			}
		}
	}()
}

func (s *Server) browserClientsGoneLongEnough(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, seenAt := range s.browserClients {
		if now.Sub(seenAt) > browserClientStaleAfter {
			delete(s.browserClients, id)
		}
	}
	if !s.browserClientSeen || len(s.browserClients) > 0 {
		s.browserClientsEmptySince = time.Time{}
		return false
	}
	if s.browserClientsEmptySince.IsZero() {
		s.browserClientsEmptySince = now
		return false
	}
	return now.Sub(s.browserClientsEmptySince) >= browserClientGoneGrace
}

func (s *Server) shutdownAfterBrowserClientsGone() {
	s.browserShutdownOnce.Do(func() {
		s.localLog.add("browser launcher page closed; stopping local launcher resources")
		s.stopAllLocalLauncherResources()
		s.sftp.CloseAll()
		s.sftpSync.CloseAll()
		s.shells.closeAll()
		s.closeChemSSHBridgeSession()
		if s.shutdown != nil {
			s.shutdown()
		}
	})
}

func browserClientIDFromRequest(r *http.Request) string {
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		return id
	}
	if r.Body == nil {
		return ""
	}
	var req browserClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return ""
	}
	return strings.TrimSpace(req.ID)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	profileID := strings.TrimSpace(r.URL.Query().Get("profile_id"))
	if profileID != "" && s.profileLogs != nil {
		writeJSON(w, map[string][]string{"lines": s.profileLogs.snapshot(profileID)}, nil)
		return
	}
	writeJSON(w, map[string][]string{"lines": s.logs.snapshot()}, nil)
}

func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	profileID := strings.TrimSpace(r.URL.Query().Get("profile_id"))

	// Get the appropriate log
	var log *safeLog
	if profileID != "" && s.profileLogs != nil {
		log = s.profileLogs.forProfile(profileID)
	} else {
		log = s.logs
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send existing logs first
	existingLines := log.snapshot()
	for _, line := range existingLines {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	// Mark the end of the replay so the client can swap the whole buffer in one
	// step instead of appending the replay on top of what it already shows.
	fmt.Fprint(w, "event: replay-end\ndata: {}\n\n")
	flusher.Flush()

	// Create channel for new log lines
	ch := make(chan string, 10)
	log.addListener(ch)
	defer log.removeListener(ch)
	defer close(ch)

	// Stream new logs
	ctx := r.Context()
	for {
		select {
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
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

func (s *Server) handleUISettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, err := config.LoadUISettings()
		writeJSON(w, settings, err)
	case http.MethodPost:
		var payload struct {
			Language           *string `json:"language"`
			TerminalFontSize   *int    `json:"terminal_font_size"`
			TerminalScrollback *int    `json:"terminal_scrollback"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		settings, err := config.LoadUISettings()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		languageChanged := ""
		if payload.Language != nil {
			language, ok := config.NormalizeUILanguage(*payload.Language)
			if !ok {
				writeError(w, http.StatusBadRequest, errors.New("unsupported UI language"))
				return
			}
			settings.Language = language
			languageChanged = language
		}
		if payload.TerminalFontSize != nil {
			if *payload.TerminalFontSize < config.MinTerminalFontSize || *payload.TerminalFontSize > config.MaxTerminalFontSize {
				writeError(w, http.StatusBadRequest, fmt.Errorf("terminal font size must be between %d and %d", config.MinTerminalFontSize, config.MaxTerminalFontSize))
				return
			}
			settings.TerminalFontSize = *payload.TerminalFontSize
		}
		if payload.TerminalScrollback != nil {
			if *payload.TerminalScrollback < config.MinTerminalScrollback || *payload.TerminalScrollback > config.MaxTerminalScrollback {
				writeError(w, http.StatusBadRequest, fmt.Errorf("terminal scrollback must be between %d and %d", config.MinTerminalScrollback, config.MaxTerminalScrollback))
				return
			}
			settings.TerminalScrollback = *payload.TerminalScrollback
		}
		if err := config.SaveUISettings(settings); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if languageChanged != "" {
			s.localLog.add("UI language set to " + languageChanged)
		}
		if payload.TerminalFontSize != nil || payload.TerminalScrollback != nil {
			s.localLog.add(fmt.Sprintf("terminal display settings updated: font %dpx, scrollback %d lines", settings.TerminalFontSize, settings.TerminalScrollback))
		}
		writeJSON(w, settings, nil)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
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
		if err := profile.Validate(); err != nil {
			writeProfileSaveError(w, err)
			return
		}
	}
	for _, profile := range profiles {
		_ = s.rt.Secrets.Delete(profile.ID, secret.KeyPassword)
		_ = s.rt.Secrets.Delete(profile.ID, secret.KeyPrivatePassphrase)
		_ = s.rt.Secrets.Delete(profile.ID, secret.KeySecurityToken)
		if err := s.rt.Profiles.Save(profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.reviveProfile(profile.ID)
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
	PasswordAction      string `json:"password_action"`
	Password            string `json:"password"`
	PassphraseAction    string `json:"passphrase_action"`
	Passphrase          string `json:"passphrase"`
	SecurityTokenAction string `json:"security_token_action"`
	SecurityToken       string `json:"security_token"`
}

func (s *Server) saveProfileWithSecrets(p config.Profile, req secretRequest) error {
	if err := p.Validate(); err != nil {
		return err
	}
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
	} else {
		if p.AuthMethod == config.AuthPassword {
			switch req.PasswordAction {
			case "replace":
				if req.Password == "" {
					// The editor shows an empty field as "leave unchanged", and an
					// empty secret is unusable for SSH. Never clobber a stored
					// password with an empty value; require "clear" to remove it.
					break
				}
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
			}
			_ = s.rt.Secrets.Delete(p.ID, secret.KeyPrivatePassphrase)
			p.HasPrivateKeyPassphrase = false
			p.PrivateKeyPath = ""
		}
		if p.AuthMethod == config.AuthPrivateKey {
			switch req.PassphraseAction {
			case "replace":
				if req.Passphrase == "" {
					break
				}
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
			}
			_ = s.rt.Secrets.Delete(p.ID, secret.KeyPassword)
			p.HasPassword = false
		}
	}
	switch req.SecurityTokenAction {
	case "replace":
		if req.SecurityToken == "" {
			break
		}
		started := time.Now()
		if err := s.rt.Secrets.Set(p.ID, secret.KeySecurityToken, req.SecurityToken); err != nil {
			return err
		}
		s.profileLog(p.ID, "security token saved to secret store in "+time.Since(started).Round(time.Millisecond).String())
		p.HasSecurityToken = true
	case "clear":
		started := time.Now()
		if err := s.rt.Secrets.Delete(p.ID, secret.KeySecurityToken); err != nil {
			return err
		}
		s.profileLog(p.ID, "security token cleared from secret store in "+time.Since(started).Round(time.Millisecond).String())
		p.HasSecurityToken = false
	case "keep":
	}
	s.syncSecretFlags(&p)
	if err := s.rt.Profiles.Save(p); err != nil {
		return err
	}
	s.reviveProfile(p.ID)
	s.logProfileWarnings(p)
	return nil
}

// syncSecretFlags makes the cached "has secret" flags match the secret store. A
// flag without a usable secret makes the editor claim "已保存" while
// authentication can never succeed, and a secret without the flag hides a
// working credential. Read failures keep the incoming flags, since the store may
// only be temporarily unavailable.
func (s *Server) syncSecretFlags(p *config.Profile) {
	if !p.IsLocal() {
		switch p.AuthMethod {
		case config.AuthPassword:
			p.HasPassword = s.alignSecretFlag(p.ID, secret.KeyPassword, p.HasPassword, "password")
			p.HasPrivateKeyPassphrase = false
		case config.AuthPrivateKey:
			p.HasPrivateKeyPassphrase = s.alignSecretFlag(p.ID, secret.KeyPrivatePassphrase, p.HasPrivateKeyPassphrase, "private key passphrase")
			p.HasPassword = false
		}
	}
	p.HasSecurityToken = s.alignSecretFlag(p.ID, secret.KeySecurityToken, p.HasSecurityToken, "security token")
}

func (s *Server) alignSecretFlag(profileID, key string, claimed bool, label string) bool {
	value, ok, err := s.rt.Secrets.Get(profileID, key)
	if err != nil {
		return claimed
	}
	usable := ok && value != ""
	if claimed && !usable {
		s.profileLog(profileID, label+" flag cleared: secret store has no usable value")
	}
	return usable
}

func writeProfileSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrProfileInvalid) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func (s *Server) reviveProfile(profileID string) {
	if profileID == "" {
		return
	}
	s.mu.Lock()
	delete(s.retiredProfiles, profileID)
	s.mu.Unlock()
}

func (s *Server) profileRetired(profileID string) bool {
	if profileID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retiredProfiles[profileID]
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
	formattedLine := time.Now().Format("15:04:05") + "  " + line
	l.lines = append(l.lines, formattedLine)
	if len(l.lines) > 500 {
		l.lines = l.lines[len(l.lines)-500:]
	}
	// Broadcast to all SSE listeners
	for _, ch := range l.listeners {
		select {
		case ch <- formattedLine:
		default: // Skip if channel is full (slow consumer)
		}
	}
}

func (l *safeLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *safeLog) addListener(ch chan string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners = append(l.listeners, ch)
}

func (l *safeLog) removeListener(ch chan string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, listener := range l.listeners {
		if listener == ch {
			l.listeners = append(l.listeners[:i], l.listeners[i+1:]...)
			break
		}
	}
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

func (s *profileLogStore) delete(profileID string) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		profileID = "_unspecified"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.logs, profileID)
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

// logProfileWarnings writes the non-fatal configuration warnings into the
// profile log, which is the one channel both the GUI and the tunnel side can see.
func (s *Server) logProfileWarnings(p config.Profile) {
	for _, warning := range p.Warnings() {
		s.profileLog(p.ID, "warning: "+warning)
	}
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

func hostKeyPolicyForRequest(acceptNew, replaceChanged bool) sshclient.HostKeyPolicy {
	if replaceChanged {
		return sshclient.HostKeyReplaceChanged
	}
	if acceptNew {
		return sshclient.HostKeyAcceptNew
	}
	return sshclient.HostKeyStrict
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
				"host":                  hostKeyErr.Host,
				"address":               hostKeyErr.Address,
				"key_type":              hostKeyErr.KeyType,
				"fingerprint":           hostKeyErr.Fingerprint,
				"known_hosts_path":      hostKeyErr.KnownHostsPath,
				"previous_fingerprints": hostKeyErr.PreviousFingerprints,
				"mismatch":              hostKeyErr.Mismatch,
			},
		}
		if encErr := json.NewEncoder(w).Encode(body); encErr != nil {
			log.Printf("write ssh error response: %v", encErr)
		}
		return
	}
	writeError(w, http.StatusBadGateway, err)
}
