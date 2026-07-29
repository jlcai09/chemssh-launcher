package sshclient

import (
	"testing"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
)

// fakeSecretStore is a minimal in-memory secret.Store used by auth tests.
type fakeSecretStore struct {
	secrets map[string]string
}

func (f fakeSecretStore) Set(id, key, value string) error {
	f.secrets[id+":"+key] = value
	return nil
}

func (f fakeSecretStore) Get(id, key string) (string, bool, error) {
	v, ok := f.secrets[id+":"+key]
	return v, ok, nil
}

func (f fakeSecretStore) Delete(id, key string) error {
	delete(f.secrets, id+":"+key)
	return nil
}

// TestPasswordAuthRegistersBothPasswordAndKeyboardInteractive ensures that a
// password profile advertises both the RFC 4252 password method and the RFC
// 4256 keyboard-interactive method. Servers that only allow
// keyboard-interactive (common on HPC login nodes) must still be able to
// authenticate.
func TestPasswordAuthRegistersBothPasswordAndKeyboardInteractive(t *testing.T) {
	store := fakeSecretStore{secrets: map[string]string{}}
	if err := store.Set("p1", secret.KeyPassword, "hunter2"); err != nil {
		t.Fatal(err)
	}

	profile := config.NewProfileDefaults()
	profile.ID = "p1"
	profile.Name = "test"
	profile.AuthMethod = config.AuthPassword

	cfg, err := ClientConfigWithHostKeyPolicy(profile, store, HostKeyAcceptNew)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Auth) != 2 {
		t.Fatalf("expected 2 auth methods (password + keyboard-interactive), got %d", len(cfg.Auth))
	}
}

// TestKeyboardInteractivePasswordReturnsPasswordForEveryChallenge verifies the
// keyboard-interactive callback answers each challenge with the stored password.
func TestKeyboardInteractivePasswordReturnsPasswordForEveryChallenge(t *testing.T) {
	cb := keyboardInteractiveCallback("hunter2")
	answers, err := cb("name", "instruction", []string{"q1", "q2", "q3"}, []bool{false, false, false})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 3 {
		t.Fatalf("expected 3 answers, got %d", len(answers))
	}
	for i, a := range answers {
		if a != "hunter2" {
			t.Fatalf("answer %d = %q, want %q", i, a, "hunter2")
		}
	}
}
