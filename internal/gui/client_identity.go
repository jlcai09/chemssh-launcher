package gui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"chemssh-launcher/internal/config"
)

const (
	launcherClientIdentityVersion = 1
	launcherClientIDPrefix        = "client_launcher_"
)

var launcherClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

type launcherClientIdentity struct {
	Version   int    `json:"version"`
	ClientID  string `json:"client_id"`
	CreatedAt string `json:"created_at"`
}

type chemSSHBridgeClientIdentity struct {
	Enabled  bool   `json:"enabled"`
	Version  int    `json:"version"`
	ClientID string `json:"client_id"`
	Source   string `json:"source"`
}

func loadOrCreateDefaultLauncherClientIdentity() (launcherClientIdentity, error) {
	path, err := config.DefaultClientIdentityPath()
	if err != nil {
		return launcherClientIdentity{}, err
	}
	return loadOrCreateLauncherClientIdentity(path)
}

func loadOrCreateLauncherClientIdentity(path string) (launcherClientIdentity, error) {
	identity, err := readLauncherClientIdentity(path)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return launcherClientIdentity{}, err
	}
	identity, err = newLauncherClientIdentity()
	if err != nil {
		return launcherClientIdentity{}, err
	}
	if err := writeLauncherClientIdentity(path, identity); err != nil {
		return launcherClientIdentity{}, err
	}
	return identity, nil
}

func readLauncherClientIdentity(path string) (launcherClientIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return launcherClientIdentity{}, err
	}
	var identity launcherClientIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return launcherClientIdentity{}, err
	}
	if err := validateLauncherClientIdentity(identity); err != nil {
		return launcherClientIdentity{}, err
	}
	return identity, nil
}

func newLauncherClientIdentity() (launcherClientIdentity, error) {
	id, err := config.NewID()
	if err != nil {
		return launcherClientIdentity{}, err
	}
	return launcherClientIdentity{
		Version:   launcherClientIdentityVersion,
		ClientID:  launcherClientIDPrefix + id,
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
	}, nil
}

func writeLauncherClientIdentity(path string, identity launcherClientIdentity) error {
	if err := validateLauncherClientIdentity(identity); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validateLauncherClientIdentity(identity launcherClientIdentity) error {
	if identity.Version != launcherClientIdentityVersion {
		return errors.New("unsupported launcher client identity version")
	}
	if !launcherClientIDPattern.MatchString(identity.ClientID) {
		return errors.New("invalid launcher client id")
	}
	if identity.CreatedAt == "" {
		return errors.New("launcher client identity created_at is required")
	}
	return nil
}

func (s *Server) ensureLauncherClientIdentity() (launcherClientIdentity, error) {
	s.mu.Lock()
	if s.clientIdentity.ClientID != "" {
		identity := s.clientIdentity
		s.mu.Unlock()
		return identity, nil
	}
	s.mu.Unlock()

	identity, err := loadOrCreateDefaultLauncherClientIdentity()
	if err != nil {
		return launcherClientIdentity{}, err
	}

	s.mu.Lock()
	if s.clientIdentity.ClientID == "" {
		s.clientIdentity = identity
	} else {
		identity = s.clientIdentity
	}
	s.mu.Unlock()
	return identity, nil
}

func (s *Server) launcherClientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientIdentity.ClientID
}
