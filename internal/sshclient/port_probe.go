package sshclient

import (
	"fmt"
	"strings"

	"chemweb-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

func IsRemotePortOccupied(client *ssh.Client, profile config.Profile) (bool, error) {
	conn, err := client.Dial("tcp", profile.RemoteAddress())
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if isConnectionRefused(err) {
		return false, nil
	}
	return false, fmt.Errorf("remote port probe failed for %s: %w", profile.RemoteAddress(), err)
}

func CheckRemotePortAvailable(client *ssh.Client, profile config.Profile) error {
	occupied, err := IsRemotePortOccupied(client, profile)
	if err != nil {
		return err
	}
	if occupied {
		return fmt.Errorf("remote port occupied: %s is already accepting TCP connections", profile.RemoteAddress())
	}
	return nil
}

func isConnectionRefused(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection refused") ||
		strings.Contains(text, "connect failed") ||
		strings.Contains(text, "actively refused")
}
