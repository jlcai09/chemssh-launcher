package sshclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"chemssh-launcher/internal/config"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// proxyBridgeVersion is part of the file contract with ChemSSH. Bump it only
// when the payload shape changes incompatibly.
const proxyBridgeVersion = 1

const proxyBridgeDirName = ".chemssh-launcher"

// ProxyBridgePath is where the launcher publishes the reverse proxy endpoint
// for ChemSSH's own terminal tabs, under the remote home directory.
func ProxyBridgePath(home string, remotePort int) string {
	return path.Join(home, proxyBridgeDirName, fmt.Sprintf("terminal-proxy-%d.json", remotePort))
}

type ProxyBridgeFile struct {
	Version  int               `json:"version"`
	ProxyURL string            `json:"proxy_url"`
	NoProxy  string            `json:"no_proxy"`
	Env      map[string]string `json:"env"`
}

func newProxyBridgeFile(proxyURL, noProxy string) ProxyBridgeFile {
	env := make(map[string]string)
	for _, pair := range config.ProxyEnvPairs(proxyURL, noProxy) {
		env[pair.Key] = pair.Value
	}
	return ProxyBridgeFile{
		Version:  proxyBridgeVersion,
		ProxyURL: proxyURL,
		NoProxy:  noProxy,
		Env:      env,
	}
}

// WriteProxyBridge publishes the reverse proxy endpoint on the remote server so
// ChemSSH terminal tabs opened while the session runs inherit it. It returns
// the remote path for removal when the session stops.
func WriteProxyBridge(client *ssh.Client, remotePort int, proxyURL, noProxy string) (string, error) {
	if client == nil {
		return "", errors.New("ssh client is required to publish the proxy bridge")
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return "", fmt.Errorf("open sftp session: %w", err)
	}
	defer sftpClient.Close()

	home, err := sftpClient.Getwd()
	if err != nil || strings.TrimSpace(home) == "" {
		if err != nil {
			return "", fmt.Errorf("resolve remote home: %w", err)
		}
		return "", errors.New("resolve remote home: empty path")
	}
	dir := path.Join(home, proxyBridgeDirName)
	if err := sftpClient.MkdirAll(dir); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	target := ProxyBridgePath(home, remotePort)
	payload, err := json.MarshalIndent(newProxyBridgeFile(proxyURL, noProxy), "", "  ")
	if err != nil {
		return "", err
	}
	payload = append(payload, '\n')

	file, err := sftpClient.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	_ = sftpClient.Chmod(target, 0o600)
	return target, nil
}

// RemoveProxyBridge deletes a previously published bridge file. A missing file
// is not an error because another stop path may already have cleaned it up.
func RemoveProxyBridge(client *ssh.Client, remotePath string) error {
	if client == nil || strings.TrimSpace(remotePath) == "" {
		return nil
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("open sftp session: %w", err)
	}
	defer sftpClient.Close()
	if err := sftpClient.Remove(remotePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", remotePath, err)
	}
	return nil
}
