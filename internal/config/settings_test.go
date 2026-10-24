package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUISettingsMissingFileUsesSystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Language != LanguageSystem {
		t.Fatalf("language = %q, want %q", settings.Language, LanguageSystem)
	}
}

func TestUISettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveUISettingsFile(path, UISettings{Language: LanguageChinese}); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Version != 1 || settings.Language != LanguageChinese {
		t.Fatalf("unexpected settings: %+v", settings)
	}
}

func TestLoadUISettingsInvalidContentFallsBackToSystem(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Language != LanguageSystem {
		t.Fatalf("language = %q, want %q", settings.Language, LanguageSystem)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"version":1,"language":"fr"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err = LoadUISettingsFile(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Language != LanguageSystem {
		t.Fatalf("language = %q, want %q", settings.Language, LanguageSystem)
	}
}

func TestSaveUISettingsRejectsUnknownLanguage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveUISettingsFile(path, UISettings{Language: "fr"}); err == nil {
		t.Fatal("expected error for unsupported language")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("settings file should not be written, stat err = %v", err)
	}
}
