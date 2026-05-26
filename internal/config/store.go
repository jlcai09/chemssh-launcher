package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrProfileNotFound = errors.New("profile not found")

type ProfileStore interface {
	List() ([]Profile, error)
	Get(idOrName string) (Profile, error)
	Save(profile Profile) error
	Delete(idOrName string) error
}

type FileStore struct {
	Path string
}

type fileData struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}

func NewFileStore(path string) *FileStore {
	return &FileStore{Path: path}
}

func (s *FileStore) List() ([]Profile, error) {
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	if len(data.Profiles) == 0 {
		return []Profile{}, nil
	}
	return append([]Profile(nil), data.Profiles...), nil
}

func (s *FileStore) Get(idOrName string) (Profile, error) {
	data, err := s.load()
	if err != nil {
		return Profile{}, err
	}
	for _, p := range data.Profiles {
		if p.ID == idOrName || strings.EqualFold(p.Name, idOrName) {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, idOrName)
}

func (s *FileStore) Save(profile Profile) error {
	if profile.ID == "" {
		id, err := NewID()
		if err != nil {
			return err
		}
		profile.ID = id
	}

	data, err := s.load()
	if err != nil {
		return err
	}
	for i := range data.Profiles {
		if data.Profiles[i].ID == profile.ID {
			data.Profiles[i] = profile
			return s.save(data)
		}
	}
	data.Profiles = append(data.Profiles, profile)
	return s.save(data)
}

func (s *FileStore) Delete(idOrName string) error {
	data, err := s.load()
	if err != nil {
		return err
	}
	for i, p := range data.Profiles {
		if p.ID == idOrName || strings.EqualFold(p.Name, idOrName) {
			data.Profiles = append(data.Profiles[:i], data.Profiles[i+1:]...)
			return s.save(data)
		}
	}
	return fmt.Errorf("%w: %s", ErrProfileNotFound, idOrName)
}

func (s *FileStore) load() (fileData, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fileData{Version: 1}, nil
	}
	if err != nil {
		return fileData{}, err
	}
	if len(b) == 0 {
		return fileData{Version: 1}, nil
	}
	var data fileData
	if err := json.Unmarshal(b, &data); err != nil {
		return fileData{}, err
	}
	if data.Version == 0 {
		data.Version = 1
	}
	if data.Profiles == nil {
		data.Profiles = []Profile{}
	}
	return data, nil
}

func (s *FileStore) save(data fileData) error {
	if data.Version == 0 {
		data.Version = 1
	}
	if data.Profiles == nil {
		data.Profiles = []Profile{}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(s.Path, b, 0o600)
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
