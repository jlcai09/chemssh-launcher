package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const AppName = "ChemSSHLauncher"

func DefaultDir() (string, error) {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("AppData"); appData != "" {
			return filepath.Join(appData, AppName), nil
		}
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		return filepath.Join(dir, "chemssh-launcher"), nil
	}
	return filepath.Join(dir, AppName), nil
}

func DefaultProfilesPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles.json"), nil
}

func DefaultVaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vault.json"), nil
}

func DefaultKnownHostsPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "known_hosts"), nil
}

func DefaultWebViewDataDir() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "webview2"), nil
}

func DefaultWebViewClearMarkerPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "webview2-clear-pending"), nil
}

func DefaultSFTPOpenCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err == nil && dir != "" {
		if runtime.GOOS == "linux" {
			return filepath.Join(dir, "chemssh-launcher", "sftp-open"), nil
		}
		return filepath.Join(dir, AppName, "sftp-open"), nil
	}
	fallback, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(fallback, "sftp-open-cache"), nil
}

func LegacyWebViewDataDirs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	appData := os.Getenv("AppData")
	if appData == "" {
		return nil
	}
	roots := []string{
		filepath.Join(appData, "chemssh-launcher-webview2.exe"),
		filepath.Join(appData, "chemssh-launcher.exe"),
	}
	seen := make(map[string]struct{}, len(roots))
	var dirs []string
	for _, root := range roots {
		clean := filepath.Clean(root)
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dirs = append(dirs, clean)
	}
	return dirs
}
