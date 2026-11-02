package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chemssh-launcher/internal/sftpclient"
)

func newServerCopyTestServer() *Server {
	return &Server{logs: &safeLog{}, sftp: sftpclient.NewManager()}
}

// A paste between sessions that are not known to reach the same account must be
// reported as unsupported, so the UI falls back to a streamed copy instead of
// failing the paste.
func TestServerCopySupportRejectsUnknownSessions(t *testing.T) {
	server := newServerCopyTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/sftp/server-copy-support?source=missing&target=other", nil)
	rec := httptest.NewRecorder()

	server.handleSFTPServerCopySupport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got serverCopyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Supported {
		t.Fatal("unknown sessions were reported as supporting a server-side copy")
	}
	if got.Reason == "" {
		t.Fatal("unsupported response has no reason")
	}
}

func TestCopyOnServerRejectsDifferentServers(t *testing.T) {
	server := newServerCopyTestServer()
	body := `{"source_session":"missing","target_session":"other","target_path":"/data/out","items":[{"source_path":"/data/a","relative_path":"a"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/sftp/copy-on-server", strings.NewReader(body))
	rec := httptest.NewRecorder()

	server.handleSFTPCopyOnServer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got serverCopyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Supported {
		t.Fatal("a copy across different servers was reported as supported")
	}
	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want none", got.Results)
	}
}
