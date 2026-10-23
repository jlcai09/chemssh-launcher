package gui

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"chemssh-launcher/internal/config"
	appruntime "chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestHandleStartReturnsHostKeyConfirmationBeforeQueueing(t *testing.T) {
	configDir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", configDir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", configDir)
	}

	sshAddress, _, closeSSH := startMinimalSSHServer(t)
	defer closeSSH()
	host, portText, err := net.SplitHostPort(sshAddress)
	if err != nil {
		t.Fatal(err)
	}
	sshPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	profile := config.NewProfileDefaults()
	profile.ID = "profile-1"
	profile.Name = "Unknown Host"
	profile.SSHHost = host
	profile.SSHPort = sshPort
	profile.SSHUser = "user"
	profile.AuthMethod = config.AuthPassword
	profile.HasPassword = true
	profile.LocalPort = freeLocalPort(t)

	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	if err := profiles.Save(profile); err != nil {
		t.Fatal(err)
	}
	secrets := secret.NewMemoryStore()
	if err := secrets.Set(profile.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})

	body := bytes.NewBufferString(`{"id":"profile-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/session/start", body)
	rec := httptest.NewRecorder()

	server.handleStart(rec, req)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusPreconditionRequired, rec.Body.String())
	}
	var got struct {
		HostKey struct {
			Address     string `json:"address"`
			Fingerprint string `json:"fingerprint"`
			Mismatch    bool   `json:"mismatch"`
		} `json:"host_key"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.HostKey.Address == "" || got.HostKey.Fingerprint == "" {
		t.Fatalf("missing host key confirmation details: %+v", got.HostKey)
	}
	if got.HostKey.Mismatch {
		t.Fatal("unknown host key was reported as a mismatch")
	}
}

func TestHandleProfileTestReplacesChangedHostKey(t *testing.T) {
	configDir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", configDir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", configDir)
	}

	sshAddress, serverKey, closeSSH := startMinimalSSHServer(t)
	defer closeSSH()
	host, portText, err := net.SplitHostPort(sshAddress)
	if err != nil {
		t.Fatal(err)
	}
	sshPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	profile := config.NewProfileDefaults()
	profile.ID = "profile-1"
	profile.Name = "Changed Host"
	profile.SSHHost = host
	profile.SSHPort = sshPort
	profile.SSHUser = "user"
	profile.AuthMethod = config.AuthPassword
	profile.HasPassword = true

	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	if err := profiles.Save(profile); err != nil {
		t.Fatal(err)
	}
	secrets := secret.NewMemoryStore()
	if err := secrets.Set(profile.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}

	oldKey := guiTestPublicKey(t)
	knownHostsPath, err := config.DefaultKnownHostsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(sshAddress)}, oldKey)
	if err := os.WriteFile(knownHostsPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})
	body := bytes.NewBufferString(`{"id":"profile-1","replace_host_key":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/profile-test", body)
	rec := httptest.NewRecorder()

	server.handleProfileTest(rec, req)

	if rec.Code == http.StatusPreconditionRequired {
		t.Fatalf("replace_host_key did not replace the saved key; body: %s", rec.Body.String())
	}
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP(host), Port: sshPort}
	if err := callback(sshAddress, remote, serverKey); err != nil {
		t.Fatalf("new host key was not saved: %v", err)
	}
	if err := callback(sshAddress, remote, oldKey); err == nil {
		t.Fatal("old host key is still trusted after replacement")
	}
}

func guiTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func startMinimalSSHServer(t *testing.T) (string, ssh.PublicKey, func()) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveMinimalSSHConn(conn, cfg)
		}
	}()
	return ln.Addr().String(), signer.PublicKey(), func() {
		_ = ln.Close()
		<-done
	}
}

func serveMinimalSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		_ = ch.Reject(ssh.UnknownChannelType, "test server does not run sessions")
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
