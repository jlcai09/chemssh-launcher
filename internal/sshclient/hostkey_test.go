package sshclient

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"runtime"
	"testing"

	"chemweb-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

func TestHostKeyCallbackRequiresTrustBeforeAcceptingHost(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", dir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", dir)
	}

	profile := config.NewProfileDefaults()
	profile.SSHHost = "example.test"
	profile.SSHPort = 2222

	key := testPublicKey(t)
	changedKey := testPublicKey(t)
	hostname := net.JoinHostPort(profile.SSHHost, "2222")
	remoteAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}

	strict, err := hostKeyCallback(profile, HostKeyStrict)
	if err != nil {
		t.Fatal(err)
	}
	err = strict(hostname, remoteAddr, key)
	var verifyErr *HostKeyVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("expected HostKeyVerificationError, got %T: %v", err, err)
	}
	if verifyErr.Mismatch {
		t.Fatal("unknown host should not be reported as a mismatch")
	}

	acceptNew, err := hostKeyCallback(profile, HostKeyAcceptNew)
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptNew(hostname, remoteAddr, key); err != nil {
		t.Fatalf("accept new host key: %v", err)
	}

	path, err := config.DefaultKnownHostsPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("known_hosts was not written: %v", err)
	}

	strict, err = hostKeyCallback(profile, HostKeyStrict)
	if err != nil {
		t.Fatal(err)
	}
	if err := strict(hostname, remoteAddr, key); err != nil {
		t.Fatalf("known host should pass strict verification: %v", err)
	}

	err = strict(hostname, remoteAddr, changedKey)
	if !errors.As(err, &verifyErr) {
		t.Fatalf("expected changed key to fail with HostKeyVerificationError, got %T: %v", err, err)
	}
	if !verifyErr.Mismatch {
		t.Fatal("changed host key should be reported as a mismatch")
	}
}

func testPublicKey(t *testing.T) ssh.PublicKey {
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
