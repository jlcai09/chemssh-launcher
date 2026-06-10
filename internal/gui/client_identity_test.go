package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateLauncherClientIdentityCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_identity.json")

	first, err := loadOrCreateLauncherClientIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != launcherClientIdentityVersion {
		t.Fatalf("version = %d, want %d", first.Version, launcherClientIdentityVersion)
	}
	if err := validateLauncherClientIdentity(first); err != nil {
		t.Fatal(err)
	}

	second, err := loadOrCreateLauncherClientIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.ClientID != first.ClientID {
		t.Fatalf("client id changed after reload: %q -> %q", first.ClientID, second.ClientID)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at changed after reload: %q -> %q", first.CreatedAt, second.CreatedAt)
	}
}

func TestHandleChemSSHBridgeClientIdentity(t *testing.T) {
	server := &Server{
		clientIdentity: launcherClientIdentity{
			Version:   launcherClientIdentityVersion,
			ClientID:  "client_launcher_test",
			CreatedAt: "2026-06-09T10:00:00Z",
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/chemssh-bridge/client-identity", nil)
	rec := httptest.NewRecorder()

	server.handleChemSSHBridgeClientIdentity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got chemSSHBridgeClientIdentity
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.ClientID != "client_launcher_test" || got.Source != "launcher" {
		t.Fatalf("unexpected identity response: %+v", got)
	}
}
