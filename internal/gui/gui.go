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
	"strconv"
	"strings"
	"sync"
	"time"

	"chemweb-launcher/internal/app"
	"chemweb-launcher/internal/browser"
	"chemweb-launcher/internal/config"
	"chemweb-launcher/internal/netcheck"
	"chemweb-launcher/internal/runtime"
	"chemweb-launcher/internal/secret"
	"chemweb-launcher/internal/sshclient"
	"chemweb-launcher/internal/version"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	rt       *runtime.Runtime
	mux      *http.ServeMux
	logs     *safeLog
	mu       sync.Mutex
	session  *activeSession
	shutdown context.CancelFunc
}

type activeSession struct {
	cancel context.CancelFunc
	id     string
	name   string
}

type safeLog struct {
	mu    sync.Mutex
	lines []string
}

func Run(stdout, stderr io.Writer) error {
	rt, err := runtime.New()
	if err != nil {
		return err
	}
	server := NewServer(rt)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := "http://" + ln.Addr().String()
	fmt.Fprintln(stdout, "Chemweb Launcher", version.String(), "GUI:", addr)

	httpServer := &http.Server{Handler: server.mux}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	if err := browser.Open(addr); err != nil {
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
		rt:   rt,
		mux:  http.NewServeMux(),
		logs: &safeLog{},
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.Handle("/static/", http.FileServer(http.FS(assets)))
	s.mux.HandleFunc("/api/profiles", s.handleProfiles)
	s.mux.HandleFunc("/api/profiles/", s.handleProfileByID)
	s.mux.HandleFunc("/api/profile-test", s.handleProfileTest)
	s.mux.HandleFunc("/api/session/start", s.handleStart)
	s.mux.HandleFunc("/api/session/stop", s.handleStop)
	s.mux.HandleFunc("/api/session/status", s.handleStatus)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/version", s.handleVersion)
	s.mux.HandleFunc("/api/defaults", s.handleDefaults)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, assets, "static/index.html")
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
	if err := s.checkLocalPort(p); err != nil {
		s.logs.add(err.Error())
		writeError(w, http.StatusConflict, err)
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
	if err := sshclient.CheckRemotePortAvailable(client, p); err != nil {
		s.logs.add(err.Error())
		s.logs.add("attempting pidfile cleanup before start")
		_ = sshclient.StopRemoteCommand(client, p)
		time.Sleep(500 * time.Millisecond)
		if retryErr := sshclient.CheckRemotePortAvailable(client, p); retryErr != nil {
			_ = client.Close()
			writeError(w, http.StatusConflict, err)
			return
		}
		s.logs.add("remote port freed after pidfile cleanup")
	}
	_ = client.Close()
	s.logs.add("remote port OK: " + p.RemoteAddress())

	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("a session is already running"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.session = &activeSession{cancel: cancel, id: p.ID, name: p.Name}
	s.mu.Unlock()

	s.logs.add("starting " + p.Name + " at " + p.BrowserURL())
	go func() {
		err := app.New(s.rt.Profiles, s.rt.Secrets, s.logs, s.logs).Start(ctx, p)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logs.add("session stopped with error: " + err.Error())
		} else {
			s.logs.add("session stopped")
		}
		s.mu.Lock()
		s.session = nil
		s.mu.Unlock()
	}()
	writeJSON(w, map[string]bool{"ok": true}, nil)
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
	if err := s.checkLocalPort(p); err != nil {
		s.logs.add(err.Error())
		writeError(w, http.StatusConflict, err)
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
	if err := sshclient.CheckRemotePortAvailable(client, p); err != nil {
		s.logs.add(err.Error())
		s.logs.add("attempting pidfile cleanup before test")
		_ = sshclient.StopRemoteCommand(client, p)
		time.Sleep(500 * time.Millisecond)
		if retryErr := sshclient.CheckRemotePortAvailable(client, p); retryErr != nil {
			_ = client.Close()
			writeError(w, http.StatusConflict, err)
			return
		}
		s.logs.add("remote port freed after pidfile cleanup")
	}
	_ = client.Close()
	s.logs.add("remote port OK: " + p.RemoteAddress())
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

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session == nil {
		writeJSON(w, map[string]bool{"ok": true}, nil)
		return
	}
	s.logs.add("stopping " + session.name)
	session.cancel()
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
	name := ""
	id := ""
	if running {
		name = session.name
		id = session.id
	}
	writeJSON(w, map[string]any{"running": running, "id": id, "name": name}, nil)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string][]string{"lines": s.logs.snapshot()}, nil)
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
