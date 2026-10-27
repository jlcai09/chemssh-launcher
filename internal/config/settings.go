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

const (
	DefaultTerminalFontSize   = 13
	MinTerminalFontSize       = 10
	MaxTerminalFontSize       = 24
	DefaultTerminalScrollback = 5000
	MinTerminalScrollback     = 1000
	MaxTerminalScrollback     = 50000
)

type UISettings struct {
	Version            int    `json:"version"`
	Language           string `json:"language"`
	TerminalFontSize   int    `json:"terminal_font_size"`
	TerminalScrollback int    `json:"terminal_scrollback"`
}

func DefaultUISettings() UISettings {
	return UISettings{
		Version:            1,
		Language:           LanguageSystem,
		TerminalFontSize:   DefaultTerminalFontSize,
		TerminalScrollback: DefaultTerminalScrollback,
	}
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
	return normalizeUISettings(settings), nil
}

func normalizeUISettings(settings UISettings) UISettings {
	settings.Version = 1
	language, ok := NormalizeUILanguage(settings.Language)
	if !ok {
		language = LanguageSystem
	}
	settings.Language = language
	settings.TerminalFontSize = normalizeLoadedRange(settings.TerminalFontSize, DefaultTerminalFontSize, MinTerminalFontSize, MaxTerminalFontSize)
	settings.TerminalScrollback = normalizeLoadedRange(settings.TerminalScrollback, DefaultTerminalScrollback, MinTerminalScrollback, MaxTerminalScrollback)
	return settings
}

func normalizeLoadedRange(value, fallback, min, max int) int {
	if value == 0 {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
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
	if settings.TerminalFontSize == 0 {
		settings.TerminalFontSize = DefaultTerminalFontSize
	}
	if settings.TerminalFontSize < MinTerminalFontSize || settings.TerminalFontSize > MaxTerminalFontSize {
		return fmt.Errorf("terminal font size must be between %d and %d", MinTerminalFontSize, MaxTerminalFontSize)
	}
	if settings.TerminalScrollback == 0 {
		settings.TerminalScrollback = DefaultTerminalScrollback
	}
	if settings.TerminalScrollback < MinTerminalScrollback || settings.TerminalScrollback > MaxTerminalScrollback {
		return fmt.Errorf("terminal scrollback must be between %d and %d", MinTerminalScrollback, MaxTerminalScrollback)
	}
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
