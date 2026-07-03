package secret

import (
	"path/filepath"
	"testing"
)

func TestVaultStoreSetGetDelete(t *testing.T) {
	store := NewVaultStore(filepath.Join(t.TempDir(), "vault.json"), "correct horse battery staple")

	if err := store.Set("profile-1", KeySecurityToken, "token-value"); err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.Get("profile-1", KeySecurityToken)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "token-value" {
		t.Fatalf("unexpected vault lookup: value=%q ok=%v", value, ok)
	}
	if err := store.Delete("profile-1", KeySecurityToken); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get("profile-1", KeySecurityToken); err != nil || ok {
		t.Fatalf("expected vault secret to be deleted: ok=%v err=%v", ok, err)
	}
}
