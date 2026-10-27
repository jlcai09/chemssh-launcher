package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appruntime "chemssh-launcher/internal/runtime"
)

// Promotion used to ride on the status poll, so a read-only request every two
// seconds changed which session the local proxy served.
func TestStatusPollDoesNotPromoteAndActivateDoes(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	first := &activeSession{id: "first", name: "First", forwarding: true}
	second := &activeSession{id: "second", name: "Second", forwarding: true}
	server.mu.Lock()
	server.setSessionLocked(first)
	server.setSessionLocked(second)
	server.mu.Unlock()

	proxyID := func() string {
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.proxyProfileID
	}

	rec := httptest.NewRecorder()
	server.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/session/status?profile_id=first&activate=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := proxyID(); got != "second" {
		t.Fatalf("status poll promoted %q, want the proxy to stay on %q", got, "second")
	}

	rec = httptest.NewRecorder()
	server.handleActivate(rec, httptest.NewRequest(http.MethodPost, "/api/session/activate", strings.NewReader(`{"id":"first"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Activated bool   `json:"activated"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Activated || payload.ID != "first" {
		t.Fatalf("activate response = %+v, want activated first", payload)
	}
	if got := proxyID(); got != "first" {
		t.Fatalf("proxy = %q after activate, want %q", got, "first")
	}
}

func TestActivateSkipsSessionsWithoutForwarding(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	paused := &activeSession{id: "paused", name: "Paused", forwarding: false}
	server.mu.Lock()
	server.sessions["paused"] = paused
	server.mu.Unlock()

	rec := httptest.NewRecorder()
	server.handleActivate(rec, httptest.NewRequest(http.MethodPost, "/api/session/activate", strings.NewReader(`{"id":"paused"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Activated bool `json:"activated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Activated {
		t.Fatal("a session without forwarding was promoted")
	}
	server.mu.Lock()
	proxyID := server.proxyProfileID
	server.mu.Unlock()
	if proxyID != "" {
		t.Fatalf("proxy = %q, want it left unset", proxyID)
	}
}

func TestActivateRejectsNonPostRequests(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	rec := httptest.NewRecorder()
	server.handleActivate(rec, httptest.NewRequest(http.MethodGet, "/api/session/activate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
