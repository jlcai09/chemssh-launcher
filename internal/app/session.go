package app

import (
	"context"
	"errors"
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

	check, err := sshclient.RunCheckPortCommand(client, profile, a.Stdout, a.Stderr)
	if err != nil {
		return err
	}

	var process *sshclient.RemoteProcess
	if !check.Reusable {
		process, err = sshclient.StartRemoteCommand(client, profile, a.Stdout, a.Stderr)
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

	if err := netcheck.WaitForURL(ctx, profile.HealthURL(), 90*time.Second, time.Second); err != nil {
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
