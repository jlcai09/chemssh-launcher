package sshclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type HostKeyPolicy int

const (
	HostKeyStrict HostKeyPolicy = iota
	HostKeyAcceptNew
)

type HostKeyVerificationError struct {
	Host           string
	Address        string
	KeyType        string
	Fingerprint    string
	KnownHostsPath string
	Mismatch       bool
	Err            error
}

func (e *HostKeyVerificationError) Error() string {
	if e.Mismatch {
		return fmt.Sprintf("ssh host key mismatch for %s (%s %s); possible man-in-the-middle attack or server reinstallation. Check %s before trusting this host again", e.Address, e.KeyType, e.Fingerprint, e.KnownHostsPath)
	}
	return fmt.Sprintf("unknown ssh host key for %s (%s %s). Confirm this fingerprint with the server administrator, then trust it to save it in %s", e.Address, e.KeyType, e.Fingerprint, e.KnownHostsPath)
}

func (e *HostKeyVerificationError) Unwrap() error {
	return e.Err
}

var knownHostsMu sync.Mutex

func hostKeyCallback(profile config.Profile, policy HostKeyPolicy) (ssh.HostKeyCallback, error) {
	path, err := config.DefaultKnownHostsPath()
	if err != nil {
		return nil, err
	}
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, err
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(profile.SSHHost, strconv.Itoa(profile.SSHPort))
	normalizedAddress := knownhosts.Normalize(address)

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := callback(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}

		mismatch := len(keyErr.Want) > 0
		verificationErr := &HostKeyVerificationError{
			Host:           profile.SSHHost,
			Address:        normalizedAddress,
			KeyType:        key.Type(),
			Fingerprint:    ssh.FingerprintSHA256(key),
			KnownHostsPath: path,
			Mismatch:       mismatch,
			Err:            err,
		}
		if mismatch || policy != HostKeyAcceptNew {
			return verificationErr
		}
		if err := appendKnownHost(path, normalizedAddress, key); err != nil {
			return err
		}
		return nil
	}, nil
}

func ensureKnownHostsFile(path string) error {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func appendKnownHost(path, address string, key ssh.PublicKey) error {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintln(file, knownhosts.Line([]string{address}, key))
	return err
}
