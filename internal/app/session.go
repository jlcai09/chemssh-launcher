package app

import (
	"context"
	"time"

	"chemweb-launcher/internal/browser"
	"chemweb-launcher/internal/config"
	"chemweb-launcher/internal/netcheck"
	"chemweb-launcher/internal/sshclient"
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
	if err := sshclient.CheckRemotePortAvailable(client, profile); err != nil {
		_ = sshclient.StopRemoteCommand(client, profile)
		time.Sleep(500 * time.Millisecond)
		if retryErr := sshclient.CheckRemotePortAvailable(client, profile); retryErr != nil {
			return err
		}
	}

	process, err := sshclient.StartRemoteCommand(client, profile, a.Stdout, a.Stderr)
	if err != nil {
		return err
	}
	defer process.Stop()

	tunnel, err := sshclient.StartTunnel(ctx, client, profile)
	if err != nil {
		return err
	}
	defer tunnel.Close()

	if err := netcheck.WaitForURL(ctx, profile.HealthURL(), 90*time.Second, time.Second); err != nil {
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

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-process.Done():
		return err
	}
}
