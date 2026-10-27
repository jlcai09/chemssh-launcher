package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chemssh-launcher/internal/config"
	appruntime "chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"
)

func newProfileTestServer(t *testing.T) (*Server, *config.FileStore, *secret.MemoryStore) {
	t.Helper()
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	secrets := secret.NewMemoryStore()
	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})
	return server, profiles, secrets
}

func sampleRemoteProfile(name string) config.Profile {
	p := config.NewProfileDefaults()
	p.ID = "profile-1"
	p.Name = name
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	return p
}

func statusIDs(t *testing.T, server *Server) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/session/statuses", nil)
	rec := httptest.NewRecorder()
	server.handleStatuses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("statuses returned %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Statuses map[string]json.RawMessage `json:"statuses"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Statuses
}

func TestDeleteProfileFailureDoesNotRetireProfile(t *testing.T) {
	server, profiles, _ := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	p.LocalPort = freeLocalPort(t)
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}
	// Make the store read-only so Delete fails while Get still succeeds.
	if err := os.Chmod(profiles.Path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(profiles.Path, 0o666) })

	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/"+p.ID, nil)
	rec := httptest.NewRecorder()
	server.handleProfileByID(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete returned %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if server.profileRetired(p.ID) {
		t.Fatal("failed delete left the profile retired")
	}
	if _, err := profiles.Get(p.ID); err != nil {
		t.Fatalf("profile disappeared after a failed delete: %v", err)
	}

	// The profile still exists, so a start attempt must fail visibly instead of
	// returning the silent "accepted" response a retired profile produces. No
	// password is stored, so the SSH dial fails before any network I/O.
	startReq := httptest.NewRequest(http.MethodPost, "/api/start", strings.NewReader(`{"id":"`+p.ID+`"}`))
	startRec := httptest.NewRecorder()
	server.handleStart(startRec, startReq)
	if startRec.Code == http.StatusOK {
		t.Fatalf("start succeeded for a profile whose delete failed: %s", startRec.Body.String())
	}
}

func TestStartRetiredProfileReturnsConflict(t *testing.T) {
	server, profiles, _ := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/"+p.ID, nil)
	rec := httptest.NewRecorder()
	server.handleProfileByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", rec.Code, rec.Body.String())
	}

	// Put the profile back without clearing the retired marker, as an external
	// restore of the store would.
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/start", strings.NewReader(`{"id":"`+p.ID+`"}`))
	startRec := httptest.NewRecorder()
	server.handleStart(startRec, startReq)
	if startRec.Code != http.StatusConflict {
		t.Fatalf("start returned %d, want 409: %s", startRec.Code, startRec.Body.String())
	}
}

func TestDeleteMissingProfileReturnsNotFound(t *testing.T) {
	server, _, _ := newProfileTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/missing", nil)
	rec := httptest.NewRecorder()

	server.handleProfileByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete returned %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProfileStopsSessionAndClearsRecords(t *testing.T) {
	server, profiles, secrets := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	p.LocalPort = freeLocalPort(t)
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(p.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}

	tunnel, err := sshclient.StartTunnel(context.Background(), nil, p)
	if err != nil {
		t.Fatal(err)
	}
	session := &activeSession{
		id:         p.ID,
		name:       p.Name,
		profile:    p,
		tunnel:     tunnel,
		forwarding: true,
		ready:      true,
	}
	server.mu.Lock()
	server.setSessionLocked(session)
	server.pendingStart[p.ID] = true
	server.lastFailure[p.ID] = sessionFailure{Message: "previous failure"}
	server.mu.Unlock()
	server.profileLog(p.ID, "line written before deletion")

	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/"+p.ID, nil)
	rec := httptest.NewRecorder()
	server.handleProfileByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := profiles.Get(p.ID); !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("profile still present: %v", err)
	}
	if _, ok, err := secrets.Get(p.ID, secret.KeyPassword); err != nil || ok {
		t.Fatalf("password secret retained: ok=%v err=%v", ok, err)
	}

	server.mu.Lock()
	_, hasSession := server.sessions[p.ID]
	pending := server.pendingStart[p.ID]
	_, failed := server.lastFailure[p.ID]
	retired := server.retiredProfiles[p.ID]
	server.mu.Unlock()
	if hasSession {
		t.Fatal("session retained after profile deletion")
	}
	if pending {
		t.Fatal("pending start record retained after profile deletion")
	}
	if failed {
		t.Fatal("failure record retained after profile deletion")
	}
	if !retired {
		t.Fatal("deleted profile was not marked retired")
	}

	server.profileLogs.mu.Lock()
	_, hasLogs := server.profileLogs.logs[p.ID]
	server.profileLogs.mu.Unlock()
	if hasLogs {
		t.Fatal("per-profile logs retained after profile deletion")
	}

	ln, err := net.Listen("tcp", p.LocalAddress())
	if err != nil {
		t.Fatalf("tunnel listener still bound after deletion: %v", err)
	}
	_ = ln.Close()

	if _, ok := statusIDs(t, server)[p.ID]; ok {
		t.Fatal("statuses still reports the deleted profile")
	}
}

func TestDeletedProfileIgnoresLateStartRecords(t *testing.T) {
	server, profiles, _ := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/"+p.ID, nil)
	rec := httptest.NewRecorder()
	server.handleProfileByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", rec.Code, rec.Body.String())
	}

	server.markStartPending(p.ID)
	server.recordSessionFailure(p.ID, "late failure")
	if _, ok := statusIDs(t, server)[p.ID]; ok {
		t.Fatal("a late start record resurrected the deleted profile in statuses")
	}

	restored := p
	restored.Name = "Restored Server"
	if err := server.saveProfileWithSecrets(restored, secretRequest{PasswordAction: "keep"}); err != nil {
		t.Fatal(err)
	}
	server.markStartPending(p.ID)
	if _, ok := statusIDs(t, server)[p.ID]; !ok {
		t.Fatal("saving a profile with the same id did not clear the retired marker")
	}
}

func TestHandleProfilesRejectsInvalidProfile(t *testing.T) {
	server, profiles, _ := newProfileTestServer(t)
	body := `{"profile":{"name":"","ssh_host":"example.com","ssh_port":22,"ssh_user":"user","auth_method":"password","remote_host":"127.0.0.1","remote_port":8888,"local_host":"127.0.0.1","local_port":8888}}`
	req := httptest.NewRequest(http.MethodPost, "/api/profiles", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleProfiles(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	list, err := profiles.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("invalid profile was saved: %+v", list)
	}
}

func TestHandleProfileUpdateRejectsInvalidProfile(t *testing.T) {
	server, profiles, secrets := newProfileTestServer(t)
	p := sampleRemoteProfile("Original Name")
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}
	_ = secrets.Set(p.ID, secret.KeyPassword, "password")

	body := `{"profile":{"name":"Renamed","ssh_host":"","ssh_port":22,"ssh_user":"user","auth_method":"password","remote_host":"127.0.0.1","remote_port":8888,"local_host":"127.0.0.1","local_port":8888}}`
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/"+p.ID, strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleProfileByID(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	stored, err := profiles.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Original Name" {
		t.Fatalf("invalid update changed the stored profile: %+v", stored)
	}
}

func TestImportRejectsInvalidProfileBeforeTouchingSecrets(t *testing.T) {
	server, profiles, secrets := newProfileTestServer(t)
	if err := secrets.Set("remote", secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"profiles":[{"id":"remote","name":"Imported","ssh_host":"","ssh_port":22,"ssh_user":"user","auth_method":"password","remote_host":"127.0.0.1","remote_port":8888,"local_host":"127.0.0.1","local_port":8888}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/backend/import", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleImportProfiles(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := secrets.Get("remote", secret.KeyPassword); err != nil || !ok {
		t.Fatalf("import cleared secrets before validation: ok=%v err=%v", ok, err)
	}
	list, err := profiles.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("invalid import saved profiles: %+v", list)
	}
}
