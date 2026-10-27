package app

import (
	"context"
	"errors"
	"time"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/health"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/sshclient"
)

type Session struct {
	Profile config.Profile
	App     *App
}

func (a *App) Start(ctx context.Context, profile config.Profile) error {
	if err := netcheck.CheckPortAvailable(profile.LocalHost, profile.LocalPort); err != nil {
		return err
	}

	client, err := sshclient.Dial(profile, a.Secrets)
	if err != nil {
		return err
	}
	defer client.Close()

	var proxyEnv []config.EnvPair
	proxyChannel, err := sshclient.StartLocalProxyChannel(ctx, client, profile)
	if err != nil {
		a.warn("local network channel unavailable; continuing without it: " + err.Error())
	} else if proxyChannel != nil {
		defer func() {
			proxyChannel.Close()
			if err := sshclient.RemoveProxyBridge(client, proxyChannel.BridgePath); err != nil {
				a.warn("could not remove the ChemSSH proxy bridge file: " + err.Error())
			}
		}()
		proxyEnv = proxyChannel.Env
		if proxyChannel.BridgeErr != nil {
			a.warn("ChemSSH terminal proxy bridge not written; only the remote command and launcher terminals use the proxy: " + proxyChannel.BridgeErr.Error())
		}
		if a.Stdout != nil {
			_, _ = a.Stdout.Write([]byte("local network channel: " + proxyChannel.ProxyURL + " (remote loopback)\n"))
		}
	}

	check, err := sshclient.RunCheckPortCommand(client, profile, proxyEnv, a.Stdout, a.Stderr)
	if err != nil {
		return err
	}

	var process *sshclient.RemoteProcess
	if !check.Reusable {
		process, err = sshclient.StartRemoteCommand(client, profile, proxyEnv, a.Stdout, a.Stderr)
		if err != nil {
			return err
		}
	}

	tunnel, err := sshclient.StartTunnel(ctx, client, profile)
	if err != nil {
		if process != nil {
			_ = process.Stop()
		}
		return err
	}
	defer tunnel.Close()

	if err := health.WaitForProfileHealth(ctx, profile, a.Secrets, 90*time.Second, time.Second); err != nil {
		if process != nil {
			_ = process.Stop()
		}
		return err
	}
	if a.Stdout != nil {
		_, _ = a.Stdout.Write([]byte("health check OK: " + profile.HealthURL() + "\n"))
	}
	if profile.OpenBrowser {
		if err := browser.Open(profile.BrowserURL()); err != nil {
			return err
		}
	}

	var processDone <-chan error
	if process != nil {
		processDone = process.Done()
	}
	select {
	case <-ctx.Done():
		if process != nil {
			_ = process.Stop()
		}
		return ctx.Err()
	case err := <-processDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if a.Stdout != nil {
			_, _ = a.Stdout.Write([]byte("remote command exited; keeping tunnel open until stopped\n"))
		}
		<-ctx.Done()
		return ctx.Err()
	}
}
