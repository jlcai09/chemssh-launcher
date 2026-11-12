package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"chemssh-launcher/internal/sshclient"
)

func TestShellCwdEndpointRequiresTokenAndExistingSession(t *testing.T) {
	server := NewServer(nil)
	for _, tc := range []struct {
		name   string
		method string
		token  bool
		want   int
	}{
		{"missing token", http.MethodGet, false, http.StatusForbidden},
		{"missing session", http.MethodGet, true, http.StatusNotFound},
		{"wrong method", http.MethodPost, true, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/session/shell/cwd?id=missing", nil)
			if tc.token {
				addLauncherAPIToken(req, server)
			}
			rec := httptest.NewRecorder()
			server.mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestShellCwdUnavailableDoesNotReturnHome(t *testing.T) {
	server := NewServer(nil)
	server.shells.sessions["shell"] = &launcherShell{shell: &sshclient.InteractiveShell{}}
	req := httptest.NewRequest(http.MethodGet, "/api/session/shell/cwd?id=shell", nil)
	addLauncherAPIToken(req, server)
	rec := httptest.NewRecorder()
	server.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}
