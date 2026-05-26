package config

import (
	"os"
	"path/filepath"
	"runtime"
)

const AppName = "ChemwebLauncher"

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
		return filepath.Join(dir, "chemweb-launcher"), nil
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
