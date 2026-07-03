package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLauncherAPIRequiresTokenForWrites(t *testing.T) {
	server := NewServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/browser-client/heartbeat", strings.NewReader(`{"id":"client"}`))
	rec := httptest.NewRecorder()

	server.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLauncherAPIAcceptsMatchingTokenForWrites(t *testing.T) {
	server := NewServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/browser-client/heartbeat", strings.NewReader(`{"id":"client"}`))
	addLauncherAPIToken(req, server)
	rec := httptest.NewRecorder()

	server.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestLauncherAPIRejectsCrossSiteWrites(t *testing.T) {
	server := NewServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/browser-client/heartbeat", strings.NewReader(`{"id":"client"}`))
	addLauncherAPIToken(req, server)
	req.Header.Set("Origin", "http://example.invalid")
	rec := httptest.NewRecorder()

	server.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLauncherAPIRejectsPartialOrMismatchedTokens(t *testing.T) {
	tests := []struct {
		name   string
		header string
		cookie string
	}{
		{name: "header only", header: "token"},
		{name: "cookie only", cookie: "token"},
		{name: "mismatched token", header: "token", cookie: "other-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(nil)
			req := httptest.NewRequest(http.MethodPost, "/api/browser-client/heartbeat", strings.NewReader(`{"id":"client"}`))
			if tt.header != "" {
				if tt.header == "token" {
					tt.header = server.apiToken
				}
				req.Header.Set(launcherAPITokenHeader, tt.header)
			}
			if tt.cookie != "" {
				if tt.cookie == "token" {
					tt.cookie = server.apiToken
				}
				req.AddCookie(&http.Cookie{Name: launcherAPITokenCookie, Value: tt.cookie})
			}
			rec := httptest.NewRecorder()

			server.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
		})
	}
}

func TestChemSSHAPIProxyIsNotLauncherTokenProtected(t *testing.T) {
	server := NewServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/chemssh-own-post-api", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	server.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatal("ChemSSH proxy fallback was rejected by launcher API token middleware")
	}
}

func addLauncherAPIToken(req *http.Request, server *Server) {
	req.Header.Set(launcherAPITokenHeader, server.apiToken)
	req.AddCookie(&http.Cookie{Name: launcherAPITokenCookie, Value: server.apiToken})
}
