package sshclient

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"chemssh-launcher/internal/config"
)

func TestX11LocalServerRequiresActualAuthority(t *testing.T) {
	server := startFakeXServer(t)
	settings := config.X11Config{Enabled: true, Display: server.display, XAuthority: filepath.Join(t.TempDir(), "missing")}
	if _, err := prepareLocalXServer(settings); err == nil || !strings.Contains(err.Error(), "Xauthority") {
		t.Fatalf("invented authority for an existing server: %v", err)
	}
	settings.XAuthority = fakeXAuthority(t, server)
	local, err := prepareLocalXServer(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(local.cookie, server.cookie) {
		t.Fatal("local credential changed")
	}
	local.Close()
	local.Close()
	d, _ := parseX11Display(server.display)
	if err := verifyXServer(d, server.cookie); err != nil {
		t.Fatalf("terminal close stopped an external server: %v", err)
	}
}
