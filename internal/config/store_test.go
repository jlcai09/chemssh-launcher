package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStoreRoundTripDoesNotStoreSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	store := NewFileStore(path)
	profile := NewProfileDefaults()
	profile.ID = "hpc-main"
	profile.Name = "HPC Main"
	profile.SSHHost = "server.example.com"
	profile.SSHUser = "user"
	profile.HasPassword = true

	if err := store.Save(profile); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("HPC Main")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != profile.ID || got.Name != profile.Name || got.LocalPort != 8888 {
		t.Fatalf("unexpected profile after round trip: %+v", got)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "password-value") {
		t.Fatal("profile JSON contains a raw password")
	}

	var data fileData
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	if data.Version != 1 || len(data.Profiles) != 1 {
		t.Fatalf("unexpected file data: %+v", data)
	}
}

func TestFileStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	store := NewFileStore(path)
	profile := NewProfileDefaults()
	profile.ID = "delete-me"
	profile.Name = "Delete Me"

	if err := store.Save(profile); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("Delete Me"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("delete-me"); err == nil {
		t.Fatal("expected deleted profile to be missing")
	}
}

func TestProfileBrowserAndHealthURLsAreSeparate(t *testing.T) {
	profile := NewProfileDefaults()
	profile.LocalURLPath = "chemweb"
	profile.HealthCheckURL = "http://127.0.0.1:8888/health"

	if got := profile.BrowserURL(); got != "http://127.0.0.1:8888/chemweb" {
		t.Fatalf("unexpected browser URL: %q", got)
	}
	if got := profile.HealthURL(); got != "http://127.0.0.1:8888/health" {
		t.Fatalf("unexpected health URL: %q", got)
	}
}

func TestDefaultStartCommandIncludesEnvironmentPromptAndChemweb(t *testing.T) {
	profile := NewProfileDefaults()
	for _, want := range []string{
		"source .venv/bin/activate",
		"__chemweb_conda_prompt",
		"__chemweb_venv_prompt",
		"export PS1=",
		"chemweb --config config.yaml",
	} {
		if !strings.Contains(profile.StartCommand, want) {
			t.Fatalf("default start command missing %q\n%s", want, profile.StartCommand)
		}
	}
}
