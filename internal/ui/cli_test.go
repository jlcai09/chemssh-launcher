package ui

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"

	"golang.org/x/term"
)

func TestPromptStringClearsOptionalValueWithDash(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("-\n"))
	out := &bytes.Buffer{}
	got := promptString(reader, out, "Health check URL", "http://127.0.0.1:8888/")
	if got != "" {
		t.Fatalf("promptString returned %q, want empty", got)
	}
}

func TestPromptStringKeepsCurrentValueOnEmptyInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\n"))
	out := &bytes.Buffer{}
	got := promptString(reader, out, "Health check URL", "http://127.0.0.1:8888/")
	if got != "http://127.0.0.1:8888/" {
		t.Fatalf("promptString returned %q, want the current value", got)
	}
}

func TestPromptMultilineClearsCommandsWithDash(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("-\n"))
	out := &bytes.Buffer{}
	got := promptMultiline(reader, out, "Pre-start commands", "cd /home/user/project")
	if got != "" {
		t.Fatalf("promptMultiline returned %q, want empty", got)
	}
}

func TestPromptMultilineKeepsCommandsOnSingleDot(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(".\n"))
	out := &bytes.Buffer{}
	got := promptMultiline(reader, out, "Pre-start commands", "cd /home/user/project")
	if got != "cd /home/user/project" {
		t.Fatalf("promptMultiline returned %q, want the current value", got)
	}
}

func TestReadSecretConsumesThePassedReader(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal; readSecret would intentionally bypass the injected reader")
	}
	reader := bufio.NewReader(strings.NewReader("secret-value\nafter\n"))
	value, err := readSecret(reader, io.Discard, "Password")
	if err != nil {
		t.Fatal(err)
	}
	if value != "secret-value" {
		t.Fatalf("readSecret returned %q, want secret-value", value)
	}
	if next := readLine(reader); next != "after" {
		t.Fatalf("readSecret did not consume from the passed reader; next line = %q", next)
	}
}

func TestDeleteProfileRequiresConfirmation(t *testing.T) {
	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	p := config.NewProfileDefaults()
	p.ID = "profile-1"
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	if err := profiles.Save(p); err != nil {
		t.Fatal(err)
	}
	secrets := secret.NewMemoryStore()
	if err := secrets.Set(p.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}

	if err := deleteProfile(p.ID, strings.NewReader("n\n"), out, profiles, secrets); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Get(p.ID); err != nil {
		t.Fatalf("cancelled delete removed the profile: %v", err)
	}
	if _, ok, err := secrets.Get(p.ID, secret.KeyPassword); err != nil || !ok {
		t.Fatalf("cancelled delete removed the secret: ok=%v err=%v", ok, err)
	}

	if err := deleteProfile(p.ID, strings.NewReader("y\n"), out, profiles, secrets); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Get(p.ID); !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("confirmed delete did not remove the profile: %v", err)
	}
	if _, ok, err := secrets.Get(p.ID, secret.KeyPassword); err != nil || ok {
		t.Fatalf("confirmed delete retained the secret: ok=%v err=%v", ok, err)
	}
}
