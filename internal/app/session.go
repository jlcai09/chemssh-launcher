package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/secret"
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

	if err := a.waitForProfileHealth(ctx, profile, 90*time.Second, time.Second); err != nil {
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

func (a *App) waitForProfileHealth(ctx context.Context, profile config.Profile, timeout, interval time.Duration) error {
	options := netcheck.HealthOptions{}
	if profile.HasSecurityToken {
		options.RejectStatuses = map[int]string{
			http.StatusUnauthorized: "ChemSSH rejected the configured security token",
			http.StatusForbidden:    "ChemSSH rejected the configured security token",
		}
	}
	return netcheck.WaitForURLWithOptions(ctx, a.healthURLWithToken(profile), timeout, interval, options)
}

func (a *App) healthURLWithToken(profile config.Profile) string {
	baseURL := profile.HealthURL()
	if a.Secrets == nil {
		return baseURL
	}
	token, ok, err := a.Secrets.Get(profile.ID, secret.KeySecurityToken)
	if err != nil || !ok || token == "" {
		return baseURL
	}
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	return baseURL + separator + "token=" + url.QueryEscape(token)
}
