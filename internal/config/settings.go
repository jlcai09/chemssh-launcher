package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	LanguageSystem  = "system"
	LanguageChinese = "zh"
	LanguageEnglish = "en"
)

type UISettings struct {
	Version  int    `json:"version"`
	Language string `json:"language"`
}

func DefaultUISettings() UISettings {
	return UISettings{Version: 1, Language: LanguageSystem}
}

func NormalizeUILanguage(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", LanguageSystem:
		return LanguageSystem, true
	case LanguageChinese:
		return LanguageChinese, true
	case LanguageEnglish:
		return LanguageEnglish, true
	default:
		return "", false
	}
}

func LoadUISettings() (UISettings, error) {
	path, err := DefaultSettingsPath()
	if err != nil {
		return DefaultUISettings(), err
	}
	return LoadUISettingsFile(path)
}

func LoadUISettingsFile(path string) (UISettings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultUISettings(), nil
	}
	if err != nil {
		return DefaultUISettings(), err
	}
	settings := DefaultUISettings()
	if len(bytes.TrimSpace(b)) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		return DefaultUISettings(), nil
	}
	language, ok := NormalizeUILanguage(settings.Language)
	if !ok {
		language = LanguageSystem
	}
	settings.Version = 1
	settings.Language = language
	return settings, nil
}

func SaveUISettings(settings UISettings) error {
	path, err := DefaultSettingsPath()
	if err != nil {
		return err
	}
	return SaveUISettingsFile(path, settings)
}

func SaveUISettingsFile(path string, settings UISettings) error {
	language, ok := NormalizeUILanguage(settings.Language)
	if !ok {
		return fmt.Errorf("unsupported UI language %q", settings.Language)
	}
	settings.Version = 1
	settings.Language = language
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}
