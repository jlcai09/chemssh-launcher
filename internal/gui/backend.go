package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"chemssh-launcher/internal/config"
)

type importedProfilesFile struct {
	Version  int              `json:"version"`
	Profiles []config.Profile `json:"profiles"`
}

type backendOverview struct {
	ConfigDir          string            `json:"config_dir"`
	ProfilesPath       string            `json:"profiles_path"`
	WebViewDataDir     string            `json:"webview_data_dir"`
	SFTPOpenCacheDir   string            `json:"sftp_open_cache_dir"`
	ClearPending       bool              `json:"clear_pending"`
	ManagedCacheExists bool              `json:"managed_cache_exists"`
	CacheEntries       []backendCacheDir `json:"cache_entries"`
}

type backendCacheDir struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Exists bool   `json:"exists"`
}

func exportProfiles(w io.Writer, profiles []config.Profile) error {
	payload := importedProfilesFile{
		Version:  1,
		Profiles: profiles,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func importProfilesFromReader(r io.Reader) ([]config.Profile, error) {
	var payload importedProfilesFile
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload.Profiles) == 0 {
		return nil, errors.New("no profiles found in import file")
	}

	seen := make(map[string]struct{}, len(payload.Profiles))
	result := make([]config.Profile, 0, len(payload.Profiles))
	for _, profile := range payload.Profiles {
		normalized := normalizeImportedProfile(profile)
		if normalized.ID == "" {
			id, err := config.NewID()
			if err != nil {
				return nil, err
			}
			normalized.ID = id
		}
		if _, ok := seen[normalized.ID]; ok {
			return nil, fmt.Errorf("duplicate profile id in import: %s", normalized.ID)
		}
		seen[normalized.ID] = struct{}{}
		result = append(result, normalized)
	}
	return result, nil
}

func normalizeImportedProfile(profile config.Profile) config.Profile {
	defaults := config.NewProfileDefaults()
	if profile.Kind == "" {
		profile.Kind = defaults.Kind
	}
	if profile.SSHPort == 0 {
		profile.SSHPort = defaults.SSHPort
	}
	if profile.AuthMethod == "" {
		profile.AuthMethod = defaults.AuthMethod
	}
	if profile.RemoteHost == "" {
		profile.RemoteHost = defaults.RemoteHost
	}
	if profile.RemotePort == 0 {
		profile.RemotePort = defaults.RemotePort
	}
	if profile.LocalHost == "" {
		profile.LocalHost = defaults.LocalHost
	}
	if profile.LocalPort == 0 {
		profile.LocalPort = defaults.LocalPort
	}
	if profile.LocalURLPath == "" {
		profile.LocalURLPath = defaults.LocalURLPath
	}
	if strings.TrimSpace(profile.StartCommand) == "" {
		profile.StartCommand = defaults.StartCommand
	}
	if profile.IsLocal() {
		localDefaults := config.NewLocalProfileDefaults()
		if profile.LocalHost == "" {
			profile.LocalHost = localDefaults.LocalHost
		}
		if profile.LocalPort == 0 {
			profile.LocalPort = localDefaults.LocalPort
		}
		if profile.LocalURLPath == "" {
			profile.LocalURLPath = localDefaults.LocalURLPath
		}
		profile.SSHHost = ""
		profile.SSHPort = 0
		profile.SSHUser = ""
		profile.AuthMethod = ""
		profile.PrivateKeyPath = ""
		profile.RemoteHost = ""
		profile.RemotePort = 0
		if strings.TrimSpace(profile.StartCommand) == defaults.StartCommand {
			profile.StartCommand = ""
		}
	}
	profile.HasPassword = false
	profile.HasPrivateKeyPassphrase = false
	return profile
}

func backendInfo() (backendOverview, error) {
	configDir, err := config.DefaultDir()
	if err != nil {
		return backendOverview{}, err
	}
	profilesPath, err := config.DefaultProfilesPath()
	if err != nil {
		return backendOverview{}, err
	}
	webViewDataDir, err := config.DefaultWebViewDataDir()
	if err != nil {
		return backendOverview{}, err
	}
	sftpOpenCacheDir, err := config.DefaultSFTPOpenCacheDir()
	if err != nil {
		return backendOverview{}, err
	}
	clearMarker, err := config.DefaultWebViewClearMarkerPath()
	if err != nil {
		return backendOverview{}, err
	}
	_, clearPendingErr := os.Stat(clearMarker)
	clearPending := clearPendingErr == nil
	_, managedCacheErr := os.Stat(webViewDataDir)
	_, sftpOpenCacheErr := os.Stat(sftpOpenCacheDir)

	entries := []backendCacheDir{
		{
			Path:   webViewDataDir,
			Kind:   "managed",
			Exists: managedCacheErr == nil,
		},
		{
			Path:   sftpOpenCacheDir,
			Kind:   "sftp-open",
			Exists: sftpOpenCacheErr == nil,
		},
	}
	for _, legacy := range config.LegacyWebViewDataDirs() {
		_, err := os.Stat(legacy)
		entries = append(entries, backendCacheDir{
			Path:   legacy,
			Kind:   "legacy",
			Exists: err == nil,
		})
	}

	return backendOverview{
		ConfigDir:          configDir,
		ProfilesPath:       profilesPath,
		WebViewDataDir:     webViewDataDir,
		SFTPOpenCacheDir:   sftpOpenCacheDir,
		ClearPending:       clearPending,
		ManagedCacheExists: managedCacheErr == nil,
		CacheEntries:       entries,
	}, nil
}

func clearBackendCache() (backendOverview, []string, error) {
	info, err := backendInfo()
	if err != nil {
		return backendOverview{}, nil, err
	}

	var messages []string
	var pending bool
	var cleared bool
	for _, entry := range info.CacheEntries {
		if entry.Kind != "legacy" && entry.Kind != "sftp-open" {
			continue
		}
		if !entry.Exists {
			continue
		}
		if err := os.RemoveAll(entry.Path); err != nil {
			pending = true
			messages = append(messages, fmt.Sprintf("暂时无法清理 %s：%v", entry.Path, err))
			continue
		}
		cleared = true
		messages = append(messages, "已清理 "+entry.Path)
	}
	if !cleared && !pending {
		messages = append(messages, "没有发现可清理的后台缓存目录")
	}

	clearMarker, err := config.DefaultWebViewClearMarkerPath()
	if err != nil {
		return backendOverview{}, messages, err
	}
	if pending {
		if err := os.WriteFile(clearMarker, []byte("pending\n"), 0o600); err != nil {
			return backendOverview{}, messages, err
		}
		messages = append(messages, "部分缓存文件正在使用中，下次启动时会继续尝试清理")
	} else if err := os.Remove(clearMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return backendOverview{}, messages, err
	}

	updated, err := backendInfo()
	if err != nil {
		return backendOverview{}, messages, err
	}
	return updated, messages, nil
}

func applyPendingCacheCleanup(logs *safeLog) {
	clearMarker, err := config.DefaultWebViewClearMarkerPath()
	if err != nil {
		if logs != nil {
			logs.add("warning: resolve cache cleanup marker: " + err.Error())
		}
		return
	}
	if _, err := os.Stat(clearMarker); errors.Is(err, os.ErrNotExist) {
		return
	}

	var failed []string
	if dir, err := config.DefaultSFTPOpenCacheDir(); err != nil {
		failed = append(failed, fmt.Sprintf("SFTP file cache (%v)", err))
	} else if dir != "" {
		if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, fmt.Sprintf("%s (%v)", dir, err))
		}
	}
	for _, dir := range config.LegacyWebViewDataDirs() {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, fmt.Sprintf("%s (%v)", dir, err))
		}
	}
	if len(failed) > 0 {
		if logs != nil {
			logs.add("warning: pending cache cleanup still blocked: " + strings.Join(failed, "; "))
		}
		return
	}
	_ = os.Remove(clearMarker)
	if logs != nil {
		logs.add("pending backend cache cleanup completed")
	}
}

func cleanupSFTPOpenCache(logs *safeLog) {
	dir, err := config.DefaultSFTPOpenCacheDir()
	if err != nil {
		if logs != nil {
			logs.add("warning: resolve SFTP file cache directory: " + err.Error())
		}
		return
	}
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if logs != nil {
			logs.add("warning: cleanup SFTP file cache: " + err.Error())
		}
		return
	}
	if logs != nil {
		logs.add("SFTP file cache cleaned: " + dir)
	}
}

func ensureParentDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
