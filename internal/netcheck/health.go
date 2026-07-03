package netcheck

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type HealthOptions struct {
	RejectStatuses map[int]string
}

func WaitForURL(ctx context.Context, url string, timeout, interval time.Duration) error {
	return WaitForURLWithOptions(ctx, url, timeout, interval, HealthOptions{})
}

func WaitForURLWithOptions(ctx context.Context, url string, timeout, interval time.Duration, options HealthOptions) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(deadline, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if message, rejected := options.RejectStatuses[resp.StatusCode]; rejected {
				if message == "" {
					message = http.StatusText(resp.StatusCode)
				}
				return fmt.Errorf("health check failed for %s: unexpected HTTP status %d: %s", url, resp.StatusCode, message)
			} else if resp.StatusCode >= 200 && resp.StatusCode <= 499 {
				return nil
			} else {
				lastErr = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
			}
		} else {
			lastErr = err
		}

		select {
		case <-deadline.Done():
			if lastErr != nil {
				return fmt.Errorf("health check failed for %s: %w", url, lastErr)
			}
			return deadline.Err()
		case <-ticker.C:
		}
	}
}
