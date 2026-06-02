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
	"strconv"
	"strings"
	"sync"
	"time"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/chemssh"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"
	"chemssh-launcher/internal/version"
	"chemssh-launcher/internal/webview"

	"golang.org/x/crypto/ssh"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	rt       *runtime.Runtime
	mux      *http.ServeMux
	logs     *safeLog
	localLog *safeLog
	options  Options
	baseURL  string
	mu       sync.Mutex
	session  *activeSession
	shutdown context.CancelFunc
}

type Options struct {
	UseWebView   bool
	ForceBrowser bool
	DevTools     bool
}

type activeSession struct {
	id         string
	name       string
	profile    config.Profile
	client     *ssh.Client
	process    *sshclient.RemoteProcess
	tunnel     *sshclient.Tunnel
	healthStop context.CancelFunc
	forwarding bool
	stopping   bool
	remotePID  int
}

type safeLog struct {
	mu    sync.Mutex
	lines []string
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
		rt:       rt,
		mux:      http.NewServeMux(),
		logs:     &safeLog{},
		localLog: &safeLog{},
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/shell", s.handleShell)
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
	s.mux.HandleFunc("/api/launcher-logs", s.handleLauncherLogs)
	s.mux.HandleFunc("/api/backend", s.handleBackendInfo)
	s.mux.HandleFunc("/api/backend/open-config-dir", s.handleOpenConfigDir)
	s.mux.HandleFunc("/api/backend/export", s.handleExportProfiles)
	s.mux.HandleFunc("/api/backend/import", s.handleImportProfiles)
	s.mux.HandleFunc("/api/backend/clear-cache", s.handleClearCache)
	s.mux.HandleFunc("/api/version", s.handleVersion)
	s.mux.HandleFunc("/api/defaults", s.handleDefaults)
	s.mux.HandleFunc("/api/", s.handleChemSSHProxy)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, assets, "static/index.html")
}

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/shell" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, assets, "static/shell.html")
}

func (s *Server) handleLauncherLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/launcher-logs" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, assets, "static/logs.html")
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
	s.logs.add("start requested for " + p.Name)
	if warning := sshclient.PortWarning(p); warning != "" {
		s.logs.add("warning: " + warning)
	}
	if p.UsesNonLoopbackLocalHost() {
		s.logs.add("warning: local bind host " + p.LocalHost + " may expose the tunnel")
	}
	if err := s.blockActiveForwardingConflict(p); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.checkLocalPort(p); err != nil {
		s.logs.add(err.Error())
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
		s.logs.add("SSH connection failed for " + p.Name + ": " + err.Error())
		writeSSHError(w, err)
		return
	}
	s.logs.add("SSH connection OK for " + p.Name)
	check, err := sshclient.RunCheckPortCommand(client, p, s.logs, s.logs)
	if err != nil {
		_ = client.Close()
		s.logs.add("ChemSSH port check failed for " + p.Name + ": " + err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	if check.Reusable {
		s.logs.add("remote ChemSSH is reusable; starting tunnel without launching another server")
	} else {
		s.logs.add("remote ChemSSH port is available; starting configured command")
	}

	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		_ = client.Close()
		writeError(w, http.StatusConflict, errors.New("a session is already running"))
		return
	}

	var process *sshclient.RemoteProcess
	if !check.Reusable {
		process, err = sshclient.StartRemoteCommand(client, p, s.logs, s.logs)
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
	s.session = session
	s.mu.Unlock()

	s.logs.add("starting " + p.Name + " at " + p.BrowserURL())
	if process != nil {
		go s.watchRemoteProcess(session)
	}
	go func() {
		if err := netcheck.WaitForURL(healthCtx, p.HealthURL(), 90*time.Second, time.Second); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.logs.add("health check failed: " + err.Error())
			s.stopSessionResources(session, true)
			return
		}
		s.logs.add("health check OK: " + p.HealthURL())
		s.refreshRemoteIdentity(session, p)
		if p.OpenBrowser {
			s.openSessionURL(p)
		}
	}()
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) blockActiveForwardingConflict(p config.Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.session
	if session == nil {
		return nil
	}
	if session.id != p.ID || session.forwarding {
		return errors.New("a session is already running")
	}
	return nil
}

func (s *Server) restartForwardingIfPaused(p config.Profile) (bool, error) {
	s.mu.Lock()
	session := s.session
	if session == nil {
		s.mu.Unlock()
		return false, nil
	}
	if session.id != p.ID || session.forwarding {
		s.mu.Unlock()
		return true, errors.New("a session is already running")
	}
	client := session.client
	if client == nil {
		s.mu.Unlock()
		return true, errors.New("session is still running, but its SSH connection is no longer available")
	}
	s.mu.Unlock()

	s.logs.add("service is already running; restarting forwarding only for " + p.Name)
	tunnel, err := sshclient.StartTunnel(context.Background(), client, p)
	if err != nil {
		return true, err
	}
	healthCtx, healthStop := context.WithCancel(context.Background())

	s.mu.Lock()
	if s.session != session || session.forwarding {
		s.mu.Unlock()
		healthStop()
		_ = tunnel.Close()
		return true, errors.New("a session is already running")
	}
	session.profile = p
	session.tunnel = tunnel
	session.healthStop = healthStop
	session.forwarding = true
	s.mu.Unlock()

	go s.waitForRestartedForwardingHealth(session, p, healthCtx)
	s.logs.add("forwarding restarted for " + p.BrowserURL())
	return true, nil
}

func (s *Server) waitForRestartedForwardingHealth(session *activeSession, p config.Profile, ctx context.Context) {
	if err := netcheck.WaitForURL(ctx, p.HealthURL(), 90*time.Second, time.Second); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.logs.add("health check failed after forwarding restart: " + err.Error())
		s.stopSessionResources(session, true)
		return
	}
	s.logs.add("health check OK: " + p.HealthURL())
	s.refreshRemoteIdentity(session, p)
	if p.OpenBrowser {
		s.openSessionURL(p)
	}
}

func (s *Server) refreshRemoteIdentity(session *activeSession, p config.Profile) {
	s.mu.Lock()
	if s.session != session {
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
		s.logs.add("warning: could not read ChemSSH identity: " + err.Error())
		return
	}
	s.mu.Lock()
	if s.session == session {
		session.remotePID = identity.PID
	}
	s.mu.Unlock()
	s.logs.add("ChemSSH identity OK: pid " + strconv.Itoa(identity.PID) + ", version " + identity.ProjectVersion)
}

func (s *Server) openSessionURL(p config.Profile) {
	if s.options.UseWebView && !s.options.ForceBrowser {
		return
	}
	url := s.sessionProxyURL(p)
	if err := browser.Open(url); err != nil {
		s.logs.add("warning: could not open browser: " + err.Error())
	}
}

func (s *Server) sessionProxyURL(p config.Profile) string {
	path := normalizedProxyPath(p.LocalURLPath)
	if path == "/" {
		return s.baseURL + "/chemssh"
	}
	return s.baseURL + "/chemssh" + path
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

func (s *Server) activeChemSSHTarget() (*url.URL, *activeSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.session
	if session == nil || !session.forwarding {
		return nil, nil
	}
	target, err := url.Parse("http://" + session.profile.LocalAddress())
	if err != nil {
		return nil, nil
	}
	return target, session
}

const chemsshProxyMaxRetries = 5
const chemsshProxyRetryBase = 200 * time.Millisecond

func (s *Server) handleChemSSHProxy(w http.ResponseWriter, r *http.Request) {
	target, session := s.activeChemSSHTarget()
	if target == nil || session == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("chemssh service is not available; start forwarding first"))
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
		proxy := s.newChemSSHReverseProxy(target)
		errCh := make(chan error, 1)
		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			errCh <- err
		}
		proxy.ServeHTTP(w, s.rewriteChemSSHProxyRequest(r, target))
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
	writeError(w, status, err)
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

func writeLoadingPage(w http.ResponseWriter, refreshURL string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Refresh", "2;url="+refreshURL)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, chemsshLoadingHTML)
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
  <h2>ChemSSH is starting up…</h2>
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

func (s *Server) rewriteChemSSHProxyRequest(r *http.Request, target *url.URL) *http.Request {
	req := r.Clone(r.Context())
	req.Host = target.Host
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path = chemsshProxyTargetPath(r.URL.Path)
	req.URL.RawPath = req.URL.Path
	return req
}

func chemsshProxyTargetPath(path string) string {
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

func (s *Server) newChemSSHReverseProxy(target *url.URL) *httputil.ReverseProxy {
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
		rewritten := rewriteChemSSHLocationHeader(location, target)
		if rewritten != "" {
			resp.Header.Set("Location", rewritten)
		}
		return nil
	}
	return proxy
}

func rewriteChemSSHLocationHeader(location string, target *url.URL) string {
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
	if parsed.Path == "/" {
		parsed.Path = "/chemssh"
	} else if strings.HasPrefix(parsed.Path, "/") && !strings.HasPrefix(parsed.Path, "/chemssh") {
		parsed.Path = "/chemssh" + parsed.Path
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
	s.logs.add("testing SSH for " + p.Name)
	policy := sshclient.HostKeyStrict
	if req.AcceptHostKey {
		policy = sshclient.HostKeyAcceptNew
	}
	client, err := sshclient.DialWithHostKeyPolicy(p, s.rt.Secrets, policy)
	if err != nil {
		s.logs.add("SSH connection failed for " + p.Name + ": " + err.Error())
		writeSSHError(w, err)
		return
	}
	s.logs.add("SSH connection OK for " + p.Name)
	if _, err := sshclient.RunCheckPortCommand(client, p, s.logs, s.logs); err != nil {
		_ = client.Close()
		s.logs.add("ChemSSH port check failed for " + p.Name + ": " + err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	_ = client.Close()
	s.logs.add("ChemSSH port check OK for " + p.RemoteAddress())
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) checkLocalPort(p config.Profile) error {
	ok, err := netcheck.IsPortAvailable(p.LocalHost, p.LocalPort)
	if err != nil {
		return err
	}
	if ok {
		s.logs.add("local port OK: " + p.LocalAddress())
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
	if s.session != session {
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
		s.logs.add("remote command exited normally; keeping forwarding open")
		return
	}
	s.session = nil
	tunnel := session.tunnel
	client := session.client
	healthStop := session.healthStop
	session.tunnel = nil
	session.client = nil
	session.healthStop = nil
	session.forwarding = false
	s.mu.Unlock()

	s.logs.add("remote command stopped with error: " + err.Error())
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

func (s *Server) stopForwarding() {
	s.mu.Lock()
	session := s.session
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
	clearSession := session.process == nil && session.remotePID <= 0 && client == nil
	if clearSession {
		s.session = nil
		session.client = nil
	}
	s.mu.Unlock()

	s.logs.add("stopping forwarding for " + session.name)
	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if session.process == nil && session.remotePID <= 0 && client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		identity, err := chemssh.FetchIdentity(ctx, client, session.profile)
		cancel()
		if err == nil {
			s.mu.Lock()
			if s.session == session {
				session.remotePID = identity.PID
			}
			s.mu.Unlock()
			s.logs.add("ChemSSH identity OK: pid " + strconv.Itoa(identity.PID) + ", version " + identity.ProjectVersion)
		} else {
			s.logs.add("warning: could not read ChemSSH identity after stopping forwarding: " + err.Error())
		}
	}
	if clearSession && client != nil {
		_ = client.Close()
	}
	if clearSession {
		s.logs.add("forwarding stopped")
	} else {
		s.logs.add("forwarding stopped; remote service is still running")
	}
}

func (s *Server) stopSessionResources(session *activeSession, stopRemote bool) {
	s.mu.Lock()
	if s.session != session {
		s.mu.Unlock()
		return
	}
	s.session = nil
	session.stopping = true
	tunnel := session.tunnel
	process := session.process
	client := session.client
	healthStop := session.healthStop
	profile := session.profile
	session.tunnel = nil
	session.process = nil
	session.client = nil
	session.healthStop = nil
	session.forwarding = false
	s.mu.Unlock()

	if healthStop != nil {
		healthStop()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if stopRemote {
		pid := session.remotePID
		if pid <= 0 && client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			identity, err := chemssh.FetchIdentity(ctx, client, profile)
			cancel()
			if err != nil {
				s.logs.add("warning: could not read ChemSSH identity before stopping: " + err.Error())
			} else {
				pid = identity.PID
			}
		}
		if pid > 0 && client != nil {
			if err := sshclient.StopRemotePID(client, pid); err != nil {
				s.logs.add("warning: could not kill remote ChemSSH pid " + strconv.Itoa(pid) + ": " + err.Error())
			} else {
				s.logs.add("remote ChemSSH pid stopped: " + strconv.Itoa(pid))
			}
		} else if process != nil {
			_ = process.Stop()
		} else {
			s.logs.add("warning: remote ChemSSH pid is unknown; remote service may still be running")
		}
	}
	if client != nil {
		_ = client.Close()
	}
	if stopRemote {
		s.logs.add("remote service and forwarding stopped")
	} else {
		s.logs.add("session resources stopped")
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.stopForwarding()
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleStopService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session != nil {
		s.logs.add("stopping remote service and forwarding for " + session.name)
		s.stopSessionResources(session, true)
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	running := session != nil
	forwarding := false
	name := ""
	id := ""
	if running {
		name = session.name
		id = session.id
		forwarding = session.forwarding
	}
	url := ""
	if running && forwarding {
		url = s.sessionProxyURL(session.profile)
	}
	openBrowser := false
	if running {
		openBrowser = session.profile.OpenBrowser
	}
	writeJSON(w, map[string]any{"running": running, "forwarding": forwarding, "id": id, "name": name, "url": url, "open_browser": openBrowser}, nil)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
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
	if p.AuthMethod == config.AuthPassword {
		switch req.PasswordAction {
		case "replace":
			started := time.Now()
			if err := s.rt.Secrets.Set(p.ID, secret.KeyPassword, req.Password); err != nil {
				return err
			}
			s.logs.add("password saved to secret store in " + time.Since(started).Round(time.Millisecond).String())
			p.HasPassword = true
		case "clear":
			started := time.Now()
			if err := s.rt.Secrets.Delete(p.ID, secret.KeyPassword); err != nil {
				return err
			}
			s.logs.add("password cleared from secret store in " + time.Since(started).Round(time.Millisecond).String())
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
			s.logs.add("private key passphrase saved to secret store in " + time.Since(started).Round(time.Millisecond).String())
			p.HasPrivateKeyPassphrase = true
		case "clear":
			started := time.Now()
			if err := s.rt.Secrets.Delete(p.ID, secret.KeyPrivatePassphrase); err != nil {
				return err
			}
			s.logs.add("private key passphrase cleared from secret store in " + time.Since(started).Round(time.Millisecond).String())
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
