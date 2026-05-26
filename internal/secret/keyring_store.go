package secret

import (
	"errors"

	"github.com/zalando/go-keyring"
)

type KeyringStore struct {
	Service string
}

func NewKeyringStore() *KeyringStore {
	return &KeyringStore{Service: ServiceName}
}

func (s *KeyringStore) Set(profileID, key, value string) error {
	return keyring.Set(s.Service, Username(profileID, key), value)
}

func (s *KeyringStore) Get(profileID, key string) (string, bool, error) {
	value, err := keyring.Get(s.Service, Username(profileID, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *KeyringStore) Delete(profileID, key string) error {
	err := keyring.Delete(s.Service, Username(profileID, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
