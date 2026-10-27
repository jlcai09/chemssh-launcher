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
	if settings.TerminalFontSize != DefaultTerminalFontSize || settings.TerminalScrollback != DefaultTerminalScrollback {
		t.Fatalf("unexpected terminal defaults: %+v", settings)
	}
}

func TestUISettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := UISettings{Language: LanguageChinese, TerminalFontSize: 16, TerminalScrollback: 20000}
	if err := SaveUISettingsFile(path, want); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Version != 1 || settings.Language != LanguageChinese {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	if settings.TerminalFontSize != 16 || settings.TerminalScrollback != 20000 {
		t.Fatalf("terminal settings not preserved: %+v", settings)
	}
}

func TestLoadUISettingsFillsTerminalDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"language":"en"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TerminalFontSize != DefaultTerminalFontSize || settings.TerminalScrollback != DefaultTerminalScrollback {
		t.Fatalf("terminal settings should fall back to defaults: %+v", settings)
	}
}

func TestLoadUISettingsClampsTerminalRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"language":"en","terminal_font_size":99,"terminal_scrollback":10}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TerminalFontSize != MaxTerminalFontSize {
		t.Fatalf("font size = %d, want %d", settings.TerminalFontSize, MaxTerminalFontSize)
	}
	if settings.TerminalScrollback != MinTerminalScrollback {
		t.Fatalf("scrollback = %d, want %d", settings.TerminalScrollback, MinTerminalScrollback)
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

func TestSaveUISettingsRejectsTerminalRangeViolations(t *testing.T) {
	dir := t.TempDir()
	fontSizePath := filepath.Join(dir, "font.json")
	if err := SaveUISettingsFile(fontSizePath, UISettings{Language: LanguageEnglish, TerminalFontSize: 40}); err == nil {
		t.Fatal("expected error for oversized terminal font")
	}
	if _, err := os.Stat(fontSizePath); !os.IsNotExist(err) {
		t.Fatalf("settings file should not be written, stat err = %v", err)
	}
	scrollbackPath := filepath.Join(dir, "scrollback.json")
	if err := SaveUISettingsFile(scrollbackPath, UISettings{Language: LanguageEnglish, TerminalScrollback: 500}); err == nil {
		t.Fatal("expected error for undersized terminal scrollback")
	}
	if _, err := os.Stat(scrollbackPath); !os.IsNotExist(err) {
		t.Fatalf("settings file should not be written, stat err = %v", err)
	}
}

func TestSaveUISettingsFillsTerminalDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveUISettingsFile(path, UISettings{Language: LanguageEnglish}); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadUISettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TerminalFontSize != DefaultTerminalFontSize || settings.TerminalScrollback != DefaultTerminalScrollback {
		t.Fatalf("unexpected stored defaults: %+v", settings)
	}
}
