package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
)

func TestValidateChemSSHBridgeRemotePath(t *testing.T) {
	tests := []struct {
		name       string
		remotePath string
		root       string
		wantErr    bool
		wantStatus int
	}{
		{"workspace root", "/home/user/project", "/home/user/project", false, 0},
		{"workspace child", "/home/user/project/input.inp", "/home/user/project", false, 0},
		{"trailing slash root", "/home/user/project/input.inp", "/home/user/project/", false, 0},
		{"root workspace allows absolute path", "/etc/hosts", "/", false, 0},
		{"sibling prefix is outside", "/home/user/project-old/input.inp", "/home/user/project", true, http.StatusForbidden},
		{"cleaned traversal is outside", "/home/user/project/../secret.txt", "/home/user/project", true, http.StatusForbidden},
		{"relative path", "input.inp", "/home/user/project", true, http.StatusBadRequest},
		{"missing root", "/home/user/project/input.inp", "", true, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateChemSSHBridgeRemotePath(tt.remotePath, tt.root)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				var statusErr *chemSSHBridgeHTTPError
				if !errors.As(err, &statusErr) {
					t.Fatalf("expected chemSSHBridgeHTTPError, got %T", err)
				}
				if statusErr.status != tt.wantStatus {
					t.Fatalf("status = %d, want %d", statusErr.status, tt.wantStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestChemSSHBridgeOpenSyncEventsFiltersSession(t *testing.T) {
	server := &Server{
		logs: &safeLog{},
		sftpSync: &sftpOpenSyncManager{
			events: []sftpOpenSyncEvent{
				{
					Seq:        1,
					Time:       time.Unix(1, 0).UTC(),
					Session:    "sftp-page",
					RemotePath: "/home/user/project/other.txt",
					LocalPath:  `C:\cache\other.txt`,
					Status:     "done",
				},
				{
					Seq:        2,
					Time:       time.Unix(2, 0).UTC(),
					Session:    "bridge",
					RemotePath: "/home/user/project/input.inp",
					LocalPath:  `C:\cache\input.inp`,
					Status:     "done",
				},
			},
		},
		bridge: chemSSHBridgeState{sftpSessionID: "bridge"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/chemssh-bridge/open-sync-events?after=0", nil)
	rec := httptest.NewRecorder()

	server.handleChemSSHBridgeOpenSyncEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Events) != 1 {
		t.Fatalf("events len = %d, want 1", len(body.Events))
	}
	if got := body.Events[0]["remote_path"]; got != "/home/user/project/input.inp" {
		t.Fatalf("remote_path = %v", got)
	}
	if _, ok := body.Events[0]["session"]; ok {
		t.Fatal("bridge sync event exposed internal session")
	}
}

func TestChemSSHBridgeCapabilitiesLocalProfileKeepsLocalOpenFeatures(t *testing.T) {
	profile := config.NewLocalProfileDefaults()
	profile.ID = "local"
	workspaceRoot := t.TempDir()
	server := &Server{
		logs: &safeLog{},
		session: &activeSession{
			id:            profile.ID,
			name:          profile.Name,
			profile:       profile,
			forwarding:    true,
			workspaceRoot: workspaceRoot,
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/chemssh-bridge/capabilities", nil)
	rec := httptest.NewRecorder()

	server.handleChemSSHBridgeCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body chemSSHBridgeCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Enabled || body.SessionID != "local" || body.WorkspaceRoot == "" {
		t.Fatalf("unexpected capabilities: %+v", body)
	}
	if !body.Features.SystemIcons || !body.Features.OpenDefault || !body.Features.OpenText || body.Features.OpenSyncEvents {
		t.Fatalf("unexpected local features: %+v", body.Features)
	}
	if body.Endpoints.Icon == "" || body.Endpoints.Open == "" || body.Endpoints.OpenText == "" || body.Endpoints.SyncEvents != "" {
		t.Fatalf("unexpected local endpoints: %+v", body.Endpoints)
	}
}

func TestValidateChemSSHBridgeLocalPath(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "input.inp")
	got, err := validateChemSSHBridgeLocalPath(child, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected cleaned local path")
	}
	if _, err := validateChemSSHBridgeLocalPath(filepath.Join(root, "..", "outside.inp"), root); err == nil {
		t.Fatal("expected outside path to fail")
	}
}

func TestSaveLocalProfileClearsCredentials(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	secrets := secret.NewMemoryStore()
	if err := secrets.Set("local", secret.KeyPassword, "secret"); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		rt: &runtime.Runtime{
			Profiles: profiles,
			Secrets:  secrets,
		},
		logs: &safeLog{},
	}
	profile := config.NewLocalProfileDefaults()
	profile.ID = "local"
	profile.AuthMethod = config.AuthPassword
	profile.HasPassword = true

	if err := server.saveProfileWithSecrets(profile, secretRequest{PasswordAction: "keep"}); err != nil {
		t.Fatal(err)
	}
	got, err := profiles.Get("local")
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != "" || got.HasPassword || got.SSHHost != "" || got.SSHUser != "" {
		t.Fatalf("local profile retained SSH credential fields: %+v", got)
	}
	if _, ok, err := secrets.Get("local", secret.KeyPassword); err != nil || ok {
		t.Fatalf("password secret retained: ok=%v err=%v", ok, err)
	}
}

func TestLocalSessionStatusUsesChemSSHProxyURL(t *testing.T) {
	profile := config.NewLocalProfileDefaults()
	profile.ID = "local"
	profile.Name = "Local ChemSSH"
	profile.LocalURLPath = "/"
	session := &activeSession{
		id:         profile.ID,
		name:       profile.Name,
		profile:    profile,
		forwarding: true,
	}
	server := &Server{
		baseURL:        "http://127.0.0.1:4567",
		logs:           &safeLog{},
		session:        session,
		sessions:       map[string]*activeSession{profile.ID: session},
		proxyProfileID: profile.ID,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/session/status", bytes.NewReader(nil))
	rec := httptest.NewRecorder()

	server.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["url"] != "http://127.0.0.1:4567/Local%20ChemSSH/chemssh?profile_id=local" || body["id"] != "local" || body["forwarding"] != true {
		t.Fatalf("unexpected status body: %+v", body)
	}
}

func TestProfileProxyPathTargetsMatchingSession(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "remote-1"
	profile.Name = "zhongke"
	profile.LocalHost = "127.0.0.1"
	profile.LocalPort = 8879
	session := &activeSession{id: profile.ID, name: profile.Name, profile: profile, forwarding: true}
	server := &Server{
		sessions: map[string]*activeSession{profile.ID: session},
	}

	req := httptest.NewRequest(http.MethodGet, "/zhongke/chemssh/assets/app.js?profile_id=remote-1", nil)
	got := server.sessionForProxyRequestLocked(req)
	if got != session {
		t.Fatalf("sessionForProxyRequestLocked = %+v, want session", got)
	}
	if target := chemsshProxyTargetPath(req.URL.Path, session); target != "/assets/app.js" {
		t.Fatalf("proxy target path = %q, want /assets/app.js", target)
	}
}
