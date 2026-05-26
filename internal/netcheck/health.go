package netcheck

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func WaitForURL(ctx context.Context, url string, timeout, interval time.Duration) error {
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
			if resp.StatusCode >= 200 && resp.StatusCode <= 499 {
				return nil
			}
			lastErr = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
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
