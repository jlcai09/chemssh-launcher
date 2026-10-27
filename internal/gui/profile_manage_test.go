package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"chemssh-launcher/internal/config"
	appruntime "chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
)

func TestNormalizeImportedLocalProfileDropsLocalProxy(t *testing.T) {
	profile := config.NewLocalProfileDefaults()
	profile.ID = "local-import"
	profile.Name = "Local Import"
	profile.LocalProxy.Enabled = true

	normalized := normalizeImportedProfile(profile)
	if normalized.LocalProxy.Enabled {
		t.Fatalf("imported local profile kept the local network proxy enabled: %+v", normalized.LocalProxy)
	}
	if !normalized.LocalProxy.Normalized().IsLoopback() {
		t.Fatalf("imported local proxy lost its loopback default: %+v", normalized.LocalProxy)
	}
}

func TestHandleDuplicateProfileCopiesSettingsAndSecrets(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	secrets := secret.NewMemoryStore()
	source := config.NewProfileDefaults()
	source.ID = "source-profile"
	source.Name = "Example Server"
	source.SSHHost = "example.com"
	source.SSHUser = "user"
	source.HasPassword = true
	source.HasSecurityToken = true
	if err := profiles.Save(source); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(source.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(source.ID, secret.KeySecurityToken, "token"); err != nil {
		t.Fatal(err)
	}

	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})
	body := bytes.NewBufferString(`{"id":"source-profile","name":"Example Server copy"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/profiles/duplicate", body)
	rec := httptest.NewRecorder()

	server.handleDuplicateProfile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var clone config.Profile
	if err := json.NewDecoder(rec.Body).Decode(&clone); err != nil {
		t.Fatal(err)
	}
	if clone.ID == "" || clone.ID == source.ID {
		t.Fatalf("duplicate has an invalid id: %q", clone.ID)
	}
	if clone.Name != "Example Server copy" || clone.SSHHost != source.SSHHost || clone.SSHUser != source.SSHUser {
		t.Fatalf("duplicate did not preserve the profile settings: %+v", clone)
	}
	if !clone.HasPassword || !clone.HasSecurityToken {
		t.Fatalf("duplicate is missing credential flags: %+v", clone)
	}
	for key, want := range map[string]string{
		secret.KeyPassword:      "password",
		secret.KeySecurityToken: "token",
	} {
		value, ok, err := secrets.Get(clone.ID, key)
		if err != nil || !ok || value != want {
			t.Fatalf("secret %s was not copied: value=%q ok=%v err=%v", key, value, ok, err)
		}
	}
	list, err := profiles.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != source.ID || list[1].ID != clone.ID {
		t.Fatalf("duplicate was not appended after the source: %+v", list)
	}
}

func TestHandleDuplicateProfileCopiesOnlyActiveCredential(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	secrets := secret.NewMemoryStore()
	source := config.NewProfileDefaults()
	source.ID = "key-profile"
	source.Name = "Key Server"
	source.SSHHost = "example.com"
	source.SSHUser = "user"
	source.AuthMethod = config.AuthPrivateKey
	source.PrivateKeyPath = "/home/user/.ssh/id_ed25519"
	source.HasPrivateKeyPassphrase = true
	source.HasPassword = true
	if err := profiles.Save(source); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(source.ID, secret.KeyPrivatePassphrase, "token"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(source.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}

	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})
	req := httptest.NewRequest(http.MethodPost, "/api/profiles/duplicate", bytes.NewBufferString(`{"id":"key-profile"}`))
	rec := httptest.NewRecorder()

	server.handleDuplicateProfile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var clone config.Profile
	if err := json.NewDecoder(rec.Body).Decode(&clone); err != nil {
		t.Fatal(err)
	}
	if clone.Name != "Key Server (copy)" || !clone.HasPrivateKeyPassphrase || clone.HasPassword {
		t.Fatalf("unexpected duplicated credential flags: %+v", clone)
	}
	if _, ok, err := secrets.Get(clone.ID, secret.KeyPrivatePassphrase); err != nil || !ok {
		t.Fatalf("private key passphrase was not copied: ok=%v err=%v", ok, err)
	}
	if _, ok, err := secrets.Get(clone.ID, secret.KeyPassword); err != nil || ok {
		t.Fatalf("inactive password secret was copied: ok=%v err=%v", ok, err)
	}
}

func TestHandleDuplicateProfileRejectsUnknownID(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secret.NewMemoryStore()})
	body := bytes.NewBufferString(`{"id":"missing"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/profiles/duplicate", body)
	rec := httptest.NewRecorder()

	server.handleDuplicateProfile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	list, err := profiles.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("unexpected profiles after failed duplicate: %+v", list)
	}
}

func TestHandleReorderProfilesPersistsOrder(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	for _, id := range []string{"first", "second", "third"} {
		profile := config.NewProfileDefaults()
		profile.ID = id
		profile.Name = id
		if err := profiles.Save(profile); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secret.NewMemoryStore()})
	body := bytes.NewBufferString(`{"ids":["third","first","second"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/profiles/reorder", body)
	rec := httptest.NewRecorder()

	server.handleReorderProfiles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var ordered []config.Profile
	if err := json.NewDecoder(rec.Body).Decode(&ordered); err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 3 || ordered[0].ID != "third" || ordered[1].ID != "first" || ordered[2].ID != "second" {
		t.Fatalf("unexpected reordered response: %+v", ordered)
	}
	list, err := profiles.List()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != "third" || list[1].ID != "first" || list[2].ID != "second" {
		t.Fatalf("order was not persisted: %+v", list)
	}
}
