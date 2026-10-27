package health

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/secret"
)

func URLWithToken(profile config.Profile, secrets secret.Store) string {
	baseURL := profile.HealthURL()
	if secrets == nil {
		return baseURL
	}
	token, ok, err := secrets.Get(profile.ID, secret.KeySecurityToken)
	if err != nil || !ok || token == "" {
		return baseURL
	}
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	return baseURL + separator + "token=" + url.QueryEscape(token)
}

func WaitForProfileHealth(ctx context.Context, profile config.Profile, secrets secret.Store, timeout, interval time.Duration) error {
	options := netcheck.HealthOptions{}
	if profile.HasSecurityToken {
		options.RejectStatuses = map[int]string{
			http.StatusUnauthorized: "ChemSSH rejected the configured security token",
			http.StatusForbidden:    "ChemSSH rejected the configured security token",
		}
	}
	return netcheck.WaitForURLWithOptions(ctx, URLWithToken(profile, secrets), timeout, interval, options)
}
