package gui

import (
	"testing"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
)

func TestSaveProfileIgnoresEmptyPasswordReplace(t *testing.T) {
	server, _, secrets := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	p.LocalPort = freeLocalPort(t)

	if err := server.saveProfileWithSecrets(p, secretRequest{PasswordAction: "replace", Password: "password"}); err != nil {
		t.Fatal(err)
	}

	// The editor sends "replace" with an empty value when the field was cleared;
	// that must not overwrite the stored password with an empty string.
	if err := server.saveProfileWithSecrets(p, secretRequest{PasswordAction: "replace", Password: ""}); err != nil {
		t.Fatal(err)
	}

	value, ok, err := secrets.Get(p.ID, secret.KeyPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "password" {
		t.Fatalf("stored password was clobbered: ok=%v value=%q", ok, value)
	}
	saved, err := server.rt.Profiles.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.HasPassword {
		t.Fatalf("password flag was dropped even though the secret exists: %+v", saved)
	}
}

func TestSaveProfileDropsPasswordFlagWithoutSecret(t *testing.T) {
	server, profiles, _ := newProfileTestServer(t)
	p := sampleRemoteProfile("Copied Server")
	p.LocalPort = freeLocalPort(t)
	p.HasPassword = true
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}

	// A duplicated or hand-edited profile can claim a saved password while the
	// secret store holds nothing usable; the flag must not survive the save.
	if err := server.saveProfileWithSecrets(p, secretRequest{PasswordAction: "keep"}); err != nil {
		t.Fatal(err)
	}

	saved, err := profiles.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HasPassword {
		t.Fatalf("stale password flag survived the save: %+v", saved)
	}
}

func TestSaveProfileClearsPasswordWithExplicitClear(t *testing.T) {
	server, profiles, secrets := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	p.LocalPort = freeLocalPort(t)

	if err := server.saveProfileWithSecrets(p, secretRequest{PasswordAction: "replace", Password: "password"}); err != nil {
		t.Fatal(err)
	}
	if err := server.saveProfileWithSecrets(p, secretRequest{PasswordAction: "clear"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := secrets.Get(p.ID, secret.KeyPassword); err != nil || ok {
		t.Fatalf("password survived an explicit clear: ok=%v err=%v", ok, err)
	}
	saved, err := profiles.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HasPassword {
		t.Fatalf("password flag kept after an explicit clear: %+v", saved)
	}
}

func TestSaveProfileIgnoresEmptySecurityTokenReplace(t *testing.T) {
	server, _, secrets := newProfileTestServer(t)
	p := sampleRemoteProfile("Example Server")
	p.LocalPort = freeLocalPort(t)

	if err := server.saveProfileWithSecrets(p, secretRequest{SecurityTokenAction: "replace", SecurityToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := server.saveProfileWithSecrets(p, secretRequest{SecurityTokenAction: "replace", SecurityToken: ""}); err != nil {
		t.Fatal(err)
	}
	value, ok, err := secrets.Get(p.ID, secret.KeySecurityToken)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "token" {
		t.Fatalf("stored security token was clobbered: ok=%v value=%q", ok, value)
	}
}

func TestSaveLocalProfileDropsEmptySecurityToken(t *testing.T) {
	server, profiles, secrets := newProfileTestServer(t)
	p := config.NewLocalProfileDefaults()
	p.ID = "local-profile"
	p.LocalPort = freeLocalPort(t)
	p.HasSecurityToken = true
	if err := secrets.Set(p.ID, secret.KeySecurityToken, ""); err != nil {
		t.Fatal(err)
	}

	if err := server.saveProfileWithSecrets(p, secretRequest{}); err != nil {
		t.Fatal(err)
	}
	saved, err := profiles.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HasSecurityToken {
		t.Fatalf("empty security token was reported as saved: %+v", saved)
	}
	if saved.Kind != config.ProfileKindLocal {
		t.Fatalf("local profile kind was changed: %+v", saved)
	}
}
