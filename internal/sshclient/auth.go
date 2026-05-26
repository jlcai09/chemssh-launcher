package sshclient

import (
	"fmt"
	"os"
	"time"

	"chemweb-launcher/internal/config"
	"chemweb-launcher/internal/secret"

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
