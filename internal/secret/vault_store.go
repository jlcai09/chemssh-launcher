package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

const vaultVersion = 1

type VaultStore struct {
	Path           string
	MasterPassword string
}

type vaultFile struct {
	Version int               `json:"version"`
	Salt    string            `json:"salt"`
	Nonce   string            `json:"nonce"`
	Data    string            `json:"data"`
	Values  map[string]string `json:"-"`
}

func NewVaultStore(path, masterPassword string) *VaultStore {
	return &VaultStore{Path: path, MasterPassword: masterPassword}
}

func (s *VaultStore) Set(profileID, key, value string) error {
	values, err := s.load()
	if err != nil {
		return err
	}
	values[Username(profileID, key)] = value
	return s.save(values)
}

func (s *VaultStore) Get(profileID, key string) (string, bool, error) {
	values, err := s.load()
	if err != nil {
		return "", false, err
	}
	value, ok := values[Username(profileID, key)]
	return value, ok, nil
}

func (s *VaultStore) Delete(profileID, key string) error {
	values, err := s.load()
	if err != nil {
		return err
	}
	delete(values, Username(profileID, key))
	return s.save(values)
}

func (s *VaultStore) load() (map[string]string, error) {
	if s.MasterPassword == "" {
		return nil, errors.New("vault master password is required")
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}

	var vf vaultFile
	if err := json.Unmarshal(b, &vf); err != nil {
		return nil, err
	}
	if vf.Version != vaultVersion {
		return nil, fmt.Errorf("unsupported vault version %d", vf.Version)
	}

	salt, err := base64.StdEncoding.DecodeString(vf.Salt)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(vf.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(vf.Data)
	if err != nil {
		return nil, err
	}

	gcm, err := s.gcm(salt)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("could not decrypt vault")
	}

	values := map[string]string{}
	if len(plaintext) > 0 {
		if err := json.Unmarshal(plaintext, &values); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (s *VaultStore) save(values map[string]string) error {
	if s.MasterPassword == "" {
		return errors.New("vault master password is required")
	}
	salt := randomBytes(16)
	nonce := randomBytes(12)
	gcm, err := s.gcm(salt)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(values)
	if err != nil {
		return err
	}
	vf := vaultFile{
		Version: vaultVersion,
		Salt:    base64.StdEncoding.EncodeToString(salt),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
		Data:    base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)),
	}
	b, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.Path, b, 0o600)
}

func (s *VaultStore) gcm(salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(s.MasterPassword), salt, 3, 64*1024, 4, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randomBytes(size int) []byte {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func VaultPasswordFingerprint(masterPassword string) string {
	sum := sha256.Sum256([]byte(masterPassword))
	return base64.StdEncoding.EncodeToString(sum[:6])
}
