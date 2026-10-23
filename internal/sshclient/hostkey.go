package sshclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type HostKeyPolicy int

const (
	HostKeyStrict HostKeyPolicy = iota
	HostKeyAcceptNew
	HostKeyReplaceChanged
)

type HostKeyVerificationError struct {
	Host                 string
	Address              string
	KeyType              string
	Fingerprint          string
	KnownHostsPath       string
	PreviousFingerprints []string
	Mismatch             bool
	Err                  error
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
		previous := make([]string, 0, len(keyErr.Want))
		for _, known := range keyErr.Want {
			previous = append(previous, ssh.FingerprintSHA256(known.Key))
		}
		verificationErr := &HostKeyVerificationError{
			Host:                 profile.SSHHost,
			Address:              normalizedAddress,
			KeyType:              key.Type(),
			Fingerprint:          ssh.FingerprintSHA256(key),
			KnownHostsPath:       path,
			PreviousFingerprints: previous,
			Mismatch:             mismatch,
			Err:                  err,
		}
		if policy == HostKeyStrict {
			return verificationErr
		}
		if mismatch {
			if policy != HostKeyReplaceChanged {
				return verificationErr
			}
			return replaceKnownHost(path, normalizedAddress, key, keyErr.Want)
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

func replaceKnownHost(path, address string, key ssh.PublicKey, stale []knownhosts.KnownKey) error {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()

	drop := make(map[int]struct{}, len(stale))
	for _, entry := range stale {
		if entry.Key == nil || entry.Key.Type() != key.Type() || entry.Filename != path || entry.Line <= 0 {
			continue
		}
		drop[entry.Line] = struct{}{}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines)+1)
	for index, line := range lines {
		if _, removed := drop[index+1]; removed {
			continue
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	kept = append(kept, knownhosts.Line([]string{address}, key))
	return writeKnownHosts(path, strings.Join(kept, "\n")+"\n")
}

func writeKnownHosts(path, content string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
