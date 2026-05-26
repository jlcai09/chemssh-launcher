package secret

import "testing"

func TestMemoryStoreSetGetDelete(t *testing.T) {
	store := NewMemoryStore()

	if err := store.Set("profile-1", KeyPassword, "secret-value"); err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.Get("profile-1", KeyPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "secret-value" {
		t.Fatalf("unexpected secret lookup: value=%q ok=%v", value, ok)
	}
	if err := store.Delete("profile-1", KeyPassword); err != nil {
		t.Fatal(err)
	}
	_, ok, err = store.Get("profile-1", KeyPassword)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected secret to be deleted")
	}
}
