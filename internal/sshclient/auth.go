package sshclient

import (
	"fmt"
	"os"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
)

func ClientConfig(profile config.Profile, secrets secret.Store) (*ssh.ClientConfig, error) {
	return ClientConfigWithHostKeyPolicy(profile, secrets, HostKeyStrict)
}

func ClientConfigWithHostKeyPolicy(profile config.Profile, secrets secret.Store, policy HostKeyPolicy) (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod

	switch profile.AuthMethod {
	case config.AuthPassword:
		password, ok, err := secrets.Get(profile.ID, secret.KeyPassword)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("password is not set for profile %q", profile.Name)
		}
		auths = append(auths, ssh.Password(password))
		auths = append(auths, keyboardInteractivePassword(password))
	case config.AuthPrivateKey:
		key, err := os.ReadFile(profile.PrivateKeyPath)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if profile.HasPrivateKeyPassphrase {
			passphrase, ok, err := secrets.Get(profile.ID, secret.KeyPrivatePassphrase)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("private key passphrase is not set for profile %q", profile.Name)
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	default:
		return nil, fmt.Errorf("unsupported auth method %q", profile.AuthMethod)
	}
	hostKeyCallback, err := hostKeyCallback(profile, policy)
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:            profile.SSHUser,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}, nil
}

// keyboardInteractivePassword returns an ssh.AuthMethod that answers every
// keyboard-interactive challenge with the stored password. Many servers
// (e.g. some HPC login nodes) only advertise keyboard-interactive even when
// the user intends to log in with a plain password. Mirrors the behavior of
// Xshell, MobaXterm and the OpenSSH client, which fall back to
// keyboard-interactive automatically when the server rejects password auth.
func keyboardInteractivePassword(password string) ssh.AuthMethod {
	return ssh.KeyboardInteractive(keyboardInteractiveCallback(password))
}

// keyboardInteractiveCallback builds the challenge-response function used by
// keyboardInteractivePassword. Split out so it can be unit tested directly,
// since ssh.AuthMethod is an opaque interface and cannot be invoked from
// tests.
func keyboardInteractiveCallback(password string) func(name, instruction string, questions []string, echos []bool) ([]string, error) {
	return func(name, instruction string, questions []string, echos []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range questions {
			answers[i] = password
		}
		return answers, nil
	}
}
