package sshclient

import (
	"context"
	"fmt"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

// LocalProxyChannel is the reverse channel that lets remote programs reach a
// proxy running on the launcher machine. It is always bound to the SSH
// server's loopback and uses a server-assigned port.
type LocalProxyChannel struct {
	tunnel     *Tunnel
	ProxyAddr  string
	ProxyURL   string
	NoProxy    string
	Env        []config.EnvPair
	BridgePath string
	BridgeErr  error
}

// StartLocalProxyChannel requests the server-side listener and publishes the
// ChemSSH bridge file. A bridge-file failure is reported through BridgeErr
// because remote commands and launcher terminals still get the env directly.
func StartLocalProxyChannel(ctx context.Context, client *ssh.Client, profile config.Profile) (*LocalProxyChannel, error) {
	return startProxyChannel(ctx, client, profile, true)
}

// StartTerminalProxyChannel gives one launcher terminal its own reverse
// listener without touching the shared ChemSSH bridge file.
func StartTerminalProxyChannel(ctx context.Context, client *ssh.Client, profile config.Profile) (*LocalProxyChannel, error) {
	return startProxyChannel(ctx, client, profile, false)
}

func startProxyChannel(ctx context.Context, client *ssh.Client, profile config.Profile, publishBridge bool) (*LocalProxyChannel, error) {
	if !profile.ProxyActive() {
		return nil, nil
	}
	proxy := profile.LocalProxy.Normalized()
	tunnel, addr, err := StartReverseTunnel(ctx, client, proxy.Address())
	if err != nil {
		return nil, fmt.Errorf("reverse channel to local proxy %s: %w", proxy.Address(), err)
	}
	proxyURL := "http://" + addr
	channel := &LocalProxyChannel{
		tunnel:    tunnel,
		ProxyAddr: addr,
		ProxyURL:  proxyURL,
		NoProxy:   proxy.NoProxy,
		Env:       config.ProxyEnvPairs(proxyURL, proxy.NoProxy),
	}
	if publishBridge {
		channel.BridgePath, channel.BridgeErr = WriteProxyBridge(client, profile.RemotePort, proxyURL, proxy.NoProxy)
	}
	return channel, nil
}

func (c *LocalProxyChannel) Close() {
	if c == nil || c.tunnel == nil {
		return
	}
	_ = c.tunnel.Close()
}
